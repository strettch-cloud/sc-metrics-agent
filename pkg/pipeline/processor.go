package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/strettch-cloud/sc-metrics-agent/pkg/aggregate"
	"github.com/strettch-cloud/sc-metrics-agent/pkg/clients/metadata"
	"github.com/strettch-cloud/sc-metrics-agent/pkg/clients/tsclient"
	"github.com/strettch-cloud/sc-metrics-agent/pkg/collector"
	"github.com/strettch-cloud/sc-metrics-agent/pkg/config"
	"github.com/strettch-cloud/sc-metrics-agent/pkg/decorator"
	"go.uber.org/zap"
)

// A variable only so tests can shorten it.
var heartbeatInterval = 10 * time.Minute

// Processor implements the Collect -> Decorate -> Aggregate -> Write pipeline
type Processor struct {
	collector       collector.Collector
	decorator       decorator.MetricDecorator
	aggregator      aggregate.Aggregator
	writer          tsclient.MetricWriter
	authMgr         *metadata.AuthManager // External auth handling
	cfg             *config.Config
	heartbeatTicker *time.Ticker
	ctx             context.Context    // Lifecycle context
	cancel          context.CancelFunc // Lifecycle cancel
	logger          *zap.Logger
}

// NewProcessor creates a new pipeline processor
func NewProcessor(
	collector collector.Collector,
	decorator decorator.MetricDecorator,
	aggregator aggregate.Aggregator,
	writer tsclient.MetricWriter,
	authMgr *metadata.AuthManager,
	cfg *config.Config,
	logger *zap.Logger,
) *Processor {
	ctx, cancel := context.WithCancel(context.Background())

	p := &Processor{
		collector:       collector,
		decorator:       decorator,
		aggregator:      aggregator,
		writer:          writer,
		authMgr:         authMgr,
		cfg:             cfg,
		logger:          logger,
		heartbeatTicker: time.NewTicker(heartbeatInterval),
		ctx:             ctx,
		cancel:          cancel,
	}

	// Initialize auth and start refresh loop
	if err := authMgr.EnsureValidToken(ctx); err != nil {
		logger.Error("Failed to get initial auth token", zap.Error(err))
	}
	authMgr.StartRefresh(ctx)
	logger.Info("Auth refresh loop started")

	// Send initial heartbeat asynchronously
	go p.sendHeartbeat()

	// Start heartbeat worker
	go p.heartbeatWorker()

	return p
}

// Process executes the complete pipeline: Collect -> Decorate -> Aggregate -> Write
func (p *Processor) Process(ctx context.Context) error {
	// Refresh auth token; failures are non-fatal since writes are buffered
	// and will resume once auth recovers.
	authToken, authErr := p.getAuthTokenWithRetry(ctx)
	if authErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		p.logger.Warn("Auth token unavailable; batch will be buffered for later delivery",
			zap.Error(authErr))
	}

	startTime := time.Now()
	p.logger.Debug("Starting metrics processing pipeline")

	// Check context before starting
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Step 1: Collect metrics
	p.logger.Debug("Step 1: Collecting metrics")
	metricFamilies, err := p.collector.Collect(ctx)
	if err != nil {
		return fmt.Errorf("failed to collect metrics: %w", err)
	}

	if len(metricFamilies) == 0 {
		p.logger.Info("No metrics collected, skipping pipeline")
		return nil
	}

	p.logger.Debug("Metrics collected successfully", zap.Int("metric_families", len(metricFamilies)))

	// Check context after collection
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Step 2: Decorate metrics
	p.logger.Debug("Step 2: Decorating metrics")
	decoratedFamilies, err := p.decorator.Decorate(metricFamilies)
	if err != nil {
		return fmt.Errorf("failed to decorate metrics: %w", err)
	}

	p.logger.Debug("Metrics decorated successfully", zap.Int("decorated_families", len(decoratedFamilies)))

	// Check context after decoration
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Step 3: Aggregate metrics
	p.logger.Debug("Step 3: Aggregating metrics")
	aggregatedMetrics, err := p.aggregator.Aggregate(decoratedFamilies)
	if err != nil {
		return fmt.Errorf("failed to aggregate metrics: %w", err)
	}

	if len(aggregatedMetrics) == 0 {
		p.logger.Warn("No metrics after aggregation")
		return nil
	}

	p.logger.Debug("Metrics aggregated successfully", zap.Int("aggregated_metrics", len(aggregatedMetrics)))

	// Sort metrics for consistent ordering
	aggregate.SortMetrics(aggregatedMetrics)

	// Check context after aggregation
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	// Step 4: Write metrics
	p.logger.Debug("Step 4: Writing metrics")
	if err := p.writer.WriteMetrics(ctx, aggregatedMetrics, authToken); err != nil {
		return fmt.Errorf("failed to write metrics: %w", err)
	}

	processingTime := time.Since(startTime)

	p.logger.Info("Pipeline processing completed successfully",
		zap.Int("collected_families", len(metricFamilies)),
		zap.Int("decorated_families", len(decoratedFamilies)),
		zap.Int("aggregated_metrics", len(aggregatedMetrics)),
		zap.Duration("processing_time", processingTime))

	return nil
}

// getAuthTokenWithRetry attempts to get a valid auth token with retry and exponential backoff.
func (p *Processor) getAuthTokenWithRetry(ctx context.Context) (string, error) {
	const (
		maxRetries    = 3
		initialDelay  = 500 * time.Millisecond
		backoffFactor = 2
	)

	token := p.authMgr.GetCurrentToken()
	if token != "" {
		return token, nil
	}

	p.logger.Warn("Auth token is empty, attempting to refresh")
	delay := initialDelay

	for attempt := 1; attempt <= maxRetries; attempt++ {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		p.logger.Debug("Attempting token refresh",
			zap.Int("attempt", attempt),
			zap.Int("max_retries", maxRetries))

		if err := p.authMgr.EnsureValidToken(ctx); err != nil {
			p.logger.Warn("Token refresh attempt failed",
				zap.Int("attempt", attempt),
				zap.Error(err))
		}

		token = p.authMgr.GetCurrentToken()
		if token != "" {
			p.logger.Info("Auth token obtained after retry",
				zap.Int("attempt", attempt))
			return token, nil
		}

		if attempt < maxRetries {
			p.logger.Debug("Waiting before next retry",
				zap.Duration("delay", delay))

			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(delay):
			}

			delay *= backoffFactor
		}
	}

	return "", fmt.Errorf("auth token is empty after %d retries - refresh may have failed", maxRetries)
}

// Close performs cleanup for the processor
func (p *Processor) Close() error {
	p.logger.Debug("Closing pipeline processor")

	// Cancel context to stop workers
	p.cancel()

	// Stop heartbeat ticker
	if p.heartbeatTicker != nil {
		p.heartbeatTicker.Stop()
	}

	// Close auth manager
	p.authMgr.Close()

	// Close collector
	if closer, ok := p.collector.(interface{ Close() error }); ok {
		if err := closer.Close(); err != nil {
			p.logger.Warn("Failed to close collector", zap.Error(err))
		}
	}
	
	// Close writer
	if err := p.writer.Close(); err != nil {
		p.logger.Warn("Failed to close writer", zap.Error(err))
	}
	
	p.logger.Debug("Pipeline processor closed")
	return nil
}

// heartbeatWorker sends periodic heartbeats
func (p *Processor) heartbeatWorker() {
	for {
		select {
		case <-p.heartbeatTicker.C:
			p.sendHeartbeat()
		case <-p.ctx.Done():
			return
		}
	}
}

// sendHeartbeat sends a heartbeat to the resource manager
func (p *Processor) sendHeartbeat() {
	// Use processor context to ensure cancellation on shutdown
	ctx, cancel := context.WithTimeout(p.ctx, 10*time.Second)
	defer cancel()

	// Get auth token
	token := p.authMgr.GetCurrentToken()
	if token == "" {
		p.logger.Error("Failed to get auth token for heartbeat")
		return
	}

	if err := p.writer.SendHeartbeat(ctx, token, p.cfg.AgentVersion); err != nil {
		p.logger.Error("Failed to send heartbeat", zap.Error(err))
		return
	}

	p.logger.Info("Heartbeat sent successfully")
}
