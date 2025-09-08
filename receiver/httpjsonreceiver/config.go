package httpjsonreceiver

import (
	"errors"
	"fmt"
	"net/url"
	"time"

	"go.opentelemetry.io/collector/config/confighttp"
)

// Config represents the receiver config
type Config struct {
	// Collection interval
	CollectionInterval time.Duration `mapstructure:"collection_interval"`

	// Initial delay before starting collection
	InitialDelay time.Duration `mapstructure:"initial_delay"`

	// HTTP client settings
	confighttp.HTTPClientSettings `mapstructure:",squash"`

	// Endpoints to scrape
	Endpoints []EndpointConfig `mapstructure:"endpoints"`

	// Resource attributes to add to all metrics
	ResourceAttributes map[string]string `mapstructure:"resource_attributes"`
}

// EndpointConfig defines configuration for a single endpoint
type EndpointConfig struct {
	// URL to scrape (required)
	URL string `mapstructure:"url"`

	// HTTP method (default: GET)
	Method string `mapstructure:"method"`

	// Additional headers
	Headers map[string]string `mapstructure:"headers"`

	// Request body for POST requests
	Body string `mapstructure:"body,omitempty"`

	// Name for this endpoint (used in labels)
	Name string `mapstructure:"name,omitempty"`

	// Metrics to extract from this endpoint
	Metrics []MetricConfig `mapstructure:"metrics"`

	// Per-endpoint timeout
	Timeout time.Duration `mapstructure:"timeout,omitempty"`
}

// MetricConfig defines how to extract and configure a metric
type MetricConfig struct {
	// Metric name (required)
	Name string `mapstructure:"name"`

	// JSONPath expression to extract value (required)
	JSONPath string `mapstructure:"json_path"`

	// Metric type: gauge, counter, histogram (default: gauge)
	Type string `mapstructure:"type"`

	// Metric description
	Description string `mapstructure:"description,omitempty"`

	// Metric unit
	Unit string `mapstructure:"unit,omitempty"`

	// Static attributes to add to this metric
	Attributes map[string]string `mapstructure:"attributes,omitempty"`

	// Value type: int, double (default: double)
	ValueType string `mapstructure:"value_type,omitempty"`
}

// Validate checks the receiver configuration is valid
func (cfg *Config) Validate() error {
	if len(cfg.Endpoints) == 0 {
		return errors.New("at least one endpoint must be specified")
	}

	if cfg.CollectionInterval <= 0 {
		cfg.CollectionInterval = 60 * time.Second
	}

	for i, endpoint := range cfg.Endpoints {
		if err := cfg.validateEndpoint(i, &endpoint); err != nil {
			return err
		}
	}

	return nil
}

func (cfg *Config) validateEndpoint(index int, endpoint *EndpointConfig) error {
	if endpoint.URL == "" {
		return fmt.Errorf("endpoints[%d]: url is required", index)
	}

	if _, err := url.Parse(endpoint.URL); err != nil {
		return fmt.Errorf("endpoints[%d]: invalid url: %w", index, err)
	}

	if endpoint.Method == "" {
		endpoint.Method = "GET"
	}

	if len(endpoint.Metrics) == 0 {
		return fmt.Errorf("endpoints[%d]: no metrics configured", index)
	}

	for j, metric := range endpoint.Metrics {
		if err := cfg.validateMetric(fmt.Sprintf("endpoints[%d].metrics[%d]", index, j), &metric); err != nil {
			return err
		}
	}

	return nil
}

func (cfg *Config) validateMetric(prefix string, metric *MetricConfig) error {
	if metric.Name == "" {
		return fmt.Errorf("%s: name is required", prefix)
	}

	if metric.JSONPath == "" {
		return fmt.Errorf("%s: json_path is required", prefix)
	}

	switch metric.Type {
	case "", "gauge":
		metric.Type = "gauge"
	case "counter", "histogram":
		// Valid types
	default:
		return fmt.Errorf("%s: invalid type %q, must be one of: gauge, counter, histogram", prefix, metric.Type)
	}

	switch metric.ValueType {
	case "", "double":
		metric.ValueType = "double"
	case "int":
		// Valid type
	default:
		return fmt.Errorf("%s: invalid value_type %q, must be one of: int, double", prefix, metric.ValueType)
	}

	return nil
}
