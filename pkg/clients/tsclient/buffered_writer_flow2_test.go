package tsclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/snappy"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/strettch-cloud/sc-metrics-agent/pkg/aggregate"
	"github.com/strettch-cloud/sc-metrics-agent/pkg/buffer"
	"github.com/strettch-cloud/sc-metrics-agent/pkg/clients/metadata"
	"github.com/strettch-cloud/sc-metrics-agent/pkg/config"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// sentRequestF2 is one request resource-manager received: the batches it
// carried, by name, in order.
type sentRequestF2 struct {
	at      time.Time
	batches []string
}

// fakeResourceManagerF2 answers each ingest request with respond.
type fakeResourceManagerF2 struct {
	server  *httptest.Server
	mu      sync.Mutex
	sent    []sentRequestF2
	respond func(w http.ResponseWriter, batches []string, attempt int)
}

func newFakeResourceManagerF2(t *testing.T, respond func(w http.ResponseWriter, batches []string, attempt int)) *fakeResourceManagerF2 {
	t.Helper()
	rm := &fakeResourceManagerF2{respond: respond}
	rm.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/resource-manager/api/v1/metrics/ingest") {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		batches := decodeEnvelopeF2(t, body)
		rm.mu.Lock()
		rm.sent = append(rm.sent, sentRequestF2{at: time.Now(), batches: batches})
		attempt := len(rm.sent)
		rm.mu.Unlock()
		rm.respond(w, batches, attempt)
	}))
	t.Cleanup(rm.server.Close)
	return rm
}

func (rm *fakeResourceManagerF2) requests() []sentRequestF2 {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	return append([]sentRequestF2(nil), rm.sent...)
}

func decodeEnvelopeF2(t *testing.T, body []byte) []string {
	var encoded []string
	if err := json.Unmarshal(body, &encoded); err != nil {
		t.Errorf("envelope is not a JSON array of strings: %v", err)
		return nil
	}
	var names []string
	for _, item := range encoded {
		names = append(names, decodeBatchF2(t, item))
	}
	return names
}

func decodeBatchF2(t *testing.T, item string) string {
	compressed, err := base64.StdEncoding.DecodeString(item)
	if err != nil {
		t.Errorf("batch is not base64: %v", err)
		return ""
	}
	return batchNameF2(t, compressed)
}

func batchNameF2(t *testing.T, compressed []byte) string {
	plain, err := snappy.Decode(nil, compressed)
	if err != nil {
		t.Errorf("batch is not snappy: %v", err)
		return ""
	}
	var metrics []aggregate.MetricWithValue
	if err := json.Unmarshal(plain, &metrics); err != nil || len(metrics) == 0 {
		t.Errorf("batch is not a JSON list of metrics: %v", err)
		return ""
	}
	return metrics[0].Name
}

func batchMetricsF2(name string) []aggregate.MetricWithValue {
	return []aggregate.MetricWithValue{{Name: name, Labels: map[string]string{"vm_id": "f2"}, Value: 1, Timestamp: 1, Type: "gauge"}}
}

// fakeMetadataF2 hands out a new token on each request.
func fakeMetadataF2(t *testing.T, cloudAPIURL string) *httptest.Server {
	t.Helper()
	var issued atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := issued.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"token": fmt.Sprintf("token-%d", n), "cloudAPIUrl": cloudAPIURL})
	}))
	t.Cleanup(server.Close)
	return server
}

type drainRigF2 struct {
	writer *BufferedMetricWriter
	queue  *buffer.DiskQueue
	dir    string
	token  string
	logs   *observer.ObservedLogs
	client *Client
}

type rigOptionsF2 struct {
	cloudAPIURL  string
	maxBytes     int
	retryDelay   time.Duration
	timeout      time.Duration
	savedBatches int
}

// newDrainRigF2 builds the writer the agent builds, with the agent's fixed
// retry settings unless the options shorten them.
func newDrainRigF2(t *testing.T, opts rigOptionsF2) *drainRigF2 {
	t.Helper()
	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)

	cfg := config.DefaultConfig()
	cfg.VMID = "f2-vm"
	cfg.MetadataServiceEndpoint = fakeMetadataF2(t, opts.cloudAPIURL).URL
	authMgr := metadata.NewAuthManager(cfg, logger)
	t.Cleanup(authMgr.Close)
	_ = authMgr.EnsureValidToken(context.Background())

	clientConfig := ClientConfig{AuthMgr: authMgr, Timeout: cfg.HTTPTimeout, MaxRetries: cfg.MaxRetries, RetryDelay: cfg.RetryInterval}
	if opts.retryDelay > 0 {
		clientConfig.RetryDelay = opts.retryDelay
	}
	if opts.timeout > 0 {
		clientConfig.Timeout = opts.timeout
	}
	client := NewClient(clientConfig, logger)

	dir := t.TempDir()
	maxBytes := opts.maxBytes
	if maxBytes == 0 {
		maxBytes = buffer.DefaultMaxBytes
	}
	queue, err := buffer.NewDiskQueue(dir, maxBytes, logger)
	require.NoError(t, err)
	for i := 0; i < opts.savedBatches; i++ {
		blob, err := client.CompressMetrics(batchMetricsF2(fmt.Sprintf("batch-%02d", i)))
		require.NoError(t, err)
		require.True(t, queue.Push(blob))
	}

	return &drainRigF2{
		writer: NewBufferedMetricWriter(client, queue, NewAuxiliaryWriter(client, logger), logger),
		queue:  queue,
		dir:    dir,
		token:  authMgr.GetCurrentToken(),
		logs:   logs,
		client: client,
	}
}

// round saves a new batch named name, as one round does, then sends.
func (rig *drainRigF2) round(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	require.NoError(t, rig.writer.WriteMetrics(ctx, batchMetricsF2(name), rig.token))
}

// savedOnDisk reopens the buffer directory and lists the batches in it.
func (rig *drainRigF2) savedOnDisk(t *testing.T) []string {
	t.Helper()
	reopened, err := buffer.NewDiskQueue(rig.dir, buffer.DefaultMaxBytes, zap.NewNop())
	require.NoError(t, err)
	var names []string
	for _, blob := range reopened.ReadN(1000) {
		names = append(names, batchNameF2(t, blob))
	}
	return names
}

func (rig *drainRigF2) logged(message string) []observer.LoggedEntry {
	return rig.logs.FilterMessage(message).All()
}

func batchNamesF2(from, to int) []string {
	var names []string
	for i := from; i < to; i++ {
		names = append(names, fmt.Sprintf("batch-%02d", i))
	}
	return names
}

func answerF2(status int) func(http.ResponseWriter, []string, int) {
	return func(w http.ResponseWriter, _ []string, _ int) { w.WriteHeader(status) }
}

func TestSendingSavedBatches(t *testing.T) {
	t.Run("with no token, no request is sent, every batch stays saved, and the agent logs that the token is missing", func(t *testing.T) {
		rm := newFakeResourceManagerF2(t, answerF2(http.StatusOK))
		rig := newDrainRigF2(t, rigOptionsF2{cloudAPIURL: rm.server.URL, savedBatches: 2})
		const noToken = ""

		require.NoError(t, rig.writer.WriteMetrics(context.Background(), batchMetricsF2("new"), noToken))

		assert.Empty(t, rm.requests())
		assert.Equal(t, append(batchNamesF2(0, 2), "new"), rig.savedOnDisk(t))
		assert.NotEmpty(t, rig.logs.Filter(func(e observer.LoggedEntry) bool {
			message := strings.ToLower(e.Message)
			return e.Level >= zapcore.WarnLevel && (strings.Contains(message, "auth") || strings.Contains(message, "token"))
		}).All())
	})

	t.Run("with no API address, no request is sent, every batch stays saved, and the agent logs that the address is missing", func(t *testing.T) {
		rig := newDrainRigF2(t, rigOptionsF2{cloudAPIURL: "", savedBatches: 2})
		require.NotEmpty(t, rig.token)

		rig.round(t, context.Background(), "new")

		assert.Equal(t, append(batchNamesF2(0, 2), "new"), rig.savedOnDisk(t))
		assert.NotEmpty(t, rig.logs.Filter(func(e observer.LoggedEntry) bool {
			message := strings.ToLower(e.Message)
			return e.Level >= zapcore.WarnLevel && (strings.Contains(message, "endpoint") || strings.Contains(message, "address") || strings.Contains(message, "url"))
		}).All())
	})

	t.Run("with no saved batch, no request is sent", func(t *testing.T) {
		rm := newFakeResourceManagerF2(t, answerF2(http.StatusOK))
		rig := newDrainRigF2(t, rigOptionsF2{cloudAPIURL: rm.server.URL})

		require.NoError(t, rig.writer.WriteMetrics(context.Background(), nil, rig.token))

		assert.Empty(t, rm.requests())
	})

	for _, status := range []int{http.StatusOK, http.StatusAccepted, http.StatusNoContent, 299} {
		t.Run(fmt.Sprintf("a %d answer deletes the sent batches and logs how many were sent", status), func(t *testing.T) {
			rm := newFakeResourceManagerF2(t, answerF2(status))
			rig := newDrainRigF2(t, rigOptionsF2{cloudAPIURL: rm.server.URL, savedBatches: 3})

			rig.round(t, context.Background(), "new")

			require.Len(t, rm.requests(), 1)
			assert.Equal(t, append(batchNamesF2(0, 3), "new"), rm.requests()[0].batches)
			assert.Empty(t, rig.savedOnDisk(t))
			drained := rig.logged("Drained buffered metric batches")
			require.Len(t, drained, 1)
			assert.EqualValues(t, 4, drained[0].ContextMap()["sent"])
		})
	}

	t.Run("the 50 oldest batches go in one request and the rest stay saved", func(t *testing.T) {
		rm := newFakeResourceManagerF2(t, answerF2(http.StatusOK))
		rig := newDrainRigF2(t, rigOptionsF2{cloudAPIURL: rm.server.URL, savedBatches: 54})

		rig.round(t, context.Background(), "new")

		require.Len(t, rm.requests(), 1)
		assert.Equal(t, batchNamesF2(0, 50), rm.requests()[0].batches)
		assert.Equal(t, append(batchNamesF2(50, 54), "new"), rig.savedOnDisk(t))
	})

	t.Run("a 200 answer deletes exactly the sent batches, not one saved while the request was out", func(t *testing.T) {
		var rig *drainRigF2
		rm := newFakeResourceManagerF2(t, func(w http.ResponseWriter, _ []string, _ int) {
			blob, _ := rig.client.CompressMetrics(batchMetricsF2("saved-during-send"))
			rig.queue.Push(blob)
			w.WriteHeader(http.StatusOK)
		})
		rig = newDrainRigF2(t, rigOptionsF2{cloudAPIURL: rm.server.URL, savedBatches: 2})

		rig.round(t, context.Background(), "new")

		assert.Equal(t, []string{"saved-during-send"}, rig.savedOnDisk(t))
	})

	t.Run("a batch file that cannot be read is deleted and lost, and the other batches are still sent", func(t *testing.T) {
		rm := newFakeResourceManagerF2(t, answerF2(http.StatusOK))
		rig := newDrainRigF2(t, rigOptionsF2{cloudAPIURL: rm.server.URL, savedBatches: 3})
		files, err := filepath.Glob(filepath.Join(rig.dir, "*.batch"))
		require.NoError(t, err)
		sort.Strings(files)
		unreadable := files[1]
		require.NoError(t, os.Remove(unreadable))
		require.NoError(t, os.Mkdir(unreadable, 0o750))

		rig.round(t, context.Background(), "new")

		require.Len(t, rm.requests(), 1)
		assert.Equal(t, []string{"batch-00", "batch-02", "new"}, rm.requests()[0].batches)
		assert.NoFileExists(t, unreadable)
		assert.NoDirExists(t, unreadable)
		assert.Len(t, rig.logged("Dropping unreadable batch from queue"), 1)
	})

	t.Run("a round whose batch could not be saved still sends the batches saved before", func(t *testing.T) {
		rm := newFakeResourceManagerF2(t, answerF2(http.StatusOK))
		probe := newDrainRigF2(t, rigOptionsF2{cloudAPIURL: rm.server.URL, savedBatches: 2})
		fullAtTwoBatches := probe.queue.Bytes()
		rig := newDrainRigF2(t, rigOptionsF2{cloudAPIURL: rm.server.URL, savedBatches: 2, maxBytes: fullAtTwoBatches})

		rig.round(t, context.Background(), "batch-that-does-not-fit")

		require.Len(t, rm.requests(), 1)
		assert.Equal(t, batchNamesF2(0, 2), rm.requests()[0].batches)
		assert.Empty(t, rig.savedOnDisk(t))
	})

	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprintf("a %d answer leaves every batch saved", status), func(t *testing.T) {
			rm := newFakeResourceManagerF2(t, answerF2(status))
			rig := newDrainRigF2(t, rigOptionsF2{cloudAPIURL: rm.server.URL, savedBatches: 2})

			rig.round(t, context.Background(), "new")

			assert.Equal(t, append(batchNamesF2(0, 2), "new"), rig.savedOnDisk(t))
		})
	}

	t.Run("any other status, such as 404, leaves every batch saved and is logged with the status", func(t *testing.T) {
		rm := newFakeResourceManagerF2(t, answerF2(http.StatusNotFound))
		rig := newDrainRigF2(t, rigOptionsF2{cloudAPIURL: rm.server.URL, savedBatches: 2})

		rig.round(t, context.Background(), "new")

		require.Len(t, rm.requests(), 1)
		assert.Equal(t, append(batchNamesF2(0, 2), "new"), rig.savedOnDisk(t))
		rejected := rig.logged("Ingestor rejected envelope, leaving batches in buffer")
		require.Len(t, rejected, 1)
		assert.EqualValues(t, http.StatusNotFound, rejected[0].ContextMap()["status_code"])
	})
}

func TestSendingSavedBatchesTheServerBlamesOnTheBatch(t *testing.T) {
	// Envelopes of more than one batch get envelopeStatus; a single batch gets
	// the answer perBatch gives for it.
	scripted := func(envelopeStatus int, perBatch map[string]int) func(http.ResponseWriter, []string, int) {
		return func(w http.ResponseWriter, batches []string, _ int) {
			if len(batches) != 1 {
				w.WriteHeader(envelopeStatus)
				_, _ = w.Write([]byte("envelope refused"))
				return
			}
			status, ok := perBatch[batches[0]]
			if !ok {
				status = http.StatusOK
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte("refused " + batches[0]))
		}
	}

	for _, envelopeStatus := range []int{http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity} {
		t.Run(fmt.Sprintf("after a %d, each batch is sent again alone; accepted ones are deleted and a rejected one is dropped and logged with its status and body", envelopeStatus), func(t *testing.T) {
			rm := newFakeResourceManagerF2(t, scripted(envelopeStatus, map[string]int{"batch-01": http.StatusUnprocessableEntity}))
			rig := newDrainRigF2(t, rigOptionsF2{cloudAPIURL: rm.server.URL, savedBatches: 3})

			rig.round(t, context.Background(), "new")

			var alone [][]string
			for _, request := range rm.requests()[1:] {
				alone = append(alone, request.batches)
			}
			assert.Equal(t, [][]string{{"batch-00"}, {"batch-01"}, {"batch-02"}, {"new"}}, alone)
			assert.Empty(t, rig.savedOnDisk(t))
			dropped := rig.logged("Dropping rejected batch")
			require.Len(t, dropped, 1)
			assert.Contains(t, fmt.Sprint(dropped[0].ContextMap()), "422")
			assert.Contains(t, fmt.Sprint(dropped[0].ContextMap()), "refused batch-01")
		})
	}

	for _, batchStatus := range []int{http.StatusBadRequest, http.StatusRequestEntityTooLarge} {
		t.Run(fmt.Sprintf("a batch sent alone that gets %d is dropped", batchStatus), func(t *testing.T) {
			rm := newFakeResourceManagerF2(t, scripted(http.StatusBadRequest, map[string]int{"batch-00": batchStatus}))
			rig := newDrainRigF2(t, rigOptionsF2{cloudAPIURL: rm.server.URL, savedBatches: 1})

			rig.round(t, context.Background(), "new")

			assert.Empty(t, rig.savedOnDisk(t))
			assert.Len(t, rig.logged("Dropping rejected batch"), 1)
		})
	}

	t.Run("when a batch sent alone gets any other answer, the agent stops and that batch and the untried ones stay saved", func(t *testing.T) {
		rm := newFakeResourceManagerF2(t, scripted(http.StatusBadRequest, map[string]int{"batch-01": http.StatusNotFound}))
		rig := newDrainRigF2(t, rigOptionsF2{cloudAPIURL: rm.server.URL, savedBatches: 3})

		rig.round(t, context.Background(), "new")

		var alone [][]string
		for _, request := range rm.requests()[1:] {
			alone = append(alone, request.batches)
		}
		assert.Equal(t, [][]string{{"batch-00"}, {"batch-01"}}, alone)
		assert.Equal(t, []string{"batch-01", "batch-02", "new"}, rig.savedOnDisk(t))
	})

	t.Run("batches sent alone go at most 2 requests a second", func(t *testing.T) {
		rm := newFakeResourceManagerF2(t, scripted(http.StatusBadRequest, nil))
		rig := newDrainRigF2(t, rigOptionsF2{cloudAPIURL: rm.server.URL, savedBatches: 3})

		rig.round(t, context.Background(), "new")

		requests := rm.requests()
		require.Len(t, requests, 5)
		const minimumGap = 500*time.Millisecond - 20*time.Millisecond
		for i := 2; i < len(requests); i++ {
			assert.GreaterOrEqual(t, requests[i].at.Sub(requests[i-1].at), minimumGap, "gap before request %d", i)
		}
	})
}

func TestSendingSavedBatchesWhenResourceManagerIsUnavailable(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		status := status
		t.Run(fmt.Sprintf("a %d is tried again after its Retry-After, and a later 200 deletes the sent batches", status), func(t *testing.T) {
			t.Parallel()
			rm := newFakeResourceManagerF2(t, func(w http.ResponseWriter, _ []string, attempt int) {
				if attempt == 1 {
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(status)
					return
				}
				w.WriteHeader(http.StatusOK)
			})
			rig := newDrainRigF2(t, rigOptionsF2{cloudAPIURL: rm.server.URL, savedBatches: 2})

			rig.round(t, context.Background(), "new")

			requests := rm.requests()
			require.Len(t, requests, 2)
			assert.Equal(t, requests[0].batches, requests[1].batches)
			assert.GreaterOrEqual(t, requests[1].at.Sub(requests[0].at), 950*time.Millisecond)
			assert.Less(t, requests[1].at.Sub(requests[0].at), 3*time.Second)
			assert.Empty(t, rig.savedOnDisk(t))
		})
	}

	t.Run("a send is tried 3 more times, and when none gets 200 every batch stays saved and the failure is logged", func(t *testing.T) {
		t.Parallel()
		rm := newFakeResourceManagerF2(t, func(w http.ResponseWriter, _ []string, _ int) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
		})
		rig := newDrainRigF2(t, rigOptionsF2{cloudAPIURL: rm.server.URL, savedBatches: 2})

		rig.round(t, context.Background(), "new")

		assert.Len(t, rm.requests(), 4)
		assert.Equal(t, append(batchNamesF2(0, 2), "new"), rig.savedOnDisk(t))
		assert.Len(t, rig.logged("Drain send failed, leaving batches in buffer"), 1)
	})

	t.Run("with no Retry-After, the agent waits 5 s before trying again", func(t *testing.T) {
		t.Parallel()
		rm := newFakeResourceManagerF2(t, func(w http.ResponseWriter, _ []string, attempt int) {
			if attempt == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.WriteHeader(http.StatusOK)
		})
		rig := newDrainRigF2(t, rigOptionsF2{cloudAPIURL: rm.server.URL, savedBatches: 1})

		rig.round(t, context.Background(), "new")

		requests := rm.requests()
		require.Len(t, requests, 2)
		gap := requests[1].at.Sub(requests[0].at)
		assert.GreaterOrEqual(t, gap, 4950*time.Millisecond)
		assert.Less(t, gap, 6500*time.Millisecond)
	})

	t.Run("a send with no connection is tried 3 more times, then every batch stays saved and the failure is logged", func(t *testing.T) {
		t.Parallel()
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		var connections atomic.Int64
		go func() {
			for {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				connections.Add(1)
				_ = conn.Close()
			}
		}()
		t.Cleanup(func() { _ = listener.Close() })
		rig := newDrainRigF2(t, rigOptionsF2{cloudAPIURL: "http://" + listener.Addr().String(), savedBatches: 2, retryDelay: 50 * time.Millisecond})

		rig.round(t, context.Background(), "new")

		assert.EqualValues(t, 4, connections.Load())
		assert.Equal(t, append(batchNamesF2(0, 2), "new"), rig.savedOnDisk(t))
		assert.Len(t, rig.logged("Drain send failed, leaving batches in buffer"), 1)
	})

	t.Run("a send with no answer in time is tried 3 more times, then every batch stays saved", func(t *testing.T) {
		t.Parallel()
		release := make(chan struct{})
		rm := newFakeResourceManagerF2(t, func(w http.ResponseWriter, _ []string, _ int) { <-release })
		t.Cleanup(func() { close(release) })
		rig := newDrainRigF2(t, rigOptionsF2{cloudAPIURL: rm.server.URL, savedBatches: 1, retryDelay: 50 * time.Millisecond, timeout: 200 * time.Millisecond})

		rig.round(t, context.Background(), "new")

		assert.Len(t, rm.requests(), 4)
		assert.Equal(t, []string{"batch-00", "new"}, rig.savedOnDisk(t))
		assert.Len(t, rig.logged("Drain send failed, leaving batches in buffer"), 1)
	})

	t.Run("the agent waits at most 30 s for an answer", func(t *testing.T) {
		cfg := config.DefaultConfig()

		assert.Equal(t, 30*time.Second, cfg.HTTPTimeout)
	})
}

func TestHowLongTheAgentWaitsForRetryAfter(t *testing.T) {
	rows := []struct {
		name       string
		retryAfter string
		wait       time.Duration
	}{
		{"none", "", 5 * time.Second},
		{"0", "0", 5 * time.Second},
		{"a negative number", "-3", 5 * time.Second},
		{"not a number or a date", "soon", 5 * time.Second},
		{"a date in the past", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat), 5 * time.Second},
		{"10", "10", 10 * time.Second},
		{"120", "120", 30 * time.Second},
		{"a date more than 30 s ahead", time.Now().Add(2 * time.Hour).UTC().Format(http.TimeFormat), 30 * time.Second},
	}
	for _, row := range rows {
		row := row
		t.Run(fmt.Sprintf("Retry-After %s makes the agent wait %s", row.name, row.wait), func(t *testing.T) {
			t.Parallel()
			rm := newFakeResourceManagerF2(t, func(w http.ResponseWriter, _ []string, _ int) {
				if row.retryAfter != "" {
					w.Header().Set("Retry-After", row.retryAfter)
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			})
			rig := newDrainRigF2(t, rigOptionsF2{cloudAPIURL: rm.server.URL, savedBatches: 1})
			ctx, cancel := context.WithCancel(context.Background())
			// The wait is read from the retry log line, then the send is stopped,
			// so a long wait does not make the test long.
			go func() {
				defer cancel()
				for rig.logs.FilterMessage("Retrying request").Len() == 0 {
					time.Sleep(10 * time.Millisecond)
				}
			}()

			rig.round(t, ctx, "new")

			retries := rig.logged("Retrying request")
			require.NotEmpty(t, retries)
			assert.Equal(t, row.wait, retries[0].ContextMap()["wait_time"])
		})
	}
}
