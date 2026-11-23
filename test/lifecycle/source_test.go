//go:build lifecycle

package lifecycle

import (
	"strings"
	"testing"
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
	if strings.Contains(source, "Enabled:") {
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
