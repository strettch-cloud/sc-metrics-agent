//go:build lifecycle

package lifecycle

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests run the real auto-updater against a signed APT repository
// inside the container, so apt checks the repository's signature exactly as
// it does against repo.cloud.strettch.com.
//
// The updater that runs is the one installed, as on a customer's VM. So the
// systemd test installs the latest release and upgrades to this build, the path
// VMs take when this build ships; the others install this build and upgrade to
// a copy of it with a higher version, so they test this build's updater.

const (
	updaterService = "sc-metrics-agent-update-scheduler.service"
	repositoryPath = "/srv/apt"
	// The keyring the agent's APT source trusts. The untrusted key signs a
	// repository the updater must refuse.
	trustedKeyring   = "/usr/share/keyrings/sc-metrics-agent-test.gpg"
	trustedGPGHome   = "/root/gnupg-trusted"
	untrustedGPGHome = "/root/gnupg-untrusted"
	// The updater waits a random 0-59 s before starting and 30 s between
	// retries. This sleep, first on PATH, caps every wait at 2 s so a run takes
	// seconds; the systemd test below still runs with the real delay.
	fastSleepDirectory = "/opt/lifecycle-test/bin"
	updaterPATH        = fastSleepDirectory + ":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

// setUpRepository creates both signing keys and points the agent's APT source
// at the repository, the way install.sh points it at the real one.
func (box *container) setUpRepository(t *testing.T) {
	t.Helper()
	for _, gpgHome := range []string{trustedGPGHome, untrustedGPGHome} {
		box.mustExec(t, "mkdir", "-p", "--mode=700", gpgHome)
		box.mustExec(t, "gpg", "--homedir", gpgHome, "--batch", "--pinentry-mode", "loopback", "--passphrase", "",
			"--quick-gen-key", "SC lifecycle test <test@invalid>", "ed25519", "sign", "never")
	}
	box.mustExec(t, "sh", "-c", "gpg --homedir "+trustedGPGHome+" --export > "+trustedKeyring)
	box.mustExec(t, "mkdir", "-p", repositoryPath)
	box.mustExec(t, "sh", "-c", "echo 'deb [signed-by="+trustedKeyring+"] file:"+repositoryPath+" ./' > /etc/apt/sources.list.d/sc-metrics-agent.list")

	box.mustExec(t, "mkdir", "-p", fastSleepDirectory)
	box.mustExec(t, "sh", "-c", `printf '#!/bin/sh\nexec /usr/bin/sleep "$(( $1 > 2 ? 2 : $1 ))"\n' > `+fastSleepDirectory+`/sleep && chmod +x `+fastSleepDirectory+`/sleep`)
}

// buildNewerPackage repackages this build with a version that sorts after it
// and returns the new file's path.
func (box *container) buildNewerPackage(t *testing.T) string {
	t.Helper()
	newerVersion := box.packageVersion(t, "/packages/new.deb") + "+next"
	box.mustExec(t, "sh", "-c", strings.Join([]string{
		"dpkg-deb --raw-extract /packages/new.deb /root/newer",
		"sed -i 's/^Version: .*/Version: " + newerVersion + "/' /root/newer/DEBIAN/control",
		"dpkg-deb --build /root/newer /root/newer.deb",
	}, " && "))
	return "/root/newer.deb"
}

// publish makes packagePath the only package in the repository, signed with
// the key in gpgHome.
func (box *container) publish(t *testing.T, packagePath string, gpgHome string) {
	t.Helper()
	box.mustExec(t, "sh", "-c", strings.Join([]string{
		"rm -f " + repositoryPath + "/*",
		"cp " + packagePath + " " + repositoryPath + "/",
		"cd " + repositoryPath,
		"apt-ftparchive packages . > Packages",
		"apt-ftparchive release . > Release",
		"gpg --homedir " + gpgHome + " --batch --yes --clearsign --output InRelease Release",
	}, " && "))
}

// runUpdater runs the updater script directly, with the capped sleep, and
// returns its output and exit code.
func (box *container) runUpdater(t *testing.T, cloud *fakeCloud) (string, int) {
	t.Helper()
	return box.exec(t, []string{"PATH=" + updaterPATH, "SC_CONFIG_REPO_URL=" + cloud.configURL}, "/usr/bin/sc-metrics-agent-updater.sh")
}

func (box *container) installedVersion(t *testing.T) string {
	t.Helper()
	return box.mustExec(t, "dpkg-query", "--show", "--showformat=${Version}", "sc-metrics-agent")
}

func (box *container) packageVersion(t *testing.T, packageFile string) string {
	t.Helper()
	return box.mustExec(t, "dpkg-deb", "--field", packageFile, "Version")
}

// installForUpdate installs packageFile with a working fake cloud and a signed
// repository ready to publish to.
func installForUpdate(t *testing.T, packageFile string) (*container, *fakeCloud) {
	t.Helper()
	cloud := newFakeCloud(t, "")
	box := startContainer(t)
	box.configureAgent(t)
	box.setUpRepository(t)
	box.install(t, cloud, packageFile)
	box.requireRunningAndEnabled(t, agentService)
	return box, cloud
}

// requireRunningNewBinary checks the agent was restarted onto the upgraded
// binary: dpkg replaced the file, so a process still running the old one shows
// its executable as "(deleted)".
func (box *container) requireRunningNewBinary(t *testing.T, previousPID string) {
	t.Helper()
	box.requireRunningAndEnabled(t, agentService)
	pid := box.mainPID(t)
	require.NotEqual(t, previousPID, pid, "the agent was not restarted")
	require.Equal(t, "/usr/bin/sc-metrics-agent", box.mustExec(t, "readlink", "/proc/"+pid+"/exe"),
		"the agent still runs the replaced binary")
}

// This goes through the same systemd service the hourly timer starts, with its
// real random delay, so it also checks how the unit runs the updater.
func TestUpdateTimerServiceUpgradesTheLatestReleaseToThisBuild(t *testing.T) {
	t.Parallel()
	box, cloud := installForUpdate(t, "previous.deb")
	previousPID := box.mainPID(t)
	box.publish(t, "/packages/new.deb", trustedGPGHome)
	// The unit doesn't pass the test's config repository through, so a drop-in does.
	box.mustExec(t, "mkdir", "-p", "/etc/systemd/system/"+updaterService+".d")
	box.mustExec(t, "sh", "-c", "printf '[Service]\\nEnvironment=SC_CONFIG_REPO_URL="+cloud.configURL+"\\n' > /etc/systemd/system/"+updaterService+".d/lifecycle-test.conf")
	box.mustExec(t, "systemctl", "daemon-reload")

	output, exitCode := box.exec(t, nil, "systemctl", "start", updaterService)
	require.Equal(t, 0, exitCode, "the updater service failed:\n%s\n%s", output,
		box.mustExec(t, "journalctl", "--no-pager", "--unit="+updaterService))

	assert.Equal(t, box.packageVersion(t, "/packages/new.deb"), box.installedVersion(t))
	box.requireRunningNewBinary(t, previousPID)
	box.requireRunningAndEnabled(t, updaterTimer)
	cloud.ingest.ClearRequests()
	require.Eventually(t, func() bool { return cloud.metricsFromTestVM() > 0 }, 60*time.Second, pollEvery,
		"the updated agent sends no metrics")
}

func TestUpdaterUpgradesAndRestartsARunningAgent(t *testing.T) {
	t.Parallel()
	box, cloud := installForUpdate(t, "new.deb")
	previousPID := box.mainPID(t)
	newerPackage := box.buildNewerPackage(t)
	box.publish(t, newerPackage, trustedGPGHome)

	output, exitCode := box.runUpdater(t, cloud)

	require.Equal(t, 0, exitCode, output)
	assert.Equal(t, box.packageVersion(t, newerPackage), box.installedVersion(t))
	box.requireRunningNewBinary(t, previousPID)
	box.requireRunningAndEnabled(t, updaterTimer)
}

func TestUpdaterLeavesTheLatestVersionAlone(t *testing.T) {
	t.Parallel()
	box, cloud := installForUpdate(t, "new.deb")
	pidBefore := box.mainPID(t)
	box.publish(t, "/packages/new.deb", trustedGPGHome)

	output, exitCode := box.runUpdater(t, cloud)

	require.Equal(t, 0, exitCode, output)
	assert.Contains(t, output, "Already running latest version")
	assert.Equal(t, box.packageVersion(t, "/packages/new.deb"), box.installedVersion(t))
	assert.Equal(t, pidBefore, box.mainPID(t), "the agent was restarted for nothing")
}

func TestUpdaterKeepsAStoppedAgentStopped(t *testing.T) {
	t.Parallel()
	box, cloud := installForUpdate(t, "new.deb")
	box.mustExec(t, "systemctl", "stop", agentService)
	newerPackage := box.buildNewerPackage(t)
	box.publish(t, newerPackage, trustedGPGHome)

	output, exitCode := box.runUpdater(t, cloud)

	require.Equal(t, 0, exitCode, output)
	assert.Equal(t, box.packageVersion(t, newerPackage), box.installedVersion(t))
	active, _ := box.unitState(t, agentService)
	assert.NotEqual(t, "active", active, "the updater started an agent that was stopped")
}

// Only the key in the APT source's keyring may deliver packages that run as
// root, so a repository signed by any other key must change nothing.
func TestUpdaterRefusesARepositorySignedByAnotherKey(t *testing.T) {
	t.Parallel()
	box, cloud := installForUpdate(t, "new.deb")
	previousVersion := box.installedVersion(t)
	previousPID := box.mainPID(t)
	box.publish(t, box.buildNewerPackage(t), untrustedGPGHome)

	output, exitCode := box.runUpdater(t, cloud)

	assert.NotEqual(t, 0, exitCode, "the updater reported success:\n%s", output)
	assert.Equal(t, previousVersion, box.installedVersion(t))
	box.requireRunningAndEnabled(t, agentService)
	assert.Equal(t, previousPID, box.mainPID(t))
}
