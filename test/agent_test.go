package test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/strettch-cloud/sc-metrics-agent/test/helpers"
)

// These tests run the real agent binary as a separate process against fake
// metadata and ingest servers, the way systemd runs it on a customer's VM. They
// check what the agent sends, not how its packages are wired, so they keep
// passing through refactors that don't change behaviour.

const (
	testVMID    = "6f1c2b3a-9d4e-4f5a-8b6c-7d8e9f0a1b2c"
	testVersion = "1.2.3-e2e"
	// The agent collects every collectionInterval; tests wait several of them.
	collectionInterval = time.Second
	waitForAgent       = 15 * time.Second
	pollEvery          = 100 * time.Millisecond
)

var (
	buildDirectory   string
	agentBinaryOnce  sync.Once
	agentBinaryPath  string
	agentBinaryError error
)

func TestMain(m *testing.M) {
	directory, err := os.MkdirTemp("", "sc-metrics-agent-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	buildDirectory = directory
	code := m.Run()
	_ = os.RemoveAll(buildDirectory)
	os.Exit(code)
}

// agentBinary builds the agent once per test run, on first use.
func agentBinary(t *testing.T) string {
	t.Helper()
	agentBinaryOnce.Do(func() {
		agentBinaryPath = filepath.Join(buildDirectory, "sc-metrics-agent")
		build := exec.Command("go", "build", "-ldflags", "-X main.version="+testVersion, "-o", agentBinaryPath, "../cmd/agent")
		if output, err := build.CombinedOutput(); err != nil {
			agentBinaryError = fmt.Errorf("go build failed: %w\n%s", err, output)
		}
	})
	require.NoError(t, agentBinaryError)
	return agentBinaryPath
}

type fakeCloud struct {
	ingest   *helpers.MockIngestServer
	metadata *helpers.MockMetadataServer
}

func newFakeCloud(t *testing.T) *fakeCloud {
	t.Helper()
	logger := zaptest.NewLogger(t)
	ingest := helpers.NewMockIngestServer(logger)
	t.Cleanup(ingest.Close)
	metadata := helpers.NewMockMetadataServer(ingest.BaseURL(), logger)
	t.Cleanup(metadata.Close)
	return &fakeCloud{ingest: ingest, metadata: metadata}
}

// metricsReceived returns every metric the ingest server accepted.
func (cloud *fakeCloud) metricsReceived() []helpers.TimeseriesMetric {
	var metrics []helpers.TimeseriesMetric
	for _, request := range cloud.ingest.GetRequests() {
		metrics = append(metrics, request.ParsedMetrics...)
	}
	return metrics
}

type agentProcess struct {
	command *exec.Cmd
	logPath string
	exited  chan struct{}
	// exitError is written once, before exited is closed.
	exitError error
}

type agentOptions struct {
	vmID      string
	bufferDir string
}

// startAgent runs the agent with only the settings a test gives it, so the
// machine's own environment and /etc can't change the result.
func startAgent(t *testing.T, cloud *fakeCloud, options agentOptions) *agentProcess {
	t.Helper()
	workDirectory := t.TempDir()
	if options.bufferDir == "" {
		options.bufferDir = filepath.Join(workDirectory, "buffer")
	}
	// One file holds both agent.yaml's metadata address and the agent's own
	// settings, because the agent reads both from SC_AGENT_CONFIG.
	configPath := filepath.Join(workDirectory, "config.yaml")
	config := fmt.Sprintf("metadata_service:\n  base_url: %q\nbuffer_dir: %q\n", cloud.metadata.URL(), options.bufferDir)
	require.NoError(t, os.WriteFile(configPath, []byte(config), 0o600))

	environment := []string{
		"PATH=" + os.Getenv("PATH"),
		"SC_AGENT_CONFIG=" + configPath,
		"SC_COLLECTION_INTERVAL=" + collectionInterval.String(),
		"SC_HTTP_TIMEOUT=2s",
		// max_retries 0 means "retry forever" (SC-586), so use 1.
		"SC_MAX_RETRIES=1",
		"SC_RETRY_INTERVAL=100ms",
	}
	if options.vmID != "" {
		environment = append(environment, "SC_VM_ID="+options.vmID)
	}

	logPath := filepath.Join(workDirectory, "agent.log")
	logFile, err := os.Create(logPath)
	require.NoError(t, err)
	command := exec.Command(agentBinary(t))
	command.Env = environment
	command.Stdout = logFile
	command.Stderr = logFile
	require.NoError(t, command.Start())

	agent := &agentProcess{command: command, logPath: logPath, exited: make(chan struct{})}
	go func() {
		agent.exitError = command.Wait()
		_ = logFile.Close()
		close(agent.exited)
	}()
	t.Cleanup(func() {
		select {
		case <-agent.exited:
		default:
			_ = command.Process.Kill()
			<-agent.exited
		}
		if t.Failed() {
			t.Logf("agent log:\n%s", agent.log(t))
		}
	})
	return agent
}

func (agent *agentProcess) log(t *testing.T) string {
	t.Helper()
	content, err := os.ReadFile(agent.logPath)
	require.NoError(t, err)
	return string(content)
}

// waitForExit returns the exit code, or fails the test if the agent is still
// running after timeout.
func (agent *agentProcess) waitForExit(t *testing.T, timeout time.Duration) int {
	t.Helper()
	select {
	case <-agent.exited:
	case <-time.After(timeout):
		t.Fatalf("agent still running after %s", timeout)
	}
	var exitError *exec.ExitError
	if errors.As(agent.exitError, &exitError) {
		return exitError.ExitCode()
	}
	require.NoError(t, agent.exitError)
	return 0
}

func (agent *agentProcess) stop(t *testing.T) int {
	t.Helper()
	require.NoError(t, agent.command.Process.Signal(syscall.SIGTERM))
	return agent.waitForExit(t, 5*time.Second)
}

func TestAgentSendsMetricsTheResourceManagerAccepts(t *testing.T) {
	t.Parallel()
	cloud := newFakeCloud(t)
	startAgent(t, cloud, agentOptions{vmID: testVMID})

	require.Eventually(t, func() bool { return len(cloud.metricsReceived()) > 0 }, waitForAgent, pollEvery)

	request := cloud.ingest.GetRequests()[0]
	assert.True(t, strings.HasPrefix(request.Authorization, "Bearer mock-test-token-"),
		"the agent must send the token the metadata service gave it, got %q", request.Authorization)
	assert.Empty(t, cloud.ingest.ValidateRequest(request), "every metric must pass the resource manager's checks")

	sentNames := map[string]bool{}
	for _, metric := range request.ParsedMetrics {
		assert.Equal(t, testVMID, metric.Labels["vm_id"], "metric %s", metric.Name)
		sentNames[metric.Name] = true
	}
	// Disk I/O isn't required: it is only sent for disks whose mount source
	// matches a /proc/diskstats name, which depends on the machine (SC-586).
	for _, name := range []string{
		"node_cpu_seconds_total",
		"node_memory_MemTotal_bytes",
		"node_load1",
		"node_network_receive_bytes_total",
		"node_filesystem_size_bytes",
	} {
		assert.True(t, sentNames[name], "the first batch has no %s", name)
	}
}

func TestAgentSendsAHeartbeatWhenItStarts(t *testing.T) {
	t.Parallel()
	cloud := newFakeCloud(t)
	startAgent(t, cloud, agentOptions{vmID: testVMID})

	require.Eventually(t, func() bool { return len(cloud.ingest.GetHeartbeats()) > 0 }, waitForAgent, pollEvery)

	heartbeat := cloud.ingest.GetHeartbeats()[0]
	assert.True(t, strings.HasPrefix(heartbeat.Authorization, "Bearer mock-test-token-"))
	assert.Contains(t, string(heartbeat.Body), `"agentType":"SC_METRICS_AGENT"`)
}

// metricsCollectedBefore counts the received metrics collected before moment.
func metricsCollectedBefore(metrics []helpers.TimeseriesMetric, moment time.Time) int {
	count := 0
	for _, metric := range metrics {
		if metric.Timestamp < moment.UnixMilli() {
			count++
		}
	}
	return count
}

func TestAgentDeliversMetricsCollectedDuringAnIngestOutage(t *testing.T) {
	t.Parallel()
	cloud := newFakeCloud(t)
	cloud.ingest.SetUnavailable(true)
	startAgent(t, cloud, agentOptions{vmID: testVMID})

	require.Eventually(t, func() bool { return cloud.ingest.RejectedIngestCount() >= 1 }, waitForAgent, pollEvery)
	refusedBatchesCollected := time.Now()
	// The agent retries a failed send once, 100 ms later. Staying down longer
	// makes that retry fail too, so these metrics can only arrive from the buffer.
	time.Sleep(2 * collectionInterval)
	cloud.ingest.SetUnavailable(false)

	require.Eventually(t, func() bool {
		return metricsCollectedBefore(cloud.metricsReceived(), refusedBatchesCollected) > 0
	}, waitForAgent, pollEvery, "metrics collected during the outage were never delivered")
}

func TestAgentKeepsBufferedMetricsAcrossARestart(t *testing.T) {
	t.Parallel()
	cloud := newFakeCloud(t)
	bufferDir := filepath.Join(t.TempDir(), "buffer")
	cloud.ingest.SetUnavailable(true)
	firstRun := startAgent(t, cloud, agentOptions{vmID: testVMID, bufferDir: bufferDir})
	require.Eventually(t, func() bool { return cloud.ingest.RejectedIngestCount() >= 1 }, waitForAgent, pollEvery)
	require.Equal(t, 0, firstRun.stop(t))

	secondRunStarted := time.Now()
	cloud.ingest.SetUnavailable(false)
	startAgent(t, cloud, agentOptions{vmID: testVMID, bufferDir: bufferDir})

	require.Eventually(t, func() bool {
		return metricsCollectedBefore(cloud.metricsReceived(), secondRunStarted) > 0
	}, waitForAgent, pollEvery, "metrics buffered before the restart were never delivered")
}

func TestAgentStopsCleanlyOnSIGTERM(t *testing.T) {
	t.Parallel()
	cloud := newFakeCloud(t)
	agent := startAgent(t, cloud, agentOptions{vmID: testVMID})
	require.Eventually(t, func() bool { return len(cloud.metricsReceived()) > 0 }, waitForAgent, pollEvery)

	assert.Equal(t, 0, agent.stop(t))
	assert.Contains(t, agent.log(t), "Agent shutdown complete")
}

func TestAgentRefusesToStartWithoutAVMID(t *testing.T) {
	t.Parallel()
	if dmidecodeFindsAVMID() {
		t.Skip("dmidecode reports this machine's UUID, so a missing VM ID can't be set up here")
	}
	cloud := newFakeCloud(t)
	agent := startAgent(t, cloud, agentOptions{})

	// dmidecode is given up to 5 s before the agent gives up on it.
	assert.NotEqual(t, 0, agent.waitForExit(t, 10*time.Second))
	assert.Contains(t, agent.log(t), "vm_id cannot be determined")
	assert.Empty(t, cloud.ingest.GetRequests())
}

// dmidecodeFindsAVMID tries the same dmidecode paths the agent tries.
func dmidecodeFindsAVMID() bool {
	for _, dmidecode := range []string{"/usr/sbin/dmidecode", "/sbin/dmidecode", "dmidecode"} {
		output, err := exec.Command(dmidecode, "-s", "system-uuid").Output()
		if err != nil {
			continue
		}
		if _, err := uuid.Parse(strings.TrimSpace(string(output))); err == nil {
			return true
		}
	}
	return false
}

func TestAgentPrintsTheVersionItWasBuiltWith(t *testing.T) {
	t.Parallel()
	output, err := exec.Command(agentBinary(t), "-v").CombinedOutput()

	require.NoError(t, err)
	assert.Contains(t, string(output), "Version: "+testVersion)
}
