// Package flows runs the agent binary, built with the e2e tag, against fake
// metadata and resource-manager servers.
package flows

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/strettch-cloud/sc-metrics-agent/test/helpers"
	"go.uber.org/zap"
)

const (
	builtVersion   = "9.8.7"
	builtCommit    = "c0ffee1"
	builtBuildTime = "2026-01-02_03:04:05"
	vmUUID         = "6f1c2a52-8d0e-4b8e-9a51-3c1f0e7d2b44"
	issuedToken    = "token-from-metadata"
)

var agentBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agent-flow-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	agentBinary = filepath.Join(dir, "sc-metrics-agent")
	ldflags := fmt.Sprintf("-X main.version=%s -X main.commit=%s -X main.buildTime=%s", builtVersion, builtCommit, builtBuildTime)
	build := exec.Command("go", "build", "-tags", "e2e", "-ldflags", ldflags, "-o", agentBinary, "github.com/strettch-cloud/sc-metrics-agent/cmd/agent")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building the agent: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// fakeMetadata stands in for the metadata service. answer decides the reply
// to the n-th token request (n counts from 1).
type fakeMetadata struct {
	url     string
	mu      sync.Mutex
	times   []time.Time
	flavors []string
	answer  func(n int, w http.ResponseWriter)
}

func newFakeMetadata(t *testing.T, answer func(n int, w http.ResponseWriter)) *fakeMetadata {
	m := &fakeMetadata{answer: answer}
	server := helpers.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.times = append(m.times, time.Now())
		m.flavors = append(m.flavors, r.Header.Get("Metadata-Flavor"))
		n := len(m.times)
		m.mu.Unlock()
		m.answer(n, w)
	}))
	t.Cleanup(server.Close)
	m.url = server.URL
	return m
}

func (m *fakeMetadata) requests() ([]time.Time, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]time.Time(nil), m.times...), append([]string(nil), m.flavors...)
}

func writeToken(w http.ResponseWriter, token, apiURL string, expiresIn time.Duration) {
	body := map[string]string{"token": token, "expiresAt": time.Now().Add(expiresIn).UTC().Format(time.RFC3339)}
	if apiURL != "" {
		body["cloudAPIUrl"] = apiURL
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func answerToken(apiURL string) func(int, http.ResponseWriter) {
	return func(_ int, w http.ResponseWriter) { writeToken(w, issuedToken, apiURL, time.Hour) }
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type agentProcess struct {
	cmd      *exec.Cmd
	stdout   *lockedBuffer
	combined *lockedBuffer
	exited   chan struct{}
}

func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dmidecode")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755))
	return path
}

// agentEnv is what every run starts from: fake servers, a temporary buffer,
// short intervals, no config.yaml, and a dmidecode that gives a VM ID.
func agentEnv(t *testing.T, metadataURL string) map[string]string {
	return map[string]string{
		"PATH":                        os.Getenv("PATH"),
		"SC_TEST_METADATA_URL":        metadataURL,
		"SC_TEST_BUFFER_DIR":          filepath.Join(t.TempDir(), "buffer"),
		"SC_TEST_COLLECTION_INTERVAL": "200ms",
		"SC_TEST_HTTP_TIMEOUT":        "2s",
		"SC_TEST_RETRY_INTERVAL":      "50ms",
		"SC_TEST_MAX_RETRIES":         "0",
		"SC_TEST_CONFIG_YAML":         filepath.Join(t.TempDir(), "absent.yaml"),
		"SC_TEST_DMIDECODE":           writeScript(t, "echo "+vmUUID),
	}
}

func startAgent(t *testing.T, env map[string]string, args ...string) *agentProcess {
	t.Helper()
	cmd := exec.Command(agentBinary, args...)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	a := &agentProcess{cmd: cmd, stdout: &lockedBuffer{}, combined: &lockedBuffer{}, exited: make(chan struct{})}
	cmd.Stdout = io.MultiWriter(a.stdout, a.combined)
	cmd.Stderr = a.combined
	require.NoError(t, cmd.Start())
	go func() {
		_ = cmd.Wait()
		close(a.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-a.exited:
		default:
			_ = cmd.Process.Kill()
			<-a.exited
		}
	})
	return a
}

func (a *agentProcess) waitExit(t *testing.T, timeout time.Duration) int {
	t.Helper()
	select {
	case <-a.exited:
	case <-time.After(timeout):
		t.Fatalf("the agent did not exit within %s; output:\n%s", timeout, a.combined.String())
	}
	return a.cmd.ProcessState.ExitCode()
}

func (a *agentProcess) stop(t *testing.T) {
	t.Helper()
	select {
	case <-a.exited:
		return
	default:
	}
	_ = a.cmd.Process.Signal(syscall.SIGTERM)
	a.waitExit(t, 15*time.Second)
}

func (a *agentProcess) hasExited() bool {
	select {
	case <-a.exited:
		return true
	default:
		return false
	}
}

// logLines are the agent's JSON log lines; other output is skipped.
func (a *agentProcess) logLines() []map[string]any {
	var lines []map[string]any
	scanner := bufio.NewScanner(strings.NewReader(a.stdout.String()))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		var line map[string]any
		if json.Unmarshal(scanner.Bytes(), &line) == nil {
			lines = append(lines, line)
		}
	}
	return lines
}

// waitUntil fails the test when the condition is still false at the timeout,
// or when the agent exited first.
func waitUntil(t *testing.T, a *agentProcess, timeout time.Duration, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		if a.hasExited() {
			t.Fatalf("the agent exited before %s; output:\n%s", what, a.combined.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; output:\n%s", what, a.combined.String())
}

func newIngest(t *testing.T) *helpers.MockIngestServer {
	ingest := helpers.NewMockIngestServer(zap.NewNop())
	t.Cleanup(ingest.Close)
	return ingest
}

// fakeResourceManager records each metrics send; status decides the answer
// to the n-th send (n counts from 1).
type fakeResourceManager struct {
	url    string
	mu     sync.Mutex
	sends  []recordedSend
	status func(n int) int
}

type recordedSend struct {
	at            time.Time
	authorization string
}

func newFakeResourceManager(t *testing.T, status func(n int) int) *fakeResourceManager {
	rm := &fakeResourceManager{status: status}
	server := helpers.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/metrics/ingest") {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		rm.mu.Lock()
		rm.sends = append(rm.sends, recordedSend{at: time.Now(), authorization: r.Header.Get("Authorization")})
		n := len(rm.sends)
		rm.mu.Unlock()
		w.WriteHeader(rm.status(n))
	}))
	t.Cleanup(server.Close)
	rm.url = server.URL
	return rm
}

func (rm *fakeResourceManager) received() []recordedSend {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	return append([]recordedSend(nil), rm.sends...)
}

func (rm *fakeResourceManager) sentWith(token string) bool {
	for _, send := range rm.received() {
		if send.authorization == "Bearer "+token {
			return true
		}
	}
	return false
}

func accepted(int) int { return http.StatusAccepted }

func TestGettingTheTokenFlow(t *testing.T) {
	t.Run("at start the agent asks the metadata service for a token with Metadata-Flavor StrettchCloud", func(t *testing.T) {
		ingest := newIngest(t)
		metadata := newFakeMetadata(t, answerToken(ingest.BaseURL()))
		env := agentEnv(t, metadata.url)
		env["SC_TEST_COLLECTION_INTERVAL"] = "1h"
		agent := startAgent(t, env)

		waitUntil(t, agent, 15*time.Second, "a token request", func() bool {
			times, _ := metadata.requests()
			return len(times) > 0
		})
		agent.stop(t)

		_, flavors := metadata.requests()
		require.Len(t, flavors, 1)
		assert.Equal(t, "StrettchCloud", flavors[0])
	})

	t.Run("the agent asks for a new token 1 minute before the token's expiresAt, and sends with the new token", func(t *testing.T) {
		const secondsPastTheMinute = 64 // the new token is due about 4 s after start
		rm := newFakeResourceManager(t, accepted)
		metadata := newFakeMetadata(t, func(n int, w http.ResponseWriter) {
			if n == 1 {
				writeToken(w, "first-token", rm.url, secondsPastTheMinute*time.Second)
				return
			}
			writeToken(w, "second-token", rm.url, time.Hour)
		})
		agent := startAgent(t, agentEnv(t, metadata.url))

		waitUntil(t, agent, 20*time.Second, "a send with the second token", func() bool { return rm.sentWith("second-token") })
		agent.stop(t)

		times, _ := metadata.requests()
		require.GreaterOrEqual(t, len(times), 2)
		wait := times[1].Sub(times[0])
		assert.GreaterOrEqual(t, wait, 2500*time.Millisecond, "asked too early")
		assert.LessOrEqual(t, wait, 8*time.Second, "asked too late")
		assert.True(t, rm.sentWith("first-token"), "the first token is used until then")
	})

	for _, refusal := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprintf("after resource-manager answers %d the agent asks for a new token and sends with it", refusal), func(t *testing.T) {
			rm := newFakeResourceManager(t, func(n int) int {
				if n == 1 {
					return refusal
				}
				return http.StatusAccepted
			})
			metadata := newFakeMetadata(t, func(n int, w http.ResponseWriter) {
				writeToken(w, fmt.Sprintf("token-%d", n), rm.url, time.Hour)
			})
			agent := startAgent(t, agentEnv(t, metadata.url))

			waitUntil(t, agent, 20*time.Second, "a send with a second token", func() bool { return rm.sentWith("token-2") })
			agent.stop(t)

			assert.Equal(t, "Bearer token-1", rm.received()[0].authorization)
		})
	}

	t.Run("a token request that fails keeps the token and API address the agent had, and the agent asks again at the next round", func(t *testing.T) {
		rm := newFakeResourceManager(t, accepted)
		metadata := newFakeMetadata(t, func(n int, w http.ResponseWriter) {
			switch n {
			case 1:
				writeToken(w, "first-token", rm.url, 62*time.Second)
			case 2:
				http.Error(w, "unavailable", http.StatusInternalServerError)
			default:
				writeToken(w, "second-token", rm.url, time.Hour)
			}
		})
		agent := startAgent(t, agentEnv(t, metadata.url))

		waitUntil(t, agent, 20*time.Second, "a send with the second token", func() bool { return rm.sentWith("second-token") })
		agent.stop(t)

		times, _ := metadata.requests()
		require.GreaterOrEqual(t, len(times), 3)
		assert.Less(t, times[2].Sub(times[1]), 1500*time.Millisecond, "the next round, 200 ms later, asks again")
		for _, send := range rm.received() {
			if send.authorization == "Bearer second-token" {
				break
			}
			assert.Equal(t, "Bearer first-token", send.authorization, "until the second token, every send keeps the first token")
		}
	})

	t.Run("a token answer without the API address makes the agent use the new token at the API address it had, and log CloudAPI URL not available", func(t *testing.T) {
		rm := newFakeResourceManager(t, accepted)
		metadata := newFakeMetadata(t, func(n int, w http.ResponseWriter) {
			if n == 1 {
				writeToken(w, "first-token", rm.url, 62*time.Second)
				return
			}
			writeToken(w, "second-token", "", time.Hour)
		})
		agent := startAgent(t, agentEnv(t, metadata.url))

		waitUntil(t, agent, 20*time.Second, "a send with the second token", func() bool { return rm.sentWith("second-token") })
		agent.stop(t)

		assert.Contains(t, agent.combined.String(), "CloudAPI URL not available in metadata response")
	})

	t.Run("at start, a metadata service that fails makes the agent log the reason and ask again at the next round", func(t *testing.T) {
		rm := newFakeResourceManager(t, accepted)
		metadata := newFakeMetadata(t, func(n int, w http.ResponseWriter) {
			if n == 1 {
				http.Error(w, "unavailable", http.StatusInternalServerError)
				return
			}
			writeToken(w, issuedToken, rm.url, time.Hour)
		})
		agent := startAgent(t, agentEnv(t, metadata.url))

		waitUntil(t, agent, 20*time.Second, "a send with the token", func() bool { return rm.sentWith(issuedToken) })
		agent.stop(t)

		var reasons int
		for _, line := range agent.logLines() {
			if (line["level"] == "warn" || line["level"] == "error") && line["error"] != nil {
				reasons++
			}
		}
		assert.Positive(t, reasons)
		times, _ := metadata.requests()
		require.GreaterOrEqual(t, len(times), 2)
		assert.Less(t, times[1].Sub(times[0]), 1500*time.Millisecond)
	})
}
