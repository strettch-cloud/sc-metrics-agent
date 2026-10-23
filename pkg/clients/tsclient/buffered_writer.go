package tsclient

import (
	"context"
	"fmt"

	"github.com/strettch-cloud/sc-metrics-agent/pkg/aggregate"
	"github.com/strettch-cloud/sc-metrics-agent/pkg/buffer"
	"go.uber.org/zap"
)

// DrainBatchSize balances recovery speed and request size: 50 batches clear a
// 24-hour backlog in ~30 minutes while keeping requests ~135 KiB.
const DrainBatchSize = 50

// MetricWriter is the writer surface the pipeline consumes.
type MetricWriter interface {
	WriteMetrics(ctx context.Context, metrics []aggregate.MetricWithValue, authToken string) error
	AuxiliaryWriter
}

// compressedSender is the subset of *Client used by BufferedMetricWriter.
type compressedSender interface {
	CompressMetrics(metrics []aggregate.MetricWithValue) ([]byte, error)
	SendCompressedBatches(ctx context.Context, batches [][]byte, authToken string) (*Response, error)
	IngestEndpointAvailable() bool
}

// BufferedMetricWriter stores metric batches on disk and sends them only when
// downstream is healthy. Failed sends are retried on the next drain cycle.
type BufferedMetricWriter struct {
	sender compressedSender
	queue  *buffer.DiskQueue
	aux    AuxiliaryWriter
	logger *zap.Logger
}

// NewBufferedMetricWriter wraps a *Client with a disk-backed queue.
// The aux writer carries heartbeats unbuffered.
func NewBufferedMetricWriter(
	client *Client,
	queue *buffer.DiskQueue,
	aux AuxiliaryWriter,
	logger *zap.Logger,
) *BufferedMetricWriter {
	return &BufferedMetricWriter{
		sender: client,
		queue:  queue,
		aux:    aux,
		logger: logger,
	}
}

// WriteMetrics buffers the batch and attempts to drain queued batches.
// Only batch preparation failures are returned; send failures are retried
// from the buffer on future ticks.
func (bw *BufferedMetricWriter) WriteMetrics(ctx context.Context, metrics []aggregate.MetricWithValue, authToken string) error {
	if len(metrics) == 0 {
		bw.logger.Debug("No metrics to write")
		return bw.drain(ctx, authToken)
	}

	compressed, err := bw.sender.CompressMetrics(metrics)
	if err != nil {
		return fmt.Errorf("failed to compress metrics: %w", err)
	}

	if !bw.queue.Push(compressed) {
		// Push refused — either the cap is hit or the disk write failed.
		bw.logger.Warn("Batch not buffered; dropping for this tick",
			zap.Int("batch_bytes", len(compressed)),
			zap.Int("buffered_bytes", bw.queue.Bytes()),
			zap.Int("buffered_batches", bw.queue.Len()),
		)
	} else {
		bw.logger.Debug("Buffered metric batch",
			zap.Int("batch_bytes", len(compressed)),
			zap.Int("buffered_bytes", bw.queue.Bytes()),
			zap.Int("buffered_batches", bw.queue.Len()),
		)
	}

	return bw.drain(ctx, authToken)
}

// drain sends up to DrainBatchSize queued batches in FIFO order. The envelope
// is all-or-nothing: success removes every batch; failure leaves all queued
// for retry.
func (bw *BufferedMetricWriter) drain(ctx context.Context, authToken string) error {
	if bw.queue.Len() == 0 {
		return nil
	}

	if authToken == "" {
		bw.logger.Warn("Auth unavailable, holding batches in buffer",
			zap.Int("buffered_batches", bw.queue.Len()),
			zap.Int("buffered_bytes", bw.queue.Bytes()),
		)
		return nil
	}

	if !bw.sender.IngestEndpointAvailable() {
		bw.logger.Warn("Ingest endpoint unavailable, holding batches in buffer",
			zap.Int("buffered_batches", bw.queue.Len()),
			zap.Int("buffered_bytes", bw.queue.Bytes()),
		)
		return nil
	}

	select {
	case <-ctx.Done():
		bw.logger.Warn("Drain cancelled by context",
			zap.Int("buffered_batches", bw.queue.Len()),
			zap.Error(ctx.Err()),
		)
		return nil
	default:
	}

	batches := bw.queue.ReadN(DrainBatchSize)
	if len(batches) == 0 {
		return nil
	}

	response, err := bw.sender.SendCompressedBatches(ctx, batches, authToken)
	if err != nil {
		bw.logger.Warn("Drain send failed, leaving batches in buffer",
			zap.Error(err),
			zap.Int("attempted", len(batches)),
			zap.Int("buffered_batches", bw.queue.Len()),
		)
		return nil
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		bw.logger.Warn("Ingestor rejected envelope, leaving batches in buffer",
			zap.Int("status_code", response.StatusCode),
			zap.Int("attempted", len(batches)),
			zap.Int("buffered_batches", bw.queue.Len()),
		)
		return nil
	}

	// 2xx → atomic accept; pop exactly the batches we just sent.
	for range batches {
		bw.queue.Pop()
	}

	bw.logger.Info("Drained buffered metric batches",
		zap.Int("sent", len(batches)),
		zap.Int("buffered_batches", bw.queue.Len()),
		zap.Int("buffered_bytes", bw.queue.Bytes()),
	)
	return nil
}

// SendHeartbeat delegates to the underlying writer; heartbeats are not buffered.
func (bw *BufferedMetricWriter) SendHeartbeat(ctx context.Context, authToken string, version string) error {
	return bw.aux.SendHeartbeat(ctx, authToken, version)
}

// Close releases the underlying writer's resources.
func (bw *BufferedMetricWriter) Close() error {
	return bw.aux.Close()
}

// BufferedBatches reports the number of batches currently held in the queue.
// Exposed for observability and tests.
func (bw *BufferedMetricWriter) BufferedBatches() int {
	return bw.queue.Len()
}
