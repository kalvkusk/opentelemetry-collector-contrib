package httpjsonreceiver

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"
)

const (
	typeStr   = "httpjson"
	stability = component.StabilityLevelBeta

	// Default values
	defaultCollectionInterval = 60 * time.Second
	defaultTimeout            = 10 * time.Second
)

// NewFactory creates a factory for HTTP JSON receiver
func NewFactory() receiver.Factory {
	return receiver.NewFactory(
		typeStr,
		createDefaultConfig,
		receiver.WithMetrics(createMetricsReceiver, stability),
	)
}

func createDefaultConfig() component.Config {
	return &Config{
		CollectionInterval: defaultCollectionInterval,
		InitialDelay:       time.Second,
		HTTPClientSettings: confighttp.HTTPClientSettings{
			Timeout: defaultTimeout,
		},
		ResourceAttributes: make(map[string]string),
	}
}

func createMetricsReceiver(
	ctx context.Context,
	params receiver.CreateSettings,
	rConf component.Config,
	consumer consumer.Metrics,
) (receiver.Metrics, error) {
	cfg := rConf.(*Config)
	return NewReceiver(cfg, consumer, params)
}
