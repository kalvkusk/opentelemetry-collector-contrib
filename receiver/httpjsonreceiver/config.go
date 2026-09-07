package httpjsonreceiver // import "httpjsonreceiver"

import (
	"errors"
	"fmt"
	"net/url"
	"time"
)

// defaultMaxPoints bounds an `each` fan-out when the config does not.
const defaultMaxPoints = 1000

type Config struct {
	CollectionInterval time.Duration     `mapstructure:"collection_interval"`
	InitialDelay       time.Duration     `mapstructure:"initial_delay"`
	Timeout            time.Duration     `mapstructure:"timeout"`
	SkipTLSVerify      bool              `mapstructure:"skip_tls_verify"`
	Endpoints          []EndpointConfig  `mapstructure:"endpoints"`
	ResourceAttributes map[string]string `mapstructure:"resource_attributes"`
}

type EndpointConfig struct {
	URL           string            `mapstructure:"url"`
	Method        string            `mapstructure:"method"`
	Headers       map[string]string `mapstructure:"headers"`
	Body          string            `mapstructure:"body,omitempty"`
	Name          string            `mapstructure:"name,omitempty"`
	Metrics       []MetricConfig    `mapstructure:"metrics"`
	Timeout       time.Duration     `mapstructure:"timeout,omitempty"`
	SkipTLSVerify *bool             `mapstructure:"skip_tls_verify,omitempty"`
}

type MetricConfig struct {
	Name        string            `mapstructure:"name"`
	JSONPath    string            `mapstructure:"json_path"`
	Type        string            `mapstructure:"type"`
	Description string            `mapstructure:"description,omitempty"`
	Unit        string            `mapstructure:"unit,omitempty"`
	Attributes  map[string]string `mapstructure:"attributes,omitempty"`
	ValueType   string            `mapstructure:"value_type,omitempty"`
	ConvertToDecimal bool `mapstructure:"convert_to_decimal,omitempty"`
	// ConvertToAgeSeconds turns a point-in-time value into the number of seconds
	// that have elapsed since it. The extracted value may be an RFC3339 string
	// (with or without fractional seconds) or a Unix epoch number. This exists
	// because a "last updated" timestamp is not directly alertable -- a static
	// threshold cannot be compared against an absolute time -- whereas an age is.
	ConvertToAgeSeconds bool `mapstructure:"convert_to_age_seconds,omitempty"`
	// Each turns one JSON array into one data point per element, instead of the
	// default single scalar. Needed for group-by style APIs -- Cloudflare's
	// GraphQL analytics returns an array of {dimensions, count} rows -- where the
	// label values are in the response, not known when the config is written.
	Each *EachConfig `mapstructure:"each,omitempty"`
}

// EachConfig fans a metric out over the elements of the array at json_path.
// Value and the Attributes values are gjson paths evaluated relative to each
// element.
type EachConfig struct {
	// Value is the path to the number within each element. Empty means the
	// element itself is the number.
	Value string `mapstructure:"value,omitempty"`
	// Attributes are extra attributes read from each element, as name -> path.
	// They are merged over the metric's static attributes.
	Attributes map[string]string `mapstructure:"attributes,omitempty"`
	// MaxPoints caps how many data points one array may produce, defaulting to
	// defaultMaxPoints. A response that grows unexpectedly is a cardinality
	// incident in the metrics backend, so the receiver truncates and warns
	// rather than forwarding everything it was handed.
	MaxPoints int `mapstructure:"max_points,omitempty"`
}

func (cfg *Config) Validate() error {
	if len(cfg.Endpoints) == 0 {
		return errors.New("at least one endpoint must be specified")
	}

	if cfg.CollectionInterval <= 0 {
		cfg.CollectionInterval = 60 * time.Second
	}

	if cfg.Timeout <= 0 {
		cfg.Timeout = 10 * time.Second
	}

	for i, endpoint := range cfg.Endpoints {
		if err := cfg.validateEndpoint(i, &endpoint); err != nil {
			return err
		}
	}

	return nil
}

// In config.go, update the validateEndpoint method:
func (cfg *Config) validateEndpoint(index int, endpoint *EndpointConfig) error {
	if endpoint.URL == "" {
		return fmt.Errorf("endpoints[%d]: url is required", index)
	}

	if _, err := url.Parse(endpoint.URL); err != nil {
		return fmt.Errorf("endpoints[%d]: invalid url: %w", index, err)
	}

	// Apply defaults
	if endpoint.Method == "" {
		cfg.Endpoints[index].Method = "GET" // Fix: assign to cfg.Endpoints[index]
	}

	if len(endpoint.Metrics) == 0 {
		return fmt.Errorf("endpoints[%d]: no metrics configured", index)
	}

	for j, metric := range endpoint.Metrics {
		if err := cfg.validateMetric(fmt.Sprintf("endpoints[%d].metrics[%d]", index, j), &metric, index, j); err != nil {
			return err
		}
	}

	return nil
}

// Update validateMetric to also apply defaults:
func (cfg *Config) validateMetric(prefix string, metric *MetricConfig, endpointIndex, metricIndex int) error {
	if metric.Name == "" {
		return fmt.Errorf("%s: name is required", prefix)
	}

	if metric.JSONPath == "" {
		return fmt.Errorf("%s: json_path is required", prefix)
	}

	if metric.ConvertToDecimal && metric.ConvertToAgeSeconds {
		return fmt.Errorf("%s: convert_to_decimal and convert_to_age_seconds are mutually exclusive", prefix)
	}

	if metric.Each != nil {
		if metric.Each.MaxPoints < 0 {
			return fmt.Errorf("%s: each.max_points must not be negative", prefix)
		}
		if metric.Each.MaxPoints == 0 {
			cfg.Endpoints[endpointIndex].Metrics[metricIndex].Each.MaxPoints = defaultMaxPoints
		}
	}

	// Apply defaults
	if metric.Type == "" {
		cfg.Endpoints[endpointIndex].Metrics[metricIndex].Type = "gauge"
	} else {
		switch metric.Type {
		case "gauge", "counter", "histogram":
			// Valid types
		default:
			return fmt.Errorf("%s: invalid type %q, must be one of: gauge, counter, histogram", prefix, metric.Type)
		}
	}

	if metric.ValueType == "" {
		cfg.Endpoints[endpointIndex].Metrics[metricIndex].ValueType = "double"
	} else {
		switch metric.ValueType {
		case "int", "double":
			// Valid types
		default:
			return fmt.Errorf("%s: invalid value_type %q, must be one of: int, double", prefix, metric.ValueType)
		}
	}

	return nil
}
