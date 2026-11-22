package tsclient

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/strettch-cloud/sc-metrics-agent/pkg/aggregate"
	"github.com/strettch-cloud/sc-metrics-agent/pkg/buffer"
	"go.uber.org/zap"
)

// DrainBatchSize balances recovery speed and request size: 50 batches clear a
// 24-hour backlog in ~30 minutes while keeping requests ~135 KiB.
const DrainBatchSize = 50

// splitSendInterval spaces out batches sent one at a time to 2 requests a
// second, well under the ingest endpoint's per-token rate limit, which the
// heartbeat counts against too. A variable only so tests can shorten it.
var splitSendInterval = 500 * time.Millisecond

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

	if blamesTheBatch(response.StatusCode) {
		if len(batches) == 1 {
			bw.dropRejected(response)
		} else {
			bw.sendEachAlone(ctx, batches, authToken)
		}
		return nil
	}

	if !isSuccess(response.StatusCode) {
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

// sendEachAlone sends each batch of a refused envelope in its own request, so
// one bad batch can't hold back the others. It stops at the first answer that
// is neither a success nor a refusal of that batch, leaving that batch and the
// ones after it saved for a later round.
func (bw *BufferedMetricWriter) sendEachAlone(ctx context.Context, batches [][]byte, authToken string) {
	bw.logger.Warn("Ingestor refused the envelope, sending its batches one at a time",
		zap.Int("batches", len(batches)),
	)
	for _, batch := range batches {
		select {
		case <-time.After(splitSendInterval):
		case <-ctx.Done():
			return
		}

		response, err := bw.sender.SendCompressedBatches(ctx, [][]byte{batch}, authToken)
		switch {
		case err != nil:
			bw.logger.Warn("Sending a batch alone failed, leaving it and the rest in buffer", zap.Error(err))
			return
		case isSuccess(response.StatusCode):
			bw.queue.Pop()
		case blamesTheBatch(response.StatusCode):
			bw.dropRejected(response)
		default:
			bw.logger.Warn("Ingestor answered a batch sent alone, leaving it and the rest in buffer",
				zap.Int("status_code", response.StatusCode),
				zap.Int("buffered_batches", bw.queue.Len()),
			)
			return
		}
	}
	bw.logger.Info("Sent buffered batches one at a time", zap.Int("buffered_batches", bw.queue.Len()))
}

// dropRejected deletes the oldest saved batch, the one the server refused.
// Sending it again would get the same answer and block the queue.
func (bw *BufferedMetricWriter) dropRejected(response *Response) {
	bw.queue.Pop()
	bw.logger.Error("Dropping rejected batch",
		zap.Int("status_code", response.StatusCode),
		zap.String("body", string(response.Body)),
	)
}

// blamesTheBatch reports whether the status says the request's content is at
// fault, so the same batch would be refused again.
func blamesTheBatch(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

func isSuccess(status int) bool { return status >= 200 && status < 300 }

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
