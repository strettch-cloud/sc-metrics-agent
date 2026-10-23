//go:build lifecycle

// Package lifecycle installs, upgrades and removes the real .deb inside a
// container that runs systemd, the way a customer's VM does. It needs Docker,
// so it only runs with -tags lifecycle (the lifecycle CI job, one OS per run),
// not in `mise run test`.
//
// Inputs, all required:
//
//	LIFECYCLE_IMAGE         base image, e.g. mirror.gcr.io/library/debian:13
//	LIFECYCLE_DEB           the .deb under test
//	LIFECYCLE_PREVIOUS_DEB  the latest released .deb, to upgrade from
package lifecycle

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/strettch-cloud/sc-metrics-agent/test/helpers"
)

const (
	testVMID     = "6f1c2b3a-9d4e-4f5a-8b6c-7d8e9f0a1b2c"
	agentService = "sc-metrics-agent.service"
	updaterTimer = "sc-metrics-agent-update-scheduler.timer"
	pollEvery    = 500 * time.Millisecond
	// The unit restarts the agent 30 s after it dies (RestartSec=30).
	restartWait = 60 * time.Second
)

var (
	imageTag            string
	packagePath         string
	previousPackagePath string
)

func TestMain(m *testing.M) {
	baseImage := os.Getenv("LIFECYCLE_IMAGE")
	packagePath = os.Getenv("LIFECYCLE_DEB")
	previousPackagePath = os.Getenv("LIFECYCLE_PREVIOUS_DEB")
	if baseImage == "" || packagePath == "" || previousPackagePath == "" {
		fmt.Fprintln(os.Stderr, "LIFECYCLE_IMAGE, LIFECYCLE_DEB and LIFECYCLE_PREVIOUS_DEB must all be set")
		os.Exit(2)
	}
	var err error
	if packagePath, err = filepath.Abs(packagePath); err == nil {
		previousPackagePath, err = filepath.Abs(previousPackagePath)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	imageTag = "sc-metrics-agent-lifecycle:" + regexp.MustCompile(`[^a-z0-9.-]+`).ReplaceAllString(strings.ToLower(baseImage), "-")
	build := exec.Command("docker", "build", "--quiet", "--build-arg", "BASE_IMAGE="+baseImage, "--tag", imageTag, ".")
	if output, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "docker build failed: %v\n%s", err, output)
		os.Exit(2)
	}
	os.Exit(m.Run())
}

// fakeCloud stands in for everything the package talks to: the config
// repository post-install downloads agent.yaml from, the metadata service and
// the ingest API. Containers share the host's network, so they reach these
// servers on 127.0.0.1.
type fakeCloud struct {
	ingest    *helpers.MockIngestServer
	configURL string
	agentYAML string
}

// newFakeCloud serves an agent.yaml whose metadata address is metadataURL, or
// a working fake metadata service when metadataURL is empty.
func newFakeCloud(t *testing.T, metadataURL string) *fakeCloud {
	t.Helper()
	logger := zaptest.NewLogger(t)
	ingest := helpers.NewMockIngestServer(logger)
	t.Cleanup(ingest.Close)
	if metadataURL == "" {
		metadata := helpers.NewMockMetadataServer(ingest.BaseURL(), logger)
		t.Cleanup(metadata.Close)
		metadataURL = metadata.URL()
	}
	agentYAML := fmt.Sprintf("metadata_service:\n  base_url: %q\n", metadataURL)
	// download-config.sh fetches <repo>/<environment>/agent.yaml.
	config := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/production/agent.yaml" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(agentYAML))
	}))
	t.Cleanup(config.Close)
	return &fakeCloud{ingest: ingest, configURL: config.URL, agentYAML: agentYAML}
}

func (cloud *fakeCloud) metricsFromTestVM() int {
	count := 0
	for _, request := range cloud.ingest.GetRequests() {
		for _, metric := range request.ParsedMetrics {
			if metric.Labels["vm_id"] == testVMID {
				count++
			}
		}
	}
	return count
}

type container struct {
	name string
}

// startContainer boots a fresh container with systemd running and both
// packages mounted at /packages, and removes it when the test ends.
func startContainer(t *testing.T) *container {
	t.Helper()
	suffix := make([]byte, 4)
	_, _ = rand.Read(suffix)
	box := &container{name: "sc-lifecycle-" + hex.EncodeToString(suffix)}
	run := exec.Command("docker", "run", "--detach", "--name", box.name,
		// systemd as PID 1 needs to manage cgroups and mount tmpfs on /run.
		"--privileged", "--cgroupns=host", "--volume", "/sys/fs/cgroup:/sys/fs/cgroup:rw",
		"--tmpfs", "/run", "--tmpfs", "/run/lock",
		"--network", "host",
		"--volume", packagePath+":/packages/new.deb:ro",
		"--volume", previousPackagePath+":/packages/previous.deb:ro",
		imageTag)
	output, err := run.CombinedOutput()
	require.NoError(t, err, "docker run: %s", output)
	t.Cleanup(func() {
		if t.Failed() {
			journal, _ := box.exec(t, nil, "journalctl", "--no-pager", "--lines=100", "--unit="+agentService)
			t.Logf("journal of %s:\n%s", agentService, journal)
		}
		_ = exec.Command("docker", "rm", "--force", box.name).Run()
	})

	booted := assert.Eventually(t, func() bool {
		state, _ := box.exec(t, nil, "systemctl", "is-system-running")
		return state == "running" || state == "degraded"
	}, 60*time.Second, pollEvery)
	if !booted {
		// The container may have exited, which also takes its journal with it.
		logs, _ := exec.Command("docker", "logs", "--tail", "50", box.name).CombinedOutput()
		t.Fatalf("systemd did not finish booting; container output:\n%s", logs)
	}
	return box
}

// exec runs a command in the container and returns its trimmed output and
// exit code.
func (box *container) exec(t *testing.T, environment []string, command ...string) (string, int) {
	t.Helper()
	arguments := []string{"exec"}
	for _, variable := range environment {
		arguments = append(arguments, "--env", variable)
	}
	arguments = append(arguments, box.name)
	output, err := exec.Command("docker", append(arguments, command...)...).CombinedOutput()
	exitCode := 0
	if exitError, ok := err.(*exec.ExitError); ok {
		exitCode = exitError.ExitCode()
	} else {
		require.NoError(t, err)
	}
	return strings.TrimSpace(string(output)), exitCode
}

func (box *container) mustExec(t *testing.T, command ...string) string {
	t.Helper()
	output, exitCode := box.exec(t, nil, command...)
	require.Equal(t, 0, exitCode, "%s failed:\n%s", strings.Join(command, " "), output)
	return output
}

// configureAgent sets what a real VM provides another way: the VM ID, which
// dmidecode reads on a VM, and a short collection interval so the test doesn't
// wait 30 s. It uses a systemd drop-in rather than /etc/sc-metrics-agent, so
// the package still has to create that directory itself.
func (box *container) configureAgent(t *testing.T) {
	t.Helper()
	box.mustExec(t, "mkdir", "-p", "/etc/systemd/system/"+agentService+".d")
	box.mustExec(t, "sh", "-c", "printf '[Service]\\nEnvironment=SC_VM_ID="+testVMID+"\\nEnvironment=SC_COLLECTION_INTERVAL=2s\\n' > /etc/systemd/system/"+agentService+".d/lifecycle-test.conf")
}

// install installs a package the way an operator does, with post-install
// pointed at the fake config repository.
func (box *container) install(t *testing.T, cloud *fakeCloud, packageFile string, extraArguments ...string) {
	t.Helper()
	command := append([]string{"apt-get", "install", "--yes"}, extraArguments...)
	command = append(command, "/packages/"+packageFile)
	output, exitCode := box.exec(t, []string{"DEBIAN_FRONTEND=noninteractive", "SC_CONFIG_REPO_URL=" + cloud.configURL}, command...)
	require.Equal(t, 0, exitCode, "installing %s failed:\n%s", packageFile, output)
}

func (box *container) unitState(t *testing.T, unit string) (active string, enabled string) {
	t.Helper()
	active, _ = box.exec(t, nil, "systemctl", "is-active", unit)
	enabled, _ = box.exec(t, nil, "systemctl", "is-enabled", unit)
	return active, enabled
}

func (box *container) requireRunningAndEnabled(t *testing.T, unit string) {
	t.Helper()
	require.Eventually(t, func() bool {
		active, enabled := box.unitState(t, unit)
		return active == "active" && enabled == "enabled"
	}, 30*time.Second, pollEvery, "%s is not active and enabled", unit)
}

func (box *container) mainPID(t *testing.T) string {
	t.Helper()
	return box.mustExec(t, "systemctl", "show", "--property=MainPID", "--value", agentService)
}

func TestFreshInstallReinstallCrashAndRemove(t *testing.T) {
	t.Parallel()
	cloud := newFakeCloud(t, "")
	box := startContainer(t)
	box.configureAgent(t)
	box.install(t, cloud, "new.deb")

	t.Run("installs its files and directories", func(t *testing.T) {
		for _, executable := range []string{
			"/usr/bin/sc-metrics-agent",
			"/usr/bin/sc-metrics-agent-updater.sh",
			"/usr/lib/sc-metrics-agent/download-config.sh",
		} {
			box.mustExec(t, "test", "-x", executable)
		}
		box.mustExec(t, "test", "-s", "/usr/lib/sc-metrics-agent/apt-keyring.gpg")
		for directory, mode := range map[string]string{
			"/etc/sc-metrics-agent":     "755",
			"/var/lib/sc-metrics-agent": "750",
			"/var/run/sc-metrics-agent": "750",
		} {
			assert.Equal(t, mode, box.mustExec(t, "stat", "--format=%a", directory), directory)
		}
	})

	t.Run("downloads agent.yaml from the config repository", func(t *testing.T) {
		assert.Equal(t, strings.TrimSpace(cloud.agentYAML), box.mustExec(t, "cat", "/etc/strettchcloud/config/agent.yaml"))
	})

	// The updater validates this file after every update. It holds no metadata
	// address, and validation used to panic looking for one in it. SC_VM_ID
	// stands in for dmidecode, as in configureAgent.
	t.Run("validates the config the installer writes", func(t *testing.T) {
		output, exitCode := box.exec(t, []string{"SC_VM_ID=" + testVMID}, "sc-metrics-agent", "--validate-config", "/etc/sc-metrics-agent/config.yaml")
		assert.Equal(t, 0, exitCode, output)
	})

	t.Run("starts the agent and its update timer", func(t *testing.T) {
		box.requireRunningAndEnabled(t, agentService)
		box.requireRunningAndEnabled(t, updaterTimer)
	})

	t.Run("the running service sends metrics", func(t *testing.T) {
		require.Eventually(t, func() bool { return cloud.metricsFromTestVM() > 0 }, 60*time.Second, pollEvery)
	})

	t.Run("reinstalling the same version keeps it running", func(t *testing.T) {
		box.install(t, cloud, "new.deb", "--reinstall")
		box.requireRunningAndEnabled(t, agentService)
		box.requireRunningAndEnabled(t, updaterTimer)
	})

	t.Run("systemd restarts the agent after it is killed", func(t *testing.T) {
		killedPID := box.mainPID(t)
		box.mustExec(t, "kill", "-KILL", killedPID)
		require.Eventually(t, func() bool {
			active, _ := box.unitState(t, agentService)
			pid := box.mainPID(t)
			return active == "active" && pid != "0" && pid != killedPID
		}, restartWait, pollEvery)
	})

	t.Run("apt remove stops it and keeps the configuration", func(t *testing.T) {
		box.mustExec(t, "sh", "-c", "echo '# an operator edit apt remove must keep' >> /etc/sc-metrics-agent/config.yaml")
		agentConfigBefore := box.mustExec(t, "cat", "/etc/sc-metrics-agent/config.yaml")
		agentYAMLBefore := box.mustExec(t, "cat", "/etc/strettchcloud/config/agent.yaml")

		output, exitCode := box.exec(t, []string{"DEBIAN_FRONTEND=noninteractive"}, "apt-get", "remove", "--yes", "sc-metrics-agent")
		require.Equal(t, 0, exitCode, output)
		active, enabled := box.unitState(t, agentService)
		assert.NotEqual(t, "active", active)
		assert.NotEqual(t, "enabled", enabled)
		active, enabled = box.unitState(t, updaterTimer)
		assert.NotEqual(t, "active", active)
		assert.NotEqual(t, "enabled", enabled)
		_, exitCode = box.exec(t, nil, "test", "-e", "/usr/bin/sc-metrics-agent")
		assert.NotEqual(t, 0, exitCode, "the binary is still installed")
		assert.Equal(t, agentConfigBefore, box.mustExec(t, "cat", "/etc/sc-metrics-agent/config.yaml"))
		assert.Equal(t, agentYAMLBefore, box.mustExec(t, "cat", "/etc/strettchcloud/config/agent.yaml"))
	})
}

// During an image build (Packer, for example) no metadata service answers.
// The install must still succeed and leave the agent enabled for first boot.
func TestInstallSucceedsWithoutAMetadataService(t *testing.T) {
	t.Parallel()
	// Nothing listens on port 1, so every metadata request fails.
	cloud := newFakeCloud(t, "http://127.0.0.1:1")
	box := startContainer(t)
	box.configureAgent(t)

	box.install(t, cloud, "new.deb")

	_, enabled := box.unitState(t, agentService)
	assert.Equal(t, "enabled", enabled)
	_, enabled = box.unitState(t, updaterTimer)
	assert.Equal(t, "enabled", enabled)
}

func TestUpgradeFromTheLatestRelease(t *testing.T) {
	t.Parallel()
	cloud := newFakeCloud(t, "")
	box := startContainer(t)
	box.configureAgent(t)
	box.install(t, cloud, "previous.deb")
	box.requireRunningAndEnabled(t, agentService)
	previousPID := box.mainPID(t)

	box.install(t, cloud, "new.deb")

	newVersion := box.mustExec(t, "dpkg-deb", "--field", "/packages/new.deb", "Version")
	assert.Equal(t, newVersion, box.mustExec(t, "dpkg-query", "--show", "--showformat=${Version}", "sc-metrics-agent"))
	box.requireRunningAndEnabled(t, agentService)
	box.requireRunningAndEnabled(t, updaterTimer)

	// dpkg replaced the binary, so a process still running the old one shows
	// its executable as "(deleted)".
	pid := box.mainPID(t)
	assert.NotEqual(t, previousPID, pid, "the agent was not restarted")
	assert.Equal(t, "/usr/bin/sc-metrics-agent", box.mustExec(t, "readlink", "/proc/"+pid+"/exe"))
	assert.Equal(t, strings.TrimSpace(cloud.agentYAML), box.mustExec(t, "cat", "/etc/strettchcloud/config/agent.yaml"))

	cloud.ingest.ClearRequests()
	require.Eventually(t, func() bool { return cloud.metricsFromTestVM() > 0 }, 60*time.Second, pollEvery,
		"the upgraded agent sends no metrics")
}
