package httpjsonreceiver // import "httpjsonreceiver"

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/zap"
)

type Scraper struct {
	cfg    *Config
	client *http.Client
	logger *zap.Logger
}

func NewScraper(cfg *Config, client *http.Client, logger *zap.Logger) *Scraper {
	return &Scraper{
		cfg:    cfg,
		client: client,
		logger: logger,
	}
}

func (s *Scraper) Scrape(ctx context.Context) (pmetric.Metrics, error) {
	metrics := pmetric.NewMetrics()
	rm := metrics.ResourceMetrics().AppendEmpty()

	resource := rm.Resource()
	resource.Attributes().PutStr("receiver", "httpjson")

	for key, value := range s.cfg.ResourceAttributes {
		resource.Attributes().PutStr(key, value)
	}

	sm := rm.ScopeMetrics().AppendEmpty()
	sm.Scope().SetName("httpjsonreceiver")
	sm.Scope().SetVersion("1.0.0")

	for _, endpoint := range s.cfg.Endpoints {
		if err := s.scrapeEndpoint(ctx, endpoint, sm); err != nil {
			s.logger.Error("Failed to scrape endpoint",
				zap.String("url", endpoint.URL),
				zap.Error(err))
		}
	}

	return metrics, nil
}

func (s *Scraper) scrapeEndpoint(ctx context.Context, endpoint EndpointConfig, sm pmetric.ScopeMetrics) error {
	start := time.Now()

	// Create a client for this endpoint if it has custom TLS settings
	client := s.client
	if endpoint.SkipTLSVerify != nil {
		skipTLS := *endpoint.SkipTLSVerify
		if skipTLS != s.cfg.SkipTLSVerify {
			// Create endpoint-specific client with different TLS settings
			transport := &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: skipTLS,
				},
			}
			timeout := s.cfg.Timeout
			if endpoint.Timeout > 0 {
				timeout = endpoint.Timeout
			}
			client = &http.Client{
				Timeout:   timeout,
				Transport: transport,
			}

			s.logger.Debug("Using endpoint-specific TLS settings",
				zap.String("url", endpoint.URL),
				zap.Bool("skip_tls_verify", skipTLS))
		}
	}

	// Time templates are rendered per scrape, not once at startup: an API that
	// filters on an absolute time range (Cloudflare's GraphQL analytics is the
	// motivating case) needs a fresh window on every request, and a static
	// config string cannot express one.
	renderedURL, err := renderTimeTemplates(endpoint.URL, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("failed to render url template: %w", err)
	}
	renderedBody, err := renderTimeTemplates(endpoint.Body, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("failed to render body template: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, endpoint.Method, renderedURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	for key, value := range endpoint.Headers {
		req.Header.Set(key, value)
	}

	if renderedBody != "" && (endpoint.Method == "POST" || endpoint.Method == "PUT") {
		// Set ContentLength and GetBody alongside Body. Assigning Body alone
		// leaves ContentLength at 0, which net/http reads as "unknown" and sends
		// chunked; some APIs reject a chunked request body outright, and a
		// redirect cannot replay it.
		body := renderedBody
		req.Body = io.NopCloser(strings.NewReader(body))
		req.ContentLength = int64(len(body))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(body)), nil
		}
		if req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/json")
		}
	}

	if endpoint.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, endpoint.Timeout)
		defer cancel()
		req = req.WithContext(ctx)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	duration := time.Since(start)
	s.logger.Debug("HTTP request completed",
		zap.String("url", endpoint.URL),
		zap.Int("status", resp.StatusCode),
		zap.Duration("duration", duration))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP request failed with status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	return s.parseAndEmitMetrics(body, endpoint, sm)
}

func (s *Scraper) parseAndEmitMetrics(jsonData []byte, endpoint EndpointConfig, sm pmetric.ScopeMetrics) error {
	if !gjson.ValidBytes(jsonData) {
		return fmt.Errorf("invalid JSON response")
	}

	for _, metricCfg := range endpoint.Metrics {
		if err := s.extractAndEmitMetric(jsonData, metricCfg, endpoint, sm); err != nil {
			s.logger.Warn("Failed to extract metric",
				zap.String("metric", metricCfg.Name),
				zap.String("path", metricCfg.JSONPath),
				zap.String("url", endpoint.URL),
				zap.Error(err))
		}
	}

	return nil
}

func (s *Scraper) extractAndEmitMetric(jsonData []byte, metricCfg MetricConfig, endpoint EndpointConfig, sm pmetric.ScopeMetrics) error {
	result := gjson.GetBytes(jsonData, metricCfg.JSONPath)
	if !result.Exists() {
		return fmt.Errorf("JSONPath %q not found", metricCfg.JSONPath)
	}

	baseAttrs := pcommon.NewMap()
	baseAttrs.PutStr("http.url", endpoint.URL)
	baseAttrs.PutStr("http.method", endpoint.Method)
	if endpoint.Name != "" {
		baseAttrs.PutStr("endpoint.name", endpoint.Name)
	}
	baseAttrs.PutStr("json.path", metricCfg.JSONPath)
	for k, v := range metricCfg.Attributes {
		baseAttrs.PutStr(k, v)
	}

	// Both paths build the full list of points before touching sm.Metrics(), so
	// a metric that yields nothing usable is never appended at all. An empty
	// metric would otherwise read downstream as a real zero, which for something
	// like a staleness or error-rate alert is worse than an absent series.
	points, err := s.collectPoints(result, metricCfg, baseAttrs)
	if err != nil {
		return err
	}
	if len(points) == 0 {
		return fmt.Errorf("no usable values at JSONPath %q", metricCfg.JSONPath)
	}

	metric := sm.Metrics().AppendEmpty()
	metric.SetName(metricCfg.Name)
	if metricCfg.Description != "" {
		metric.SetDescription(metricCfg.Description)
	}
	if metricCfg.Unit != "" {
		metric.SetUnit(metricCfg.Unit)
	}

	now := pcommon.NewTimestampFromTime(time.Now())

	switch metricCfg.Type {
	case "counter":
		sum := metric.SetEmptySum()
		sum.SetIsMonotonic(true)
		sum.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
		fillNumberPoints(sum.DataPoints(), points, metricCfg, now)

	case "histogram":
		histogram := metric.SetEmptyHistogram()
		histogram.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
		for _, pt := range points {
			dp := histogram.DataPoints().AppendEmpty()
			dp.SetTimestamp(now)
			dp.SetCount(1)
			dp.SetSum(pt.floatValue)
			dp.BucketCounts().Append(1)
			dp.ExplicitBounds().Append(pt.floatValue)
			pt.attrs.CopyTo(dp.Attributes())
		}

	default: // gauge
		fillNumberPoints(metric.SetEmptyGauge().DataPoints(), points, metricCfg, now)
	}

	s.logger.Debug("Extracted metric",
		zap.String("name", metricCfg.Name),
		zap.Int("points", len(points)),
		zap.String("type", metricCfg.Type))

	return nil
}

// dataPoint is one extracted value plus the attributes that belong to it.
type dataPoint struct {
	intValue   int64
	floatValue float64
	attrs      pcommon.Map
}

func fillNumberPoints(dps pmetric.NumberDataPointSlice, points []dataPoint, metricCfg MetricConfig, now pcommon.Timestamp) {
	for _, pt := range points {
		dp := dps.AppendEmpty()
		dp.SetTimestamp(now)
		if metricCfg.ValueType == "int" {
			dp.SetIntValue(pt.intValue)
		} else {
			dp.SetDoubleValue(pt.floatValue)
		}
		pt.attrs.CopyTo(dp.Attributes())
	}
}

// collectPoints turns the gjson result into data points: exactly one for an
// ordinary metric, or one per array element when `each` is configured.
func (s *Scraper) collectPoints(result gjson.Result, metricCfg MetricConfig, baseAttrs pcommon.Map) ([]dataPoint, error) {
	if metricCfg.Each == nil {
		intValue, floatValue, err := toNumeric(result, metricCfg)
		if err != nil {
			return nil, err
		}
		attrs := pcommon.NewMap()
		baseAttrs.CopyTo(attrs)
		return []dataPoint{{intValue: intValue, floatValue: floatValue, attrs: attrs}}, nil
	}

	if !result.IsArray() {
		return nil, fmt.Errorf("each is configured but JSONPath %q is %s, not an array", metricCfg.JSONPath, result.Type)
	}

	elements := result.Array()
	if metricCfg.Each.MaxPoints > 0 && len(elements) > metricCfg.Each.MaxPoints {
		s.logger.Warn("Truncating each fan-out",
			zap.String("metric", metricCfg.Name),
			zap.Int("elements", len(elements)),
			zap.Int("max_points", metricCfg.Each.MaxPoints))
		elements = elements[:metricCfg.Each.MaxPoints]
	}

	points := make([]dataPoint, 0, len(elements))
	for i, element := range elements {
		value := element
		if metricCfg.Each.Value != "" {
			value = element.Get(metricCfg.Each.Value)
			if !value.Exists() {
				s.logger.Warn("Skipping element with no value at each.value",
					zap.String("metric", metricCfg.Name),
					zap.String("each.value", metricCfg.Each.Value),
					zap.Int("index", i))
				continue
			}
		}

		intValue, floatValue, err := toNumeric(value, metricCfg)
		if err != nil {
			s.logger.Warn("Skipping element with unusable value",
				zap.String("metric", metricCfg.Name),
				zap.Int("index", i),
				zap.Error(err))
			continue
		}

		attrs := pcommon.NewMap()
		baseAttrs.CopyTo(attrs)
		for name, path := range metricCfg.Each.Attributes {
			// Missing attributes become "" rather than dropping the point: an
			// absent dimension is still a real observation, and silently losing
			// it would understate a total.
			attrs.PutStr(name, element.Get(path).String())
		}
		points = append(points, dataPoint{intValue: intValue, floatValue: floatValue, attrs: attrs})
	}

	return points, nil
}

// toNumeric converts one gjson result to its int and float representations,
// applying convert_to_age_seconds / convert_to_decimal if configured.
func toNumeric(result gjson.Result, metricCfg MetricConfig) (int64, float64, error) {
	rawStr := result.String()

	// If convert_to_age_seconds is enabled, the extracted value is a point in
	// time and the metric is how long ago it was. Checked before
	// convert_to_decimal only for readability; Validate() rejects both at once.
	if metricCfg.ConvertToAgeSeconds {
		ts, err := parseTimestamp(result)
		if err != nil {
			return 0, 0, fmt.Errorf("failed to parse %q as a timestamp: %w", rawStr, err)
		}
		age := time.Since(ts).Seconds()
		return int64(age), age, nil
	}

	// If convert_to_decimal is enabled, treat the value as a hex string.
	if metricCfg.ConvertToDecimal {
		hexStr := strings.TrimPrefix(strings.TrimPrefix(rawStr, "0x"), "0X")
		parsed, err := strconv.ParseInt(hexStr, 16, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("failed to parse %q as hex: %w", rawStr, err)
		}
		return parsed, float64(parsed), nil
	}

	switch result.Type {
	case gjson.Number:
		return result.Int(), result.Float(), nil
	case gjson.String:
		if metricCfg.ValueType == "int" {
			intValue, err := strconv.ParseInt(rawStr, 10, 64)
			if err != nil {
				return 0, 0, fmt.Errorf("failed to parse value: %w", err)
			}
			return intValue, float64(intValue), nil
		}
		floatValue, err := strconv.ParseFloat(rawStr, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("failed to parse value: %w", err)
		}
		return int64(floatValue), floatValue, nil
	case gjson.True:
		return 1, 1.0, nil
	case gjson.False:
		return 0, 0.0, nil
	default:
		return 0, 0, fmt.Errorf("cannot convert %s to numeric value", result.Type)
	}
}

// parseTimestamp interprets a gjson result as a point in time, for
// convert_to_age_seconds. It accepts RFC3339 (with or without fractional
// seconds) and Unix epoch seconds, given either as a JSON number or as a
// string -- between them those cover the "last updated" fields we poll.
func parseTimestamp(result gjson.Result) (time.Time, error) {
	if result.Type == gjson.Number {
		return unixFloatToTime(result.Float()), nil
	}

	raw := result.String()
	// RFC3339Nano first: its layout treats the fractional part as optional, so
	// it also parses plain RFC3339. RFC3339 is tried second for the rare input
	// that carries an offset shape RFC3339Nano rejects.
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if ts, err := time.Parse(layout, raw); err == nil {
			return ts, nil
		}
	}

	// A JSON string may still hold an epoch, e.g. "1757260458".
	if epoch, err := strconv.ParseFloat(raw, 64); err == nil {
		return unixFloatToTime(epoch), nil
	}

	return time.Time{}, fmt.Errorf("not RFC3339 or a Unix epoch: %q", raw)
}

func unixFloatToTime(epoch float64) time.Time {
	whole, frac := math.Modf(epoch)
	return time.Unix(int64(whole), int64(frac*float64(time.Second)))
}
