//go:build lifecycle

package lifecycle

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// These tests cover what the updater does about the agent's APT source.
//
// An Ubuntu release upgrade (do-release-upgrade) switches off every APT source
// that is not Ubuntu's own. A one-line source with signed-by cannot be
// converted, so the upgrade deletes it and leaves a commented copy ending in
// ".disabled". A deb822 source is kept with "Enabled: no" added. The updater
// puts its source back, unless the owner held the package or removed the
// source.

const (
	listSource   = "/etc/apt/sources.list.d/sc-metrics-agent.list"
	deb822Source = "/etc/apt/sources.list.d/sc-metrics-agent.sources"
	// What the upgrade leaves of a one-line source.
	disabledListSource = listSource + ".disabled"
	// The backup the upgrade to 24.04 leaves when it removes a one-line source.
	backupListSource = listSource + ".distUpgrade"
	// A repository with one edition per release, like the real one.
	editionsPath = "/srv/apt-editions"
	// The keyring install.sh writes and the package keeps current.
	installedKeyring = "/usr/share/keyrings/sc-metrics-agent-keyring.gpg"
	packagedKeyring  = "/usr/lib/sc-metrics-agent/apt-keyring.gpg"
	allowListFile    = "/etc/update-manager/release-upgrades.d/sc-metrics-agent.cfg"
)

// ubuntuUpgradeDeletesTheSource does to the one-line source what the upgrade
// does: the file is gone and a ".disabled" copy holds the line, commented out,
// under the upgrade's note.
func (box *container) ubuntuUpgradeDeletesTheSource(t *testing.T) {
	t.Helper()
	box.mustExec(t, "sh", "-c", strings.Join([]string{
		"{ echo '# This file could not be automatically migrated to a .sources file during the upgrade.'",
		"echo '# Please see sources.list(5) (or https://manpages.ubuntu.com/manpages/man5/sources.list.5.html)'",
		"echo '# for details on how to migrate manually.'",
		"echo",
		"sed 's/^/# /' " + listSource + "; } > " + disabledListSource,
	}, "; ")+" && rm "+listSource)
}

// writeDeb822Source replaces the one-line source with a deb822 one that reads
// the same repository. With switchedOff it carries the "Enabled: no" the
// upgrade adds.
func (box *container) writeDeb822Source(t *testing.T, uri string, suite string, components string, switchedOff bool) {
	t.Helper()
	lines := []string{"Types: deb", "URIs: " + uri, "Suites: " + suite}
	if components != "" {
		lines = append(lines, "Components: "+components)
	}
	lines = append(lines, "Signed-By: "+trustedKeyring)
	if switchedOff {
		lines = append(lines, "Enabled: no")
	}
	box.mustExec(t, "sh", "-c", "printf '%s\\n' '"+strings.Join(lines, "' '")+"' > "+deb822Source+" && rm -f "+listSource)
}

// publishEditions makes packagePath the only package of a repository that
// publishes each of the named editions, signed with the key in gpgHome.
func (box *container) publishEditions(t *testing.T, packagePath string, gpgHome string, editions ...string) {
	t.Helper()
	box.mustExec(t, "sh", "-c", strings.Join([]string{
		"rm -rf " + editionsPath,
		"mkdir -p " + editionsPath + "/pool",
		"cp " + packagePath + " " + editionsPath + "/pool/",
		"cd " + editionsPath,
		"architecture=$(dpkg --print-architecture)",
		"for edition in " + strings.Join(editions, " ") + "; do " + strings.Join([]string{
			"mkdir -p dists/$edition/main/binary-$architecture",
			"apt-ftparchive packages pool > dists/$edition/main/binary-$architecture/Packages",
			"apt-ftparchive -o APT::FTPArchive::Release::Suite=$edition -o APT::FTPArchive::Release::Codename=$edition" +
				" -o APT::FTPArchive::Release::Components=main -o APT::FTPArchive::Release::Architectures=$architecture" +
				" release dists/$edition > /root/Release",
			"mv /root/Release dists/$edition/Release",
			"gpg --homedir " + gpgHome + " --batch --yes --clearsign --output dists/$edition/InRelease dists/$edition/Release",
		}, " && ") + " || exit 1; done",
	}, " && "))
}

// readEdition points the one-line source at one edition of that repository.
func (box *container) readEdition(t *testing.T, edition string) {
	t.Helper()
	box.mustExec(t, "sh", "-c", "echo 'deb [signed-by="+trustedKeyring+"] file:"+editionsPath+" "+edition+" main' > "+listSource)
}

// release is the name of the release the container runs, which is what a VM's
// source is called after once the repository publishes that release.
func (box *container) release(t *testing.T) string {
	t.Helper()
	return box.mustExec(t, "sh", "-c", ". /etc/os-release && echo \"$VERSION_CODENAME\"")
}

func (box *container) sourceFiles(t *testing.T) string {
	t.Helper()
	return box.mustExec(t, "sh", "-c", "ls /etc/apt/sources.list.d/ | grep sc-metrics-agent || true")
}

func (box *container) requireExists(t *testing.T, path string, want bool) {
	t.Helper()
	if _, exitCode := box.exec(t, nil, "test", "-e", path); (exitCode == 0) != want {
		t.Errorf("%s exists: %v, want %v", path, exitCode == 0, want)
	}
}

// requireAptUpdateWorks runs the plain apt-get update an owner runs, which
// fails when any source is broken. The distribution's own sources are set
// aside first, so it reads the agent's source alone and needs no network.
func (box *container) requireAptUpdateWorks(t *testing.T) {
	t.Helper()
	box.mustExec(t, "sh", "-c", strings.Join([]string{
		"mkdir -p /root/distribution-sources",
		"find /etc/apt/sources.list.d -type f ! -name 'sc-*' -exec mv {} /root/distribution-sources/ ';'",
		// Older releases keep their sources here; newer ones have no such file.
		"if [ -f /etc/apt/sources.list ]; then mv /etc/apt/sources.list /root/distribution-sources/; fi",
	}, " && "))
	if output, exitCode := box.exec(t, nil, "apt-get", "update"); exitCode != 0 {
		t.Errorf("apt-get update exited %d:\n%s", exitCode, output)
	}
}

// requireRestoredSource checks the source is back, switched on, in deb822, and
// that the plain apt-get update a customer runs works with it.
func (box *container) requireRestoredSource(t *testing.T, edition string) {
	t.Helper()
	source := box.mustExec(t, "cat", deb822Source)
	if strings.Contains(strings.ToLower(source), "enabled:") {
		t.Errorf("the source is still switched off:\n%s", source)
	}
	if !strings.Contains(source+"\n", "\nSuites: "+edition+"\n") {
		t.Errorf("the source does not read the %q edition:\n%s", edition, source)
	}
	box.requireExists(t, disabledListSource, false)
	box.requireAptUpdateWorks(t)
}

func (box *container) requireUpdatedTo(t *testing.T, packageFile string, output string, exitCode int) {
	t.Helper()
	if exitCode != 0 {
		t.Fatalf("the updater exited %d:\n%s", exitCode, output)
	}
	if installed, want := box.installedVersion(t), box.packageVersion(t, packageFile); installed != want {
		t.Errorf("version %s is installed, want %s\n%s", installed, want, output)
	}
}

// startFakeReleaseUpgrade starts a process that looks like Ubuntu's release
// upgrader: do-release-upgrade unpacks it to /tmp/ubuntu-release-upgrader-<random>/
// and runs it from there, as root. The returned function ends it.
func (box *container) startFakeReleaseUpgrade(t *testing.T, user string) func() {
	t.Helper()
	const directory = "/tmp/ubuntu-release-upgrader-x1y2z3"
	box.mustExec(t, "sh", "-c", "mkdir -p "+directory+" && printf '#!/bin/sh\\nsleep 600\\n' > "+directory+"/resolute && chmod +x "+directory+"/resolute")
	start := exec.Command("docker", "exec", "--detach", "--user", user, box.name, directory+"/resolute", "--mode=server", "--frontend=DistUpgradeViewNonInteractive")
	if output, err := start.CombinedOutput(); err != nil {
		t.Fatalf("starting the fake release upgrade: %v\n%s", err, output)
	}
	require.Eventually(t, func() bool {
		_, exitCode := box.exec(t, nil, "pgrep", "-f", directory+"/")
		return exitCode == 0
	}, 10*time.Second, pollEvery, "the fake release upgrade never started")
	return func() { box.mustExec(t, "pkill", "-f", directory+"/") }
}

// Ubuntu's release upgrade rebuilds APT's caches as it goes. An apt-get update
// at the wrong moment removes the files it is writing and makes it abort
// before its clean-up. So the updater stays away from APT until it is over.
func TestUpdaterWaitsForAnUbuntuReleaseUpgradeToEnd(t *testing.T) {
	t.Parallel()
	box, cloud := installForUpdate(t, "new.deb")
	previousVersion := box.installedVersion(t)
	newerPackage := box.buildNewerPackage(t)
	box.publish(t, newerPackage, trustedGPGHome)
	endReleaseUpgrade := box.startFakeReleaseUpgrade(t, "root")

	output, exitCode := box.runUpdater(t, cloud)

	if exitCode != 0 {
		t.Errorf("the updater exited %d during the release upgrade:\n%s", exitCode, output)
	}
	if installed := box.installedVersion(t); installed != previousVersion {
		t.Errorf("version %s was installed during the release upgrade:\n%s", installed, output)
	}

	endReleaseUpgrade()
	output, exitCode = box.runUpdater(t, cloud)

	box.requireUpdatedTo(t, newerPackage, output, exitCode)
}

// The upgrade runs as root. A process of any other user that merely looks like
// it must not be able to hold the agent's updates off.
func TestUpdaterIgnoresALookalikeOfTheReleaseUpgradeRunByAnotherUser(t *testing.T) {
	t.Parallel()
	box, cloud := installForUpdate(t, "new.deb")
	newerPackage := box.buildNewerPackage(t)
	box.publish(t, newerPackage, trustedGPGHome)
	box.startFakeReleaseUpgrade(t, "nobody")

	output, exitCode := box.runUpdater(t, cloud)

	box.requireUpdatedTo(t, newerPackage, output, exitCode)
}

func TestUpdaterRestoresTheSourceOnlyAfterTheUbuntuReleaseUpgradeEnds(t *testing.T) {
	t.Parallel()
	box, cloud := installForUpdate(t, "new.deb")
	newerPackage := box.buildNewerPackage(t)
	box.publish(t, newerPackage, trustedGPGHome)
	box.ubuntuUpgradeDeletesTheSource(t)
	endReleaseUpgrade := box.startFakeReleaseUpgrade(t, "root")

	output, exitCode := box.runUpdater(t, cloud)

	if exitCode != 0 {
		t.Errorf("the updater exited %d during the release upgrade:\n%s", exitCode, output)
	}
	if files := box.sourceFiles(t); files != "sc-metrics-agent.list.disabled" {
		t.Errorf("the source files are %q during the release upgrade, want only the leftover:\n%s", files, output)
	}

	endReleaseUpgrade()
	output, exitCode = box.runUpdater(t, cloud)

	box.requireUpdatedTo(t, newerPackage, output, exitCode)
	box.requireRestoredSource(t, "./")
}

func TestUpdaterRestoresTheSourceAnUbuntuUpgradeDeleted(t *testing.T) {
	t.Parallel()
	box, cloud := installForUpdate(t, "new.deb")
	previousPID := box.mainPID(t)
	newerPackage := box.buildNewerPackage(t)
	box.publish(t, newerPackage, trustedGPGHome)
	box.ubuntuUpgradeDeletesTheSource(t)

	output, exitCode := box.runUpdater(t, cloud)

	box.requireUpdatedTo(t, newerPackage, output, exitCode)
	box.requireRestoredSource(t, "./")
	box.requireRunningNewBinary(t, previousPID)
}

// The upgrade from 22.04 to 24.04 removes a one-line source it cannot convert
// and leaves no ".disabled" copy, only its own backup of the file as it was.
func TestUpdaterRestoresTheSourceTheUpgradeTo2404Removed(t *testing.T) {
	t.Parallel()
	box, cloud := installForUpdate(t, "new.deb")
	newerPackage := box.buildNewerPackage(t)
	box.publish(t, newerPackage, trustedGPGHome)
	box.mustExec(t, "mv", listSource, backupListSource)

	output, exitCode := box.runUpdater(t, cloud)

	box.requireUpdatedTo(t, newerPackage, output, exitCode)
	box.requireRestoredSource(t, "./")
	// Left in place, the backup would bring back a source the owner removes later.
	box.requireExists(t, backupListSource, false)
}

func TestUpdaterRestoresTheSourceAnUbuntuUpgradeSwitchedOff(t *testing.T) {
	t.Parallel()
	box, cloud := installForUpdate(t, "new.deb")
	newerPackage := box.buildNewerPackage(t)
	box.publish(t, newerPackage, trustedGPGHome)
	box.writeDeb822Source(t, "file:"+repositoryPath, "./", "", true)

	output, exitCode := box.runUpdater(t, cloud)

	box.requireUpdatedTo(t, newerPackage, output, exitCode)
	box.requireRestoredSource(t, "./")
}

// deb822 field names are case-insensitive and need no space after the colon.
func TestUpdaterRestoresASwitchedOffSourceWrittenInAnotherStyle(t *testing.T) {
	t.Parallel()
	box, cloud := installForUpdate(t, "new.deb")
	newerPackage := box.buildNewerPackage(t)
	box.publish(t, newerPackage, trustedGPGHome)
	box.mustExec(t, "sh", "-c", "printf '%s\\n' 'types:deb' 'uris:file:"+repositoryPath+"' 'suites:./' 'signed-by:"+trustedKeyring+"' 'enabled:no' > "+deb822Source+" && rm "+listSource)

	output, exitCode := box.runUpdater(t, cloud)

	box.requireUpdatedTo(t, newerPackage, output, exitCode)
	box.requireRestoredSource(t, "./")
}

// When the restored source cannot be written, a full disk for example, what
// the upgrade left must stay: it is all the next run has to restore from.
func TestUpdaterKeepsTheLeftoverWhenTheSourceCannotBeWritten(t *testing.T) {
	t.Parallel()
	box, cloud := installForUpdate(t, "new.deb")
	box.publish(t, box.buildNewerPackage(t), trustedGPGHome)
	box.ubuntuUpgradeDeletesTheSource(t)
	// A directory with room for the leftover and no other file.
	box.mustExec(t, "sh", "-c", strings.Join([]string{
		"cp " + disabledListSource + " /root/leftover",
		"mount -t tmpfs -o nr_inodes=2 tmpfs /etc/apt/sources.list.d",
		"cp /root/leftover " + disabledListSource,
	}, " && "))

	output, exitCode := box.runUpdater(t, cloud)

	if exitCode == 0 {
		t.Errorf("the updater reported success:\n%s", output)
	}
	if files := box.sourceFiles(t); files != "sc-metrics-agent.list.disabled" {
		t.Errorf("the source files are now %q, want only the leftover:\n%s", files, output)
	}
}

// install.sh will write the source in deb822, and "apt modernize-sources"
// converts a one-line source to it.
func TestUpdaterUpdatesFromADeb822Source(t *testing.T) {
	t.Parallel()
	box, cloud := installForUpdate(t, "new.deb")
	newerPackage := box.buildNewerPackage(t)
	box.publish(t, newerPackage, trustedGPGHome)
	box.writeDeb822Source(t, "file:"+repositoryPath, "./", "", false)

	output, exitCode := box.runUpdater(t, cloud)

	box.requireUpdatedTo(t, newerPackage, output, exitCode)
}

// Ubuntu's next release upgrade carries a source over only when it is named
// after the release the VM runs, so the restored source takes that name.
func TestRestoredSourceNamesTheVMsRelease(t *testing.T) {
	t.Parallel()
	box, cloud := installForUpdate(t, "new.deb")
	newerPackage := box.buildNewerPackage(t)
	box.publishEditions(t, newerPackage, trustedGPGHome, "previous", box.release(t))
	box.readEdition(t, "previous")
	box.ubuntuUpgradeDeletesTheSource(t)

	output, exitCode := box.runUpdater(t, cloud)

	box.requireUpdatedTo(t, newerPackage, output, exitCode)
	box.requireRestoredSource(t, box.release(t))
}

func TestRestoredSourceKeepsItsEditionWhenTheVMsReleaseIsNotPublished(t *testing.T) {
	t.Parallel()
	box, cloud := installForUpdate(t, "new.deb")
	newerPackage := box.buildNewerPackage(t)
	box.publishEditions(t, newerPackage, trustedGPGHome, "previous")
	box.readEdition(t, "previous")
	box.ubuntuUpgradeDeletesTheSource(t)

	output, exitCode := box.runUpdater(t, cloud)

	box.requireUpdatedTo(t, newerPackage, output, exitCode)
	box.requireRestoredSource(t, "previous")
}

// The upgrade can rename a deb822 source to the new release and still switch
// it off, when the repository does not publish that release yet. The 24.04
// edition is what install.sh gives a system the repository does not publish.
func TestRestoredSourceReadsThe2404EditionWhenItsOwnIsNotPublished(t *testing.T) {
	t.Parallel()
	box, cloud := installForUpdate(t, "new.deb")
	newerPackage := box.buildNewerPackage(t)
	box.publishEditions(t, newerPackage, trustedGPGHome, "noble")
	box.writeDeb822Source(t, "file:"+editionsPath, "unpublished", "main", true)

	output, exitCode := box.runUpdater(t, cloud)

	box.requireUpdatedTo(t, newerPackage, output, exitCode)
	box.requireRestoredSource(t, "noble")
}

// On a VM the package ships the key that signs the repository. Here the test
// key stands in for it.
func TestUpdaterRestoresAMissingSigningKeyWithTheSource(t *testing.T) {
	t.Parallel()
	box, cloud := installForUpdate(t, "new.deb")
	newerPackage := box.buildNewerPackage(t)
	box.publish(t, newerPackage, trustedGPGHome)
	box.mustExec(t, "cp", trustedKeyring, packagedKeyring)
	box.mustExec(t, "sh", "-c", "echo 'deb [signed-by="+installedKeyring+"] file:"+repositoryPath+" ./' > "+listSource)
	box.ubuntuUpgradeDeletesTheSource(t)
	box.mustExec(t, "rm", installedKeyring)

	output, exitCode := box.runUpdater(t, cloud)

	box.requireUpdatedTo(t, newerPackage, output, exitCode)
	box.requireExists(t, installedKeyring, true)
}

// A source that APT cannot verify must not be put back: it would break the
// customer's own apt-get update. The leftover stays, so the next run tries
// again.
func TestUpdaterDoesNotRestoreASourceItCannotVerify(t *testing.T) {
	t.Parallel()
	box, cloud := installForUpdate(t, "new.deb")
	previousVersion := box.installedVersion(t)
	box.publish(t, box.buildNewerPackage(t), untrustedGPGHome)
	box.ubuntuUpgradeDeletesTheSource(t)

	output, exitCode := box.runUpdater(t, cloud)

	if exitCode == 0 {
		t.Errorf("the updater reported success:\n%s", output)
	}
	if files := box.sourceFiles(t); files != "sc-metrics-agent.list.disabled" {
		t.Errorf("the source files are now %q, want only the leftover", files)
	}
	if installed := box.installedVersion(t); installed != previousVersion {
		t.Errorf("version %s is installed, want %s", installed, previousVersion)
	}
	box.requireAptUpdateWorks(t)
}

// Removing the source is how an owner stops updates for good. The updater
// must not put it back, and has nothing to fail about.
func TestUpdaterDoesNotRestoreASourceTheOwnerRemoved(t *testing.T) {
	t.Parallel()
	box, cloud := installForUpdate(t, "new.deb")
	previousVersion := box.installedVersion(t)
	box.publish(t, box.buildNewerPackage(t), trustedGPGHome)
	box.mustExec(t, "rm", listSource)

	output, exitCode := box.runUpdater(t, cloud)

	if exitCode != 0 {
		t.Errorf("the updater exited %d:\n%s", exitCode, output)
	}
	if files := box.sourceFiles(t); files != "" {
		t.Errorf("the updater created %q", files)
	}
	if installed := box.installedVersion(t); installed != previousVersion {
		t.Errorf("version %s is installed, want %s", installed, previousVersion)
	}
}

// An owner who renames the source keeps its line as it was. Only the upgrade
// comments the line out, and only that is put back.
func TestUpdaterDoesNotRestoreASourceTheOwnerRenamed(t *testing.T) {
	t.Parallel()
	box, cloud := installForUpdate(t, "new.deb")
	box.publish(t, box.buildNewerPackage(t), trustedGPGHome)
	box.mustExec(t, "mv", listSource, disabledListSource)

	output, exitCode := box.runUpdater(t, cloud)

	if exitCode != 0 {
		t.Errorf("the updater exited %d:\n%s", exitCode, output)
	}
	if files := box.sourceFiles(t); files != "sc-metrics-agent.list.disabled" {
		t.Errorf("the source files are now %q, want only the renamed one", files)
	}
}

// A customer holds the package (apt-mark hold) to stay on its version. A
// newer version in the repository must change nothing, and is no failure.
func TestUpdaterLeavesAHeldAgentOnItsVersion(t *testing.T) {
	t.Parallel()
	box, cloud := installForUpdate(t, "new.deb")
	previousVersion := box.installedVersion(t)
	pidBefore := box.mainPID(t)
	box.mustExec(t, "apt-mark", "hold", "sc-metrics-agent")
	box.publish(t, box.buildNewerPackage(t), trustedGPGHome)

	output, exitCode := box.runUpdater(t, cloud)

	if exitCode != 0 {
		t.Errorf("the updater exited %d:\n%s", exitCode, output)
	}
	if installed := box.installedVersion(t); installed != previousVersion {
		t.Errorf("version %s is installed, want %s", installed, previousVersion)
	}
	if status := box.packageStatus(t); status != "hi" {
		t.Errorf("the package is %s: the hold was dropped", status)
	}
	if box.mainPID(t) != pidBefore {
		t.Error("the agent was restarted")
	}
}

// With the package on hold nothing would be installed, so there is no reason
// to touch the source either.
func TestUpdaterLeavesTheSwitchedOffSourceOfAHeldAgentAlone(t *testing.T) {
	t.Parallel()
	box, cloud := installForUpdate(t, "new.deb")
	box.mustExec(t, "apt-mark", "hold", "sc-metrics-agent")
	box.publish(t, box.buildNewerPackage(t), trustedGPGHome)
	box.ubuntuUpgradeDeletesTheSource(t)

	output, exitCode := box.runUpdater(t, cloud)

	if exitCode != 0 {
		t.Errorf("the updater exited %d:\n%s", exitCode, output)
	}
	if files := box.sourceFiles(t); files != "sc-metrics-agent.list.disabled" {
		t.Errorf("the source files are now %q, want only the leftover", files)
	}
}

// Ubuntu's release upgrade keeps a deb822 source switched on only when its
// address is on the upgrade's allow list, which this file adds to.
func TestInstallPutsTheRepositoryOnUbuntusUpgradeAllowList(t *testing.T) {
	t.Parallel()
	box, _ := installForUpdate(t, "new.deb")

	allowList := box.mustExec(t, "cat", allowListFile)

	if !strings.Contains(allowList, "[ThirdPartyMirrors]\n") {
		t.Errorf("no [ThirdPartyMirrors] section:\n%s", allowList)
	}
	// The upgrade compares whole addresses, so these must be exactly what
	// install.sh writes for the stable and the beta channel.
	for _, address := range []string{
		"https://repo.cloud.strettch.com/metrics/aptly",
		"https://repo.cloud.strettch.com/metrics/beta/aptly",
	} {
		if !strings.Contains(allowList+"\n", " = "+address+"\n") {
			t.Errorf("%s is not on the allow list:\n%s", address, allowList)
		}
	}
	if conffiles := box.mustExec(t, "dpkg-query", "--show", "--showformat=${Conffiles}", "sc-metrics-agent"); !strings.Contains(conffiles, allowListFile+" ") {
		t.Errorf("dpkg does not treat %s as a configuration file, so an upgrade would overwrite the owner's changes:\n%s", allowListFile, conffiles)
	}
}

// Deleting the file is how an owner takes the repository off the allow list.
func TestAnUpgradeDoesNotBringBackADeletedAllowList(t *testing.T) {
	t.Parallel()
	box, cloud := installForUpdate(t, "new.deb")
	box.mustExec(t, "rm", allowListFile)
	newerPackage := box.buildNewerPackage(t)
	box.publish(t, newerPackage, trustedGPGHome)

	output, exitCode := box.runUpdater(t, cloud)

	box.requireUpdatedTo(t, newerPackage, output, exitCode)
	box.requireExists(t, allowListFile, false)
}
