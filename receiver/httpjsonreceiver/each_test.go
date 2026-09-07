package httpjsonreceiver

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// cloudflareResponse mirrors the shape of a real Cloudflare GraphQL analytics
// reply: an array of groups, each with a count and a dimensions object. The
// label values only exist in the response, which is what `each` is for.
const cloudflareResponse = `{
  "data": { "viewer": { "zones": [ { "httpRequestsAdaptiveGroups": [
    {"count": 41231, "sum": {"edgeResponseBytes": 918273645},
     "dimensions": {"clientRequestHTTPHost": "products.chain.grpc-web.injective.network", "edgeResponseStatus": 200}},
    {"count": 1288, "sum": {"edgeResponseBytes": 42111},
     "dimensions": {"clientRequestHTTPHost": "products.chain.grpc-web.injective.network", "edgeResponseStatus": 503}},
    {"count": 9002, "sum": {"edgeResponseBytes": 71222},
     "dimensions": {"clientRequestHTTPHost": "products.lcd.injective.network", "edgeResponseStatus": 200}},
    {"count": "not-a-number",
     "dimensions": {"clientRequestHTTPHost": "broken.injective.network", "edgeResponseStatus": 200}}
  ] } ] } },
  "errors": null
}`

func TestEachFanOutCloudflareShape(t *testing.T) {
	var gotBody string
	var gotContentLength int64
	var gotTransferEncoding []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotContentLength = r.ContentLength
		gotTransferEncoding = r.TransferEncoding
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, cloudflareResponse)
	}))
	defer server.Close()

	cfg := &Config{
		Endpoints: []EndpointConfig{{
			Name:   "cloudflare zone requests",
			URL:    server.URL,
			Method: "POST",
			Headers: map[string]string{
				"Authorization": "Bearer test-token",
			},
			Body: `{"variables":{"mintime":"{{now-6m}}","maxtime":"{{now-5m}}"}}`,
			Metrics: []MetricConfig{{
				Name:      "cloudflare.zone.requests",
				JSONPath:  "data.viewer.zones.0.httpRequestsAdaptiveGroups",
				Type:      "counter",
				ValueType: "int",
				Attributes: map[string]string{
					"zone": "injective.network",
				},
				Each: &EachConfig{
					Value: "count",
					Attributes: map[string]string{
						"host":   "dimensions.clientRequestHTTPHost",
						"status": "dimensions.edgeResponseStatus",
					},
				},
			}},
		}},
	}
	require.NoError(t, cfg.Validate())
	require.Equal(t, defaultMaxPoints, cfg.Endpoints[0].Metrics[0].Each.MaxPoints, "max_points should default")

	s := NewScraper(cfg, &http.Client{Timeout: 5 * time.Second}, zaptest.NewLogger(t))
	metrics, err := s.Scrape(context.Background())
	require.NoError(t, err)

	// The body must arrive rendered, and with a real Content-Length: assigning
	// Body without ContentLength makes net/http send it chunked, which some
	// APIs reject.
	assert.NotContains(t, gotBody, "{{", "time templates must be rendered before sending")
	assert.Contains(t, gotBody, `"mintime":"20`)
	assert.Equal(t, int64(len(gotBody)), gotContentLength, "Content-Length must be set")
	assert.Empty(t, gotTransferEncoding, "body must not be sent chunked")

	sm := metrics.ResourceMetrics().At(0).ScopeMetrics().At(0)
	require.Equal(t, 1, sm.Metrics().Len())
	m := sm.Metrics().At(0)
	assert.Equal(t, "cloudflare.zone.requests", m.Name())

	dps := m.Sum().DataPoints()
	// Three good rows; the "not-a-number" row is skipped, not emitted as zero.
	require.Equal(t, 3, dps.Len())

	got := map[string]int64{}
	for i := 0; i < dps.Len(); i++ {
		dp := dps.At(i)
		host, ok := dp.Attributes().Get("host")
		require.True(t, ok)
		status, ok := dp.Attributes().Get("status")
		require.True(t, ok)
		zone, ok := dp.Attributes().Get("zone")
		require.True(t, ok, "static attributes must survive fan-out")
		assert.Equal(t, "injective.network", zone.Str())
		got[host.Str()+"|"+status.Str()] = dp.IntValue()
	}

	assert.Equal(t, int64(41231), got["products.chain.grpc-web.injective.network|200"])
	assert.Equal(t, int64(1288), got["products.chain.grpc-web.injective.network|503"])
	assert.Equal(t, int64(9002), got["products.lcd.injective.network|200"])
	assert.NotContains(t, got, "broken.injective.network|200")
}

func TestEachMaxPointsTruncates(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"rows":[`)
	for i := 0; i < 50; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"n":1,"k":"a"}`)
	}
	sb.WriteString(`]}`)
	payload := sb.String()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, payload)
	}))
	defer server.Close()

	cfg := &Config{Endpoints: []EndpointConfig{{
		URL: server.URL, Method: "GET",
		Metrics: []MetricConfig{{
			Name: "rows", JSONPath: "rows", Type: "gauge",
			Each: &EachConfig{Value: "n", Attributes: map[string]string{"k": "k"}, MaxPoints: 10},
		}},
	}}}
	require.NoError(t, cfg.Validate())

	s := NewScraper(cfg, &http.Client{Timeout: 5 * time.Second}, zaptest.NewLogger(t))
	metrics, err := s.Scrape(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 10, metrics.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().At(0).Gauge().DataPoints().Len())
}

func TestEachOnNonArrayIsAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"scalar": 7}`)
	}))
	defer server.Close()

	cfg := &Config{Endpoints: []EndpointConfig{{
		URL: server.URL, Method: "GET",
		Metrics: []MetricConfig{{
			Name: "scalar", JSONPath: "scalar", Type: "gauge",
			Each: &EachConfig{Value: "n"},
		}},
	}}}
	require.NoError(t, cfg.Validate())

	s := NewScraper(cfg, &http.Client{Timeout: 5 * time.Second}, zaptest.NewLogger(t))
	metrics, err := s.Scrape(context.Background())
	require.NoError(t, err) // endpoint errors are logged, not fatal
	assert.Equal(t, 0, metrics.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics().Len())
}
