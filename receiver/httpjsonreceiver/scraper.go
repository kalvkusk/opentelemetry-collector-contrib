package httpjsonreceiver

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/receiver"
	"go.uber.org/zap"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/httpjsonreceiver/internal/metadata"
)

type httpJSONScraper struct {
	cfg      *Config
	settings receiver.CreateSettings
	logger   *zap.Logger
	mb       *metadata.MetricsBuilder
	client   *http.Client
}

func newScraper(cfg *Config, settings receiver.CreateSettings) *httpJSONScraper {
	return &httpJSONScraper{
		cfg:      cfg,
		settings: settings,
		logger:   settings.Logger,
		mb:       metadata.NewMetricsBuilder(cfg.MetricsBuilderConfig, settings),
	}
}

func (h *httpJSONScraper) start(ctx context.Context, host component.Host) error {
	httpClient, err := h.cfg.HTTPClientSettings.ToClient(host, h.settings.TelemetrySettings)
	if err != nil {
		return fmt.Errorf("failed to create HTTP client: %w", err)
	}
	h.client = httpClient
	return nil
}

func (h *httpJSONScraper) scrape(ctx context.Context) (pmetric.Metrics, error) {
	h.logger.Debug("Starting HTTP JSON scrape")

	for _, endpoint := range h.cfg.Endpoints {
		if err := h.scrapeEndpoint(ctx, endpoint); err != nil {
			h.logger.Error("Failed to scrape endpoint",
				zap.String("url", endpoint.URL),
				zap.Error(err))
			// Continue with other endpoints
		}
	}

	return h.mb.Emit(), nil
}

func (h *httpJSONScraper) scrapeEndpoint(ctx context.Context, endpoint EndpointConfig) error {
	start := time.Now()

	// Create HTTP request
	req, err := http.NewRequestWithContext(ctx, endpoint.Method, endpoint.URL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Add headers
	for key, value := range endpoint.Headers {
		req.Header.Set(key, string(value))
	}

	// Add request body for POST
	if endpoint.Body != "" && endpoint.Method == "POST" {
		req.Body = io.NopCloser(strings.NewReader(endpoint.Body))
		if req.Header.Get("Content-Type") == "" {
			req.Header.Set("Content-Type", "application/json")
		}
	}

	// Make request
	resp, err := h.client.Do(req)
	if err != nil {
		h.mb.RecordHttpjsonScrapeErrorsDataPoint(time.Now(), 1, endpoint.URL)
		return fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	// Record request duration
	duration := time.Since(start)
	h.mb.RecordHttpjsonRequestDurationDataPoint(
		time.Now(),
		float64(duration.Milliseconds()),
		endpoint.URL,
		endpoint.Method,
	)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		h.mb.RecordHttpjsonScrapeErrorsDataPoint(time.Now(), 1, endpoint.URL)
		return fmt.Errorf("HTTP request failed with status %d", resp.StatusCode)
	}

	// Read response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	// Parse JSON and extract metrics
	if err := h.parseAndEmitMetrics(body, endpoint); err != nil {
		h.mb.RecordHttpjsonScrapeErrorsDataPoint(time.Now(), 1, endpoint.URL)
		return err
	}

	return nil
}

func (h *httpJSONScraper) parseAndEmitMetrics(jsonData []byte, endpoint EndpointConfig) error {
	if !gjson.ValidBytes(jsonData) {
		return fmt.Errorf("invalid JSON response")
	}

	// Use endpoint metrics or default metrics
	metrics := endpoint.Metrics
	if len(metrics) == 0 {
		metrics = h.cfg.DefaultMetrics
	}

	for _, metricCfg := range metrics {
		if err := h.extractAndEmitMetric(jsonData, metricCfg, endpoint); err != nil {
			h.logger.Warn("Failed to extract metric",
				zap.String("metric", metricCfg.Name),
				zap.String("path", metricCfg.JSONPath),
				zap.Error(err))
			// Continue with other metrics
		}
	}

	return nil
}

func (h *httpJSONScraper) extractAndEmitMetric(jsonData []byte, metricCfg MetricConfig, endpoint EndpointConfig) error {
	// Extract value using JSONPath
	result := gjson.GetBytes(jsonData, metricCfg.JSONPath)
	if !result.Exists() {
		return fmt.Errorf("JSONPath %q not found", metricCfg.JSONPath)
	}

	// Convert based on value type
	var value interface{}
	var err error

	switch metricCfg.ValueType {
	case "int":
		switch result.Type {
		case gjson.Number:
			value = result.Int()
		case gjson.String:
			value, err = strconv.ParseInt(result.String(), 10, 64)
		case gjson.True:
			value = int64(1)
		case gjson.False:
			value = int64(0)
		default:
			return fmt.Errorf("cannot convert %s to int", result.Type)
		}
	default: // double
		switch result.Type {
		case gjson.Number:
			value = result.Float()
		case gjson.String:
			value, err = strconv.ParseFloat(result.String(), 64)
		case gjson.True:
			value = 1.0
		case gjson.False:
			value = 0.0
		default:
			return fmt.Errorf("cannot convert %s to float", result.Type)
		}
	}

	if err != nil {
		return fmt.Errorf("failed to parse value: %w", err)
	}

	// Create attributes
	attributes := make(map[string]string)

	// Add endpoint attributes
	attributes["http.url"] = endpoint.URL
	attributes["http.method"] = endpoint.Method
	if endpoint.Name != "" {
		attributes["endpoint.name"] = endpoint.Name
	}
	attributes["json.path"] = metricCfg.JSONPath

	// Add configured attributes
	for k, v := range metricCfg.Attributes {
		attributes[k] = v
	}

	// Emit metric based on type
	now := time.Now()

	switch metricCfg.Type {
	case "gauge":
		if metricCfg.ValueType == "int" {
			h.emitIntGauge(metricCfg.Name, value.(int64), attributes, now)
		} else {
			h.emitDoubleGauge(metricCfg.Name, value.(float64), attributes, now)
		}
	case "counter":
		if metricCfg.ValueType == "int" {
			h.emitIntSum(metricCfg.Name, value.(int64), attributes, now)
		} else {
			h.emitDoubleSum(metricCfg.Name, value.(float64), attributes, now)
		}
	case "histogram":
		h.emitHistogram(metricCfg.Name, value.(float64), attributes, now)
	}

	return nil
}

// Helper methods to emit different metric types
func (h *httpJSONScraper) emitDoubleGauge(name string, value float64, attributes map[string]string, ts time.Time) {
	// Implementation depends on your metrics builder
	// This is a simplified example
}

func (h *httpJSONScraper) emitIntGauge(name string, value int64, attributes map[string]string, ts time.Time) {
	// Implementation depends on your metrics builder
}

func (h *httpJSONScraper) emitDoubleSum(name string, value float64, attributes map[string]string, ts time.Time) {
	// Implementation depends on your metrics builder
}

func (h *httpJSONScraper) emitIntSum(name string, value int64, attributes map[string]string, ts time.Time) {
	// Implementation depends on your metrics builder
}

func (h *httpJSONScraper) emitHistogram(name string, value float64, attributes map[string]string, ts time.Time) {
	// Implementation depends on your metrics builder
}
