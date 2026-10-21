package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/strettch-cloud/sc-metrics-agent/pkg/aggregate"
	"github.com/strettch-cloud/sc-metrics-agent/pkg/clients/metadata"
	"github.com/strettch-cloud/sc-metrics-agent/pkg/collector"
	"github.com/strettch-cloud/sc-metrics-agent/pkg/config"
	"github.com/strettch-cloud/sc-metrics-agent/pkg/decorator"
)

const (
	testVMID      = "vm-under-test"
	testToken     = "token-from-metadata"
	testVersion   = "1.2.3"
	testTimestamp = int64(1700000000000)

	metadataAlwaysDown = math.MaxInt64
)

type collectFunc func(ctx context.Context) ([]*dto.MetricFamily, error)

func (collect collectFunc) Collect(ctx context.Context) ([]*dto.MetricFamily, error) {
	return collect(ctx)
}

type decorateFunc func(families []*dto.MetricFamily) ([]*dto.MetricFamily, error)

func (decorate decorateFunc) Decorate(families []*dto.MetricFamily) ([]*dto.MetricFamily, error) {
	return decorate(families)
}

type aggregateFunc func(families []*dto.MetricFamily) ([]aggregate.MetricWithValue, error)

func (aggregateFamilies aggregateFunc) Aggregate(families []*dto.MetricFamily) ([]aggregate.MetricWithValue, error) {
	return aggregateFamilies(families)
}

type closableCollector struct {
	closed     bool
	closeError error
}

func (closable *closableCollector) Collect(context.Context) ([]*dto.MetricFamily, error) {
	return nil, nil
}

func (closable *closableCollector) Close() error {
	closable.closed = true
	return closable.closeError
}

type recordedWrite struct {
	metrics   []aggregate.MetricWithValue
	authToken string
}

type recordedHeartbeat struct {
	authToken string
	version   string
}

// recordingWriter stands in for the buffered ingest writer and keeps what the
// pipeline hands it.
type recordingWriter struct {
	writes         []recordedWrite
	write          func(ctx context.Context) error
	heartbeats     chan recordedHeartbeat
	heartbeatError error
	closed         bool
	closeError     error
}

func (writer *recordingWriter) WriteMetrics(ctx context.Context, metrics []aggregate.MetricWithValue, authToken string) error {
	writer.writes = append(writer.writes, recordedWrite{metrics: metrics, authToken: authToken})
	if writer.write == nil {
		return nil
	}
	return writer.write(ctx)
}

// SendHeartbeat runs on the processor's own goroutine, so it reports through
// a channel rather than a field the test reads. A heartbeat that arrives while
// the last one is still unread is dropped, so the processor never blocks here.
func (writer *recordingWriter) SendHeartbeat(_ context.Context, authToken string, version string) error {
	select {
	case writer.heartbeats <- recordedHeartbeat{authToken: authToken, version: version}:
	default:
	}
	return writer.heartbeatError
}

func (writer *recordingWriter) Close() error {
	writer.closed = true
	return writer.closeError
}

// pipelineParts holds what a test replaces. Anything left empty is the real
// decorator and aggregator, a recording writer, a collector that returns
// testFamilies, a healthy metadata service and a logger that drops everything.
type pipelineParts struct {
	collector              collector.Collector
	decorator              decorator.MetricDecorator
	aggregator             aggregate.Aggregator
	writer                 *recordingWriter
	failedMetadataRequests int64
	logger                 *zap.Logger
}

// newMetadataServer serves testToken, except to the first failedRequests
// requests, which get the 500 of a metadata service that is down.
func newMetadataServer(t *testing.T, failedRequests int64) *httptest.Server {
	t.Helper()
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) <= failedRequests {
			http.Error(response, "metadata service unavailable", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(response).Encode(metadata.TokenResponse{Token: testToken, CloudAPIUrl: "https://api.test.invalid"})
	}))
	t.Cleanup(server.Close)
	return server
}

// newProcessor builds a processor the test must close itself.
func newProcessor(t *testing.T, parts pipelineParts) (*Processor, *recordingWriter) {
	t.Helper()
	if parts.logger == nil {
		parts.logger = zap.NewNop()
	}
	if parts.collector == nil {
		parts.collector = collectFunc(func(context.Context) ([]*dto.MetricFamily, error) {
			return testFamilies(), nil
		})
	}
	if parts.decorator == nil {
		parts.decorator = decorator.NewMetricDecorator(testVMID, map[string]string{"environment": "test"}, parts.logger)
	}
	if parts.aggregator == nil {
		parts.aggregator = aggregate.NewAggregator(parts.logger)
	}
	if parts.writer == nil {
		parts.writer = &recordingWriter{}
	}
	parts.writer.heartbeats = make(chan recordedHeartbeat, 1)

	cfg := &config.Config{
		VMID:                    testVMID,
		AgentVersion:            testVersion,
		MetadataServiceEndpoint: newMetadataServer(t, parts.failedMetadataRequests).URL,
		HTTPTimeout:             5 * time.Second,
	}
	authManager := metadata.NewAuthManager(cfg, parts.logger)
	processor := NewProcessor(parts.collector, parts.decorator, parts.aggregator, parts.writer, authManager, cfg, parts.logger)
	return processor, parts.writer
}

// startProcessor builds a processor that is closed when the test ends.
func startProcessor(t *testing.T, parts pipelineParts) (*Processor, *recordingWriter) {
	t.Helper()
	processor, writer := newProcessor(t, parts)
	t.Cleanup(func() { _ = processor.Close() })
	return processor, writer
}

func gaugeFamily(name string, value float64, labelName, labelValue string) *dto.MetricFamily {
	timestamp := testTimestamp
	return &dto.MetricFamily{
		Name: &name,
		Type: dto.MetricType_GAUGE.Enum(),
		Metric: []*dto.Metric{{
			Label:       []*dto.LabelPair{{Name: &labelName, Value: &labelValue}},
			Gauge:       &dto.Gauge{Value: &value},
			TimestampMs: &timestamp,
		}},
	}
}

// testFamilies is out of name order on purpose: the pipeline sorts before it
// writes.
func testFamilies() []*dto.MetricFamily {
	return []*dto.MetricFamily{
		gaugeFamily("node_memory_free_bytes", 512, "device", "ram"),
		gaugeFamily("node_cpu_usage", 0.25, "cpu", "0"),
	}
}

func TestProcessWritesEveryMetricWithTheVMIDAndTheToken(t *testing.T) {
	processor, writer := startProcessor(t, pipelineParts{})

	require.NoError(t, processor.Process(t.Context()))

	require.Len(t, writer.writes, 1)
	assert.Equal(t, testToken, writer.writes[0].authToken)
	assert.Equal(t, []aggregate.MetricWithValue{
		{
			Name:      "node_cpu_usage",
			Labels:    map[string]string{"cpu": "0", "vm_id": testVMID, "environment": "test"},
			Value:     0.25,
			Timestamp: testTimestamp,
			Type:      "gauge",
		},
		{
			Name:      "node_memory_free_bytes",
			Labels:    map[string]string{"device": "ram", "vm_id": testVMID, "environment": "test"},
			Value:     512,
			Timestamp: testTimestamp,
			Type:      "gauge",
		},
	}, writer.writes[0].metrics)
}

func TestProcessStopsAtTheStageThatFails(t *testing.T) {
	stageFailure := errors.New("stage failure")
	tests := map[string]struct {
		parts      pipelineParts
		wantError  string
		wantWrites int
	}{
		"collection": {
			parts: pipelineParts{collector: collectFunc(func(context.Context) ([]*dto.MetricFamily, error) {
				return nil, stageFailure
			})},
			wantError: "failed to collect metrics",
		},
		"decoration": {
			parts: pipelineParts{decorator: decorateFunc(func([]*dto.MetricFamily) ([]*dto.MetricFamily, error) {
				return nil, stageFailure
			})},
			wantError: "failed to decorate metrics",
		},
		"aggregation": {
			parts: pipelineParts{aggregator: aggregateFunc(func([]*dto.MetricFamily) ([]aggregate.MetricWithValue, error) {
				return nil, stageFailure
			})},
			wantError: "failed to aggregate metrics",
		},
		"write": {
			parts: pipelineParts{writer: &recordingWriter{write: func(context.Context) error {
				return stageFailure
			}}},
			wantError:  "failed to write metrics",
			wantWrites: 1,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			processor, writer := startProcessor(t, test.parts)

			err := processor.Process(t.Context())

			require.ErrorIs(t, err, stageFailure)
			assert.ErrorContains(t, err, test.wantError)
			assert.Len(t, writer.writes, test.wantWrites)
		})
	}
}

// The agent cancels the context when it shuts down, and a pipeline that is
// part way through must stop rather than write.
func TestProcessWritesNothingOnceTheContextIsCancelled(t *testing.T) {
	tests := map[string]func(cancel context.CancelFunc) pipelineParts{
		"before collection": func(cancel context.CancelFunc) pipelineParts {
			cancel()
			return pipelineParts{}
		},
		"during collection": func(cancel context.CancelFunc) pipelineParts {
			return pipelineParts{collector: collectFunc(func(context.Context) ([]*dto.MetricFamily, error) {
				cancel()
				return testFamilies(), nil
			})}
		},
		"during decoration": func(cancel context.CancelFunc) pipelineParts {
			return pipelineParts{decorator: decorateFunc(func(families []*dto.MetricFamily) ([]*dto.MetricFamily, error) {
				cancel()
				return families, nil
			})}
		},
		"during aggregation": func(cancel context.CancelFunc) pipelineParts {
			return pipelineParts{aggregator: aggregateFunc(func([]*dto.MetricFamily) ([]aggregate.MetricWithValue, error) {
				cancel()
				return []aggregate.MetricWithValue{{Name: "node_cpu_usage", Value: 0.25}}, nil
			})}
		},
	}
	for name, partsThatCancel := range tests {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			processor, writer := startProcessor(t, partsThatCancel(cancel))

			assert.ErrorIs(t, processor.Process(ctx), context.Canceled)
			assert.Empty(t, writer.writes)
		})
	}
}

// The writer sends over the network, so it needs the caller's context to give
// up when the agent shuts down.
func TestProcessGivesTheWriterTheCallersContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	processor, _ := startProcessor(t, pipelineParts{writer: &recordingWriter{write: func(writeContext context.Context) error {
		cancel()
		return writeContext.Err()
	}}})

	assert.ErrorIs(t, processor.Process(ctx), context.Canceled)
}

func TestProcessWritesNothingWhenThereIsNothingToSend(t *testing.T) {
	tests := map[string]pipelineParts{
		"nothing collected": {collector: collectFunc(func(context.Context) ([]*dto.MetricFamily, error) {
			return nil, nil
		})},
		"nothing left after aggregation": {aggregator: aggregateFunc(func([]*dto.MetricFamily) ([]aggregate.MetricWithValue, error) {
			return nil, nil
		})},
	}
	for name, parts := range tests {
		t.Run(name, func(t *testing.T) {
			processor, writer := startProcessor(t, parts)

			assert.NoError(t, processor.Process(t.Context()))
			assert.Empty(t, writer.writes)
		})
	}
}

// The buffered writer keeps a batch it cannot send yet, so a metadata outage
// must not lose metrics: the pipeline still hands them over, without a token.
// Takes about 1.5 seconds, the pipeline's wait between its token retries.
func TestProcessStillWritesWhenTheMetadataServiceIsDown(t *testing.T) {
	processor, writer := startProcessor(t, pipelineParts{failedMetadataRequests: metadataAlwaysDown})

	require.NoError(t, processor.Process(t.Context()))

	require.Len(t, writer.writes, 1)
	assert.Empty(t, writer.writes[0].authToken)
	assert.Len(t, writer.writes[0].metrics, 2)
}

func TestProcessFetchesTheTokenWhenTheMetadataServiceWasDownAtStart(t *testing.T) {
	// The one failed request is the processor's own, when it starts.
	processor, writer := startProcessor(t, pipelineParts{failedMetadataRequests: 1})

	require.NoError(t, processor.Process(t.Context()))

	require.Len(t, writer.writes, 1)
	assert.Equal(t, testToken, writer.writes[0].authToken)
}

// Shutdown must not wait out the token retries.
func TestProcessStopsWaitingForTheTokenWhenTheContextEnds(t *testing.T) {
	tests := map[string]struct {
		context   func(t *testing.T) context.Context
		wantError error
	}{
		"ended before the first retry": {
			context: func(t *testing.T) context.Context {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				return ctx
			},
			wantError: context.Canceled,
		},
		"ends while waiting between retries": {
			context: func(t *testing.T) context.Context {
				ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
				t.Cleanup(cancel)
				return ctx
			},
			wantError: context.DeadlineExceeded,
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			processor, writer := startProcessor(t, pipelineParts{failedMetadataRequests: metadataAlwaysDown})

			assert.ErrorIs(t, processor.Process(test.context(t)), test.wantError)
			assert.Empty(t, writer.writes)
		})
	}
}

func TestProcessorSendsAHeartbeatWhenItStarts(t *testing.T) {
	_, writer := startProcessor(t, pipelineParts{})

	select {
	case heartbeat := <-writer.heartbeats:
		assert.Equal(t, recordedHeartbeat{authToken: testToken, version: testVersion}, heartbeat)
	case <-time.After(5 * time.Second):
		t.Fatal("no heartbeat was sent when the processor started")
	}
}

func TestProcessorKeepsSendingHeartbeats(t *testing.T) {
	// The real interval is ten minutes.
	realInterval := heartbeatInterval
	heartbeatInterval = 10 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = realInterval })
	_, writer := startProcessor(t, pipelineParts{})

	// The first heartbeat is the one sent at start; the other two can only
	// come from the interval.
	for received := range 3 {
		select {
		case heartbeat := <-writer.heartbeats:
			assert.Equal(t, recordedHeartbeat{authToken: testToken, version: testVersion}, heartbeat)
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 3 heartbeats were sent", received)
		}
	}
}

func TestProcessorLogsAHeartbeatItCouldNotSend(t *testing.T) {
	tests := map[string]struct {
		parts      pipelineParts
		wantLogged string
	}{
		"no token": {
			parts:      pipelineParts{failedMetadataRequests: metadataAlwaysDown},
			wantLogged: "Failed to get auth token for heartbeat",
		},
		"send failed": {
			parts:      pipelineParts{writer: &recordingWriter{heartbeatError: errors.New("ingest unreachable")}},
			wantLogged: "Failed to send heartbeat",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			core, logs := observer.New(zap.ErrorLevel)
			test.parts.logger = zap.New(core)
			startProcessor(t, test.parts)

			assert.Eventually(t, func() bool {
				return logs.FilterMessage(test.wantLogged).Len() == 1
			}, 5*time.Second, 10*time.Millisecond)
		})
	}
}

// A collector that fails to close must not leave the writer open.
func TestCloseClosesTheWriterEvenWhenTheCollectorFailsToClose(t *testing.T) {
	collectorThatFailsToClose := &closableCollector{closeError: errors.New("collector close failed")}
	processor, writer := newProcessor(t, pipelineParts{
		collector: collectorThatFailsToClose,
		writer:    &recordingWriter{closeError: errors.New("writer close failed")},
	})

	_ = processor.Close()

	assert.True(t, collectorThatFailsToClose.closed)
	assert.True(t, writer.closed)
}
