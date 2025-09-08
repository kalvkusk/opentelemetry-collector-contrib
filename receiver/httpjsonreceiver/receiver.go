package httpjsonreceiver

import (
	"context"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"
	"go.uber.org/zap"
)

// Receiver implements the OpenTelemetry metrics receiver
type Receiver struct {
	cfg      *Config
	consumer consumer.Metrics
	logger   *zap.Logger
	settings receiver.CreateSettings

	client  *http.Client
	scraper *Scraper

	wg     sync.WaitGroup
	stopCh chan struct{}
	ticker *time.Ticker
}

// NewReceiver creates a new HTTP JSON receiver
func NewReceiver(
	cfg *Config,
	consumer consumer.Metrics,
	settings receiver.CreateSettings,
) (*Receiver, error) {

	return &Receiver{
		cfg:      cfg,
		consumer: consumer,
		logger:   settings.Logger,
		settings: settings,
		stopCh:   make(chan struct{}),
	}, nil
}

// Start starts the receiver
func (r *Receiver) Start(ctx context.Context, host component.Host) error {
	r.logger.Info("Starting HTTP JSON receiver",
		zap.Int("endpoints", len(r.cfg.Endpoints)),
		zap.Duration("interval", r.cfg.CollectionInterval))

	// Create HTTP client
	httpClient, err := r.cfg.HTTPClientSettings.ToClient(host, r.settings.TelemetrySettings)
	if err != nil {
		return err
	}
	r.client = httpClient

	// Create scraper
	r.scraper = NewScraper(r.cfg, r.client, r.logger)

	// Start collection loop
	r.wg.Add(1)
	go r.collectLoop(ctx)

	return nil
}

// Shutdown stops the receiver
func (r *Receiver) Shutdown(ctx context.Context) error {
	r.logger.Info("Shutting down HTTP JSON receiver")

	if r.ticker != nil {
		r.ticker.Stop()
	}

	close(r.stopCh)
	r.wg.Wait()

	return nil
}

// collectLoop runs the periodic collection
func (r *Receiver) collectLoop(ctx context.Context) {
	defer r.wg.Done()

	// Initial delay
	if r.cfg.InitialDelay > 0 {
		select {
		case <-time.After(r.cfg.InitialDelay):
		case <-r.stopCh:
			return
		case <-ctx.Done():
			return
		}
	}

	// Create ticker for periodic collection
	r.ticker = time.NewTicker(r.cfg.CollectionInterval)
	defer r.ticker.Stop()

	// Collect immediately, then on ticker
	r.collect(ctx)

	for {
		select {
		case <-r.ticker.C:
			r.collect(ctx)
		case <-r.stopCh:
			return
		case <-ctx.Done():
			return
		}
	}
}

// collect performs a single collection cycle
func (r *Receiver) collect(ctx context.Context) {
	r.logger.Debug("Starting metric collection")

	metrics, err := r.scraper.Scrape(ctx)
	if err != nil {
		r.logger.Error("Failed to scrape metrics", zap.Error(err))
		return
	}

	if metrics.MetricCount() == 0 {
		r.logger.Debug("No metrics collected")
		return
	}

	// Send metrics to consumer
	if err := r.consumer.ConsumeMetrics(ctx, metrics); err != nil {
		r.logger.Error("Failed to consume metrics", zap.Error(err))
	} else {
		r.logger.Debug("Successfully sent metrics",
			zap.Int("count", metrics.MetricCount()))
	}
}
