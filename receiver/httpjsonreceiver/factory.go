package httpjsonreceiver

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/confighttp"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"
	"go.opentelemetry.io/collector/receiver/scraperhelper"

	"github.com/open-telemetry/opentelemetry-collector-contrib/receiver/httpjsonreceiver/internal/metadata"
)

// This file implements factory for HTTP JSON receiver.

const (
	typeStr = "httpjson"

	// Default values
	defaultCollectionInterval = 60 * time.Second
	defaultTimeout            = 10 * time.Second
)

// NewFactory creates a factory for HTTP JSON receiver.
func NewFactory() receiver.Factory {
	return receiver.NewFactory(
		typeStr,
		createDefaultConfig,
		receiver.WithMetrics(createMetricsReceiver, metadata.MetricsStability),
	)
}

func createDefaultConfig() component.Config {
	return &Config{
		ScraperControllerSettings: scraperhelper.ScraperControllerSettings{
			CollectionInterval: defaultCollectionInterval,
			InitialDelay:       time.Second,
		},
		HTTPClientSettings: confighttp.HTTPClientSettings{
			Timeout: defaultTimeout,
		},
	}
}

func createMetricsReceiver(
	_ context.Context,
	params receiver.CreateSettings,
	rConf component.Config,
	consumer consumer.Metrics,
) (receiver.Metrics, error) {

	cfg := rConf.(*Config)

	httpJSONScraper := newScraper(cfg, params)
	scraper, err := scraperhelper.NewScraper(typeStr, httpJSONScraper.scrape)
	if err != nil {
		return nil, err
	}

	return scraperhelper.NewScraperControllerReceiver(
		&cfg.ScraperControllerSettings,
		params,
		consumer,
		scraperhelper.AddScraper(scraper),
	)
}
