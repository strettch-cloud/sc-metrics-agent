package test

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

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
		// The e2e tag adds the SC_TEST_* overrides, which release builds lack.
		arguments := []string{"build", "-tags", "e2e", "-ldflags", "-X main.version=" + testVersion, "-o", agentBinaryPath}
		if agentCoverageDirectory() != "" {
			arguments = append(arguments, "-cover", "-coverpkg=github.com/strettch-cloud/sc-metrics-agent/...")
		}
		build := exec.Command("go", append(arguments, "../cmd/agent")...)
		if output, err := build.CombinedOutput(); err != nil {
			agentBinaryError = fmt.Errorf("go build failed: %w\n%s", err, output)
		}
	})
	require.NoError(t, agentBinaryError)
	return agentBinaryPath
}

// agentCoverageDirectory is where the agents these tests start write their
// coverage. `mise run test` sets AGENT_COVERAGE_DIR and adds that coverage to
// the total, because Go counts only code run inside the test process itself.
// Empty builds the agent without coverage.
//
// It is read here, during a test, never when the package loads: only then does
// go test see the variable, and rerun these tests for a new directory instead
// of reusing a cached result that wrote no coverage.
func agentCoverageDirectory() string {
	return os.Getenv("AGENT_COVERAGE_DIR")
}

// withAgentCoverage tells an agent started with this environment where to
// write its coverage.
func withAgentCoverage(environment []string) []string {
	if agentCoverageDirectory() == "" {
		return environment
	}
	return append(environment, "GOCOVERDIR="+agentCoverageDirectory())
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

// issuedBearerHeaders lists the Authorization headers the agent may send: one
// per token the metadata service handed out.
func (cloud *fakeCloud) issuedBearerHeaders() []string {
	var headers []string
	for _, token := range cloud.metadata.IssuedTokens() {
		headers = append(headers, "Bearer "+token)
	}
	return headers
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
	// withoutAgentYAML leaves out agent.yaml, as when the installer couldn't
	// write it.
	withoutAgentYAML bool
	// environment is added to the agent's settings, after them.
	environment []string
	arguments   []string
}

// startAgent runs the agent with only the settings a test gives it, so the
// machine's own environment and /etc can't change the result.
func startAgent(t *testing.T, cloud *fakeCloud, options agentOptions) *agentProcess {
	t.Helper()
	workDirectory := t.TempDir()
	if options.bufferDir == "" {
		options.bufferDir = filepath.Join(workDirectory, "buffer")
	}
	agentYAMLPath := filepath.Join(workDirectory, "agent.yaml")
	if !options.withoutAgentYAML {
		agentYAML := fmt.Sprintf("metadata_service:\n  base_url: %q\n", cloud.metadata.URL())
		require.NoError(t, os.WriteFile(agentYAMLPath, []byte(agentYAML), 0o600))
	}
	// dmidecode stands in for the VM's firmware: it prints the VM ID, or
	// nothing when the test has none.
	dmidecodePath := filepath.Join(workDirectory, "dmidecode")
	require.NoError(t, os.WriteFile(dmidecodePath, []byte("#!/bin/sh\nprintf '%s\\n' '"+options.vmID+"'\n"), 0o700))

	environment := []string{
		"PATH=" + os.Getenv("PATH"),
		"SC_TEST_AGENT_YAML=" + agentYAMLPath,
		// Missing, so the machine's own config.yaml can't change the result.
		"SC_TEST_CONFIG_YAML=" + filepath.Join(workDirectory, "config.yaml"),
		"SC_TEST_DMIDECODE=" + dmidecodePath,
		"SC_TEST_BUFFER_DIR=" + options.bufferDir,
		"SC_TEST_COLLECTION_INTERVAL=" + collectionInterval.String(),
		"SC_TEST_HTTP_TIMEOUT=2s",
		// max_retries 0 means "retry forever" (SC-586), so use 1.
		"SC_TEST_MAX_RETRIES=1",
		"SC_TEST_RETRY_INTERVAL=100ms",
	}
	environment = append(environment, options.environment...)

	logPath := filepath.Join(workDirectory, "agent.log")
	logFile, err := os.Create(logPath)
	require.NoError(t, err)
	command := exec.Command(agentBinary(t), options.arguments...)
	command.Env = withAgentCoverage(environment)
	// Through a pipe, not the file itself: under systemd the agent writes to a
	// socket, and a file hides errors that only a pipe or a socket causes.
	command.Stdout = io.MultiWriter(logFile)
	command.Stderr = io.MultiWriter(logFile)
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
			// SIGTERM first: an agent killed outright writes no coverage.
			_ = command.Process.Signal(syscall.SIGTERM)
			select {
			case <-agent.exited:
			case <-time.After(waitForAgent):
				_ = command.Process.Kill()
				<-agent.exited
			}
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
	assert.Contains(t, cloud.issuedBearerHeaders(), request.Authorization,
		"the agent must send the token the metadata service gave it")
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
	assert.Contains(t, cloud.issuedBearerHeaders(), heartbeat.Authorization)
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
	assert.NotContains(t, agent.log(t), "Failed to sync logger")
}

// Between attempts to send a batch the agent waits 5 seconds, or as long as
// the ingest's Retry-After asks. SIGTERM must not wait for that, or systemd
// kills the agent after 90 seconds. The batch stays buffered for the next run.
func TestAgentStopsPromptlyWhileWaitingToRetryASend(t *testing.T) {
	t.Parallel()
	cloud := newFakeCloud(t)
	bufferDir := filepath.Join(t.TempDir(), "buffer")
	cloud.ingest.SetUnavailable(true)
	waitingAnHour := startAgent(t, cloud, agentOptions{
		vmID: testVMID, bufferDir: bufferDir, environment: []string{"SC_TEST_RETRY_INTERVAL=1h"},
	})
	require.Eventually(t, func() bool { return cloud.ingest.RejectedIngestCount() >= 1 }, waitForAgent, pollEvery)
	stopRequested := time.Now()

	require.Equal(t, 0, waitingAnHour.stop(t))
	assert.Contains(t, waitingAnHour.log(t), "Agent shutdown complete")

	cloud.ingest.SetUnavailable(false)
	startAgent(t, cloud, agentOptions{vmID: testVMID, bufferDir: bufferDir})
	require.Eventually(t, func() bool {
		return metricsCollectedBefore(cloud.metricsReceived(), stopRequested) > 0
	}, waitForAgent, pollEvery, "the batch the agent was sending when it stopped was never delivered")
}

func TestAgentRefusesToStartWithoutAVMID(t *testing.T) {
	t.Parallel()
	cloud := newFakeCloud(t)
	agent := startAgent(t, cloud, agentOptions{})

	// dmidecode is given up to 5 s before the agent gives up on it.
	assert.NotEqual(t, 0, agent.waitForExit(t, 10*time.Second))
	assert.Contains(t, agent.log(t), "vm_id cannot be determined")
	assert.Empty(t, cloud.ingest.GetRequests())
}

func TestAgentPrintsTheVersionItWasBuiltWith(t *testing.T) {
	t.Parallel()
	command := exec.Command(agentBinary(t), "-v")
	command.Env = withAgentCoverage(os.Environ())

	output, err := command.CombinedOutput()

	require.NoError(t, err)
	assert.Contains(t, string(output), "Version: "+testVersion)
}

func TestValidateConfigChecksTheGivenConfigYAML(t *testing.T) {
	t.Parallel()
	for name, testCase := range map[string]struct {
		configYAML string
		exitCode   int
		output     string
	}{
		"only the log level": {"log_level: debug\n", 0, "Configuration is valid"},
		"another setting":    {"collection_interval: 1ms\n", 1, "may only set log_level"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			configYAML := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, os.WriteFile(configYAML, []byte(testCase.configYAML), 0o600))
			agent := startAgent(t, newFakeCloud(t), agentOptions{vmID: testVMID, arguments: []string{"--validate-config", configYAML}})

			assert.Equal(t, testCase.exitCode, agent.waitForExit(t, waitForAgent))
			assert.Contains(t, agent.log(t), testCase.output)
		})
	}
}

// Without agent.yaml, which the installer writes, the agent can't find the
// metadata service, so it exits and says which file is missing.
func TestAgentExitsWithoutAgentYAML(t *testing.T) {
	t.Parallel()
	cloud := newFakeCloud(t)
	agent := startAgent(t, cloud, agentOptions{vmID: testVMID, withoutAgentYAML: true})

	assert.Equal(t, 1, agent.waitForExit(t, waitForAgent))
	assert.Contains(t, agent.log(t), "Failed to load configuration")
	assert.Contains(t, agent.log(t), "agent.yaml")
	assert.Empty(t, cloud.metadata.IssuedTokens())
}

func TestAgentExitsWhenItCantCreateItsBuffer(t *testing.T) {
	t.Parallel()
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	agent := startAgent(t, newFakeCloud(t), agentOptions{vmID: testVMID, bufferDir: filepath.Join(file, "buffer")})

	assert.Equal(t, 1, agent.waitForExit(t, waitForAgent))
	assert.Contains(t, agent.log(t), "Failed to open disk buffer")
}

func TestSCLogLevelDebugWritesDebugLines(t *testing.T) {
	t.Parallel()
	cloud := newFakeCloud(t)
	agent := startAgent(t, cloud, agentOptions{vmID: testVMID, environment: []string{"SC_LOG_LEVEL=debug"}})

	require.Eventually(t, func() bool { return len(cloud.metricsReceived()) > 0 }, waitForAgent, pollEvery)
	assert.Contains(t, agent.log(t), `"level":"debug"`)
}
