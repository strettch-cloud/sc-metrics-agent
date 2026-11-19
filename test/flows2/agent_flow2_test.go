// Package flows2 runs the real agent binary, built with the e2e tag, against
// fake metadata and resource-manager servers.
package flows2

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const buildVersionF2 = "9.8.7-flow2"

var agentBinaryF2 string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agent-flow2-")
	if err != nil {
		panic(err)
	}
	agentBinaryF2 = filepath.Join(dir, "sc-metrics-agent")
	build := exec.Command("go", "build", "-tags", "e2e", "-ldflags", "-X main.version="+buildVersionF2, "-o", agentBinaryF2, "./cmd/agent")
	build.Dir = filepath.Join("..", "..")
	build.Stdout, build.Stderr = os.Stderr, os.Stderr
	if err := build.Run(); err != nil {
		panic(fmt.Sprintf("building the agent: %v", err))
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// fakeCloudF2 is the metadata service and resource-manager.
type fakeCloudF2 struct {
	metadata *httptest.Server
	rm       *httptest.Server

	mu         sync.Mutex
	tokens     []time.Time
	ingests    []ingestF2
	heartbeats []map[string]string
	answer     func(w http.ResponseWriter, attempt int)
}

type ingestF2 struct {
	at            time.Time
	authorization string
}

func newFakeCloudF2(t *testing.T, answer func(w http.ResponseWriter, attempt int)) *fakeCloudF2 {
	t.Helper()
	cloud := &fakeCloudF2{answer: answer}
	cloud.rm = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasSuffix(r.URL.Path, "/compute/agent/heartbeat"):
			heartbeat := map[string]string{}
			_ = json.Unmarshal(body, &heartbeat)
			cloud.mu.Lock()
			cloud.heartbeats = append(cloud.heartbeats, heartbeat)
			cloud.mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/metrics/ingest"):
			cloud.mu.Lock()
			cloud.ingests = append(cloud.ingests, ingestF2{at: time.Now(), authorization: r.Header.Get("Authorization")})
			attempt := len(cloud.ingests)
			cloud.mu.Unlock()
			cloud.answer(w, attempt)
		default:
			http.NotFound(w, r)
		}
	}))
	cloud.metadata = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metadata/v1/auth-token" || r.Header.Get("Metadata-Flavor") != "StrettchCloud" {
			http.Error(w, "refused", http.StatusForbidden)
			return
		}
		cloud.mu.Lock()
		cloud.tokens = append(cloud.tokens, time.Now())
		n := len(cloud.tokens)
		cloud.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"token": fmt.Sprintf("token-%d", n), "cloudAPIUrl": cloud.rm.URL})
	}))
	t.Cleanup(cloud.metadata.Close)
	t.Cleanup(cloud.rm.Close)
	return cloud
}

func (c *fakeCloudF2) snapshot() (tokens []time.Time, ingests []ingestF2, heartbeats []map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Time(nil), c.tokens...), append([]ingestF2(nil), c.ingests...), append([]map[string]string(nil), c.heartbeats...)
}

// lockedBufferF2 collects the agent's output while it runs.
type lockedBufferF2 struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBufferF2) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBufferF2) messages() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var messages []string
	scanner := bufio.NewScanner(bytes.NewReader(b.buf.Bytes()))
	for scanner.Scan() {
		var line struct {
			Msg string `json:"msg"`
		}
		if json.Unmarshal(scanner.Bytes(), &line) == nil && line.Msg != "" {
			messages = append(messages, line.Msg)
		}
	}
	return messages
}

type runningAgentF2 struct {
	cmd       *exec.Cmd
	output    *lockedBufferF2
	bufferDir string
	exited    chan struct{}
	exitErr   error
}

type agentSettingsF2 struct {
	collectionInterval string
	retryInterval      string
	seedBatch          []byte
}

func startAgentBinaryF2(t *testing.T, cloud *fakeCloudF2, settings agentSettingsF2) *runningAgentF2 {
	t.Helper()
	dir := t.TempDir()
	dmidecode := filepath.Join(dir, "dmidecode")
	require.NoError(t, os.WriteFile(dmidecode, []byte("#!/bin/sh\necho 6f1c1f0e-2a7e-4a55-9a3e-0c1f2a3b4c5d\n"), 0o755))
	bufferDir := filepath.Join(dir, "buffer")
	require.NoError(t, os.MkdirAll(bufferDir, 0o750))
	if settings.seedBatch != nil {
		require.NoError(t, os.WriteFile(filepath.Join(bufferDir, "00000000000000000000.batch"), settings.seedBatch, 0o640))
	}
	if settings.retryInterval == "" {
		settings.retryInterval = "5s"
	}

	cmd := exec.Command(agentBinaryF2)
	cmd.Env = append(os.Environ(),
		"SC_TEST_DMIDECODE="+dmidecode,
		"SC_TEST_CONFIG_YAML="+filepath.Join(dir, "config.yaml"),
		"SC_TEST_METADATA_URL="+cloud.metadata.URL,
		"SC_TEST_BUFFER_DIR="+bufferDir,
		"SC_TEST_COLLECTION_INTERVAL="+settings.collectionInterval,
		"SC_TEST_RETRY_INTERVAL="+settings.retryInterval,
	)
	output := &lockedBufferF2{}
	cmd.Stdout, cmd.Stderr = output, io.Discard
	require.NoError(t, cmd.Start())
	agent := &runningAgentF2{cmd: cmd, output: output, bufferDir: bufferDir, exited: make(chan struct{})}
	go func() {
		agent.exitErr = cmd.Wait()
		close(agent.exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-agent.exited
		if t.Failed() {
			t.Logf("agent output:\n%s", strings.Join(output.messages(), "\n"))
		}
	})
	return agent
}

func (a *runningAgentF2) waitForMessage(t *testing.T, message string, within time.Duration) {
	t.Helper()
	require.Eventually(t, func() bool {
		for _, logged := range a.output.messages() {
			if logged == message {
				return true
			}
		}
		return false
	}, within, 20*time.Millisecond, "the agent did not log %q", message)
}

func TestSendingSavedBatchesWithARefusedToken(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprintf("after a %d, the agent gets a new token at once and the next round sends with it", status), func(t *testing.T) {
			cloud := newFakeCloudF2(t, func(w http.ResponseWriter, attempt int) {
				if attempt == 1 {
					w.WriteHeader(status)
					return
				}
				w.WriteHeader(http.StatusOK)
			})
			agent := startAgentBinaryF2(t, cloud, agentSettingsF2{collectionInterval: "1500ms"})
			require.Eventually(t, func() bool { _, ingests, _ := cloud.snapshot(); return len(ingests) >= 2 }, 15*time.Second, 20*time.Millisecond)

			tokens, ingests, _ := cloud.snapshot()

			refusedAt := ingests[0].at
			var newTokenAt time.Time
			for _, issuedAt := range tokens {
				if issuedAt.After(refusedAt) {
					newTokenAt = issuedAt
					break
				}
			}
			require.False(t, newTokenAt.IsZero(), "no token was fetched after the refusal")
			assert.Less(t, newTokenAt.Sub(refusedAt), time.Second, "the new token should be fetched at once")
			assert.NotEqual(t, ingests[0].authorization, ingests[1].authorization)
			assert.Equal(t, fmt.Sprintf("Bearer token-%d", len(tokens)), ingests[1].authorization)
			agent.waitForMessage(t, "Drained buffered metric batches", 5*time.Second)
		})
	}
}
