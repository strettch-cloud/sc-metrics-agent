package collector

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/procfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// useMountinfo makes the collectors see the given mountinfo lines, parsed the
// way procfs parses /proc/self/mountinfo, so a test controls what is mounted
// without needing root.
func useMountinfo(t *testing.T, mountinfoLines ...string) {
	t.Helper()
	procRoot := t.TempDir()
	processDirectory := filepath.Join(procRoot, "1")
	require.NoError(t, os.Mkdir(processDirectory, 0o755))
	mountinfo := strings.Join(mountinfoLines, "\n") + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(processDirectory, "mountinfo"), []byte(mountinfo), 0o644))
	procFS, err := procfs.NewFS(procRoot)
	require.NoError(t, err)
	process, err := procFS.Proc(1)
	require.NoError(t, err)
	mounts, err := process.MountInfo()
	require.NoError(t, err)
	useMounts(t, mounts, nil)
}

// The mount point must be a real directory, because the collector measures it
// with statfs.
func mountinfoLine(mountID, parentID int, mountPoint, fsType, device string) string {
	return fmt.Sprintf("%d %d 253:0 / %s rw,relatime shared:1 - %s %s rw", mountID, parentID, mountPoint, fsType, device)
}

func gatherFilesystem(t *testing.T) ([]*dto.MetricFamily, error) {
	t.Helper()
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(&filesystemCollector{logger: zaptest.NewLogger(t)})
	return registry.Gather()
}

func devicesReportedFor(families []*dto.MetricFamily, mountPoint string) []string {
	var devices []string
	for _, family := range families {
		if family.GetName() != "node_filesystem_size_bytes" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["mountpoint"] == mountPoint {
				devices = append(devices, labels["device"])
			}
		}
	}
	return devices
}

// A bind mount of a path on top of itself lists the same device, type and
// path twice. Two series with the same labels make Gather fail.
func TestFilesystemCollectorReportsAPathMountedTwiceOnce(t *testing.T) {
	mountPoint := t.TempDir()
	useMountinfo(t,
		mountinfoLine(40, 1, mountPoint, "ext4", "/dev/vda"),
		mountinfoLine(41, 40, mountPoint, "ext4", "/dev/vda"),
	)

	families, err := gatherFilesystem(t)

	require.NoError(t, err)
	assert.Equal(t, []string{"/dev/vda"}, devicesReportedFor(families, mountPoint))
}

// statfs measures only the top mount, so the series must carry that mount's
// device, not the hidden one's.
func TestFilesystemCollectorLabelsAStackedPathWithTheTopMount(t *testing.T) {
	mountPoint := t.TempDir()
	useMountinfo(t,
		mountinfoLine(40, 1, mountPoint, "ext4", "/dev/vda"),
		mountinfoLine(41, 40, mountPoint, "xfs", "/dev/vdb"),
	)

	families, err := gatherFilesystem(t)

	require.NoError(t, err)
	assert.Equal(t, []string{"/dev/vdb"}, devicesReportedFor(families, mountPoint))
}

// Under a tmpfs, statfs measures the tmpfs, so reporting the disk below it
// would label tmpfs numbers as that disk.
func TestFilesystemCollectorSkipsADiskHiddenUnderAnIgnoredMount(t *testing.T) {
	mountPoint := t.TempDir()
	useMountinfo(t,
		mountinfoLine(40, 1, mountPoint, "ext4", "/dev/vda"),
		mountinfoLine(41, 40, mountPoint, "tmpfs", "tmpfs"),
	)

	families, err := gatherFilesystem(t)

	require.NoError(t, err)
	assert.Empty(t, devicesReportedFor(families, mountPoint))
}

// mount --move keeps a mount's place in mountinfo, so moving an older mount on
// top of a path lists the top mount first. The top mount is the one stacked on
// the other: its parent is the hidden mount.
func TestFilesystemCollectorFindsTheTopMountWhenItIsListedFirst(t *testing.T) {
	mountPoint := t.TempDir()
	useMountinfo(t,
		mountinfoLine(76, 77, mountPoint, "ext4", "/dev/vdc"),
		mountinfoLine(77, 50, mountPoint, "ext4", "/dev/vdd"),
	)

	families, err := gatherFilesystem(t)

	require.NoError(t, err)
	assert.Equal(t, []string{"/dev/vdc"}, devicesReportedFor(families, mountPoint))
}

// proc(5): the root of a mount namespace lists itself as its own parent. It
// must not count as hidden by itself.
func TestFilesystemCollectorReportsAMountThatIsItsOwnParent(t *testing.T) {
	mountPoint := t.TempDir()
	useMountinfo(t, mountinfoLine(28, 28, mountPoint, "ext4", "/dev/vda"))

	families, err := gatherFilesystem(t)

	require.NoError(t, err)
	assert.Equal(t, []string{"/dev/vda"}, devicesReportedFor(families, mountPoint))
}

// When the hiding mount's parent link isn't visible, for example because the
// parent is outside this process's root, two mounts on one path still show as
// unhidden. Reporting both would bring back the failed Gather.
func TestFilesystemCollectorReportsAPathOnceWithoutAParentLink(t *testing.T) {
	mountPoint := t.TempDir()
	useMountinfo(t,
		mountinfoLine(40, 1, mountPoint, "ext4", "/dev/vda"),
		mountinfoLine(41, 2, mountPoint, "ext4", "/dev/vda"),
	)

	families, err := gatherFilesystem(t)

	require.NoError(t, err)
	assert.Equal(t, []string{"/dev/vda"}, devicesReportedFor(families, mountPoint))
}

// Linux allows any bytes in a path, but a Prometheus label value must be valid
// UTF-8. A path like "café" written in Latin-1 used to panic inside Gather and
// crash the whole agent on every collection.
func TestFilesystemCollectorKeepsAMountPointThatIsNotValidUTF8(t *testing.T) {
	mountPoint := filepath.Join(t.TempDir(), "caf\xe9")
	require.NoError(t, os.Mkdir(mountPoint, 0o755))
	useMountinfo(t, mountinfoLine(40, 1, mountPoint, "ext4", "/dev/vda"))

	families, err := gatherFilesystem(t)

	require.NoError(t, err)
	assert.Equal(t, []string{"/dev/vda"}, devicesReportedFor(families, strings.ToValidUTF8(mountPoint, "�")))
}
