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

// procWithMountinfo returns a procfs rooted in a temporary directory, whose
// self/mountinfo holds the given lines, so a test controls what is mounted
// without needing root.
func procWithMountinfo(t *testing.T, mountinfoLines ...string) procfs.FS {
	t.Helper()
	procRoot := t.TempDir()
	processDirectory := filepath.Join(procRoot, "1")
	require.NoError(t, os.Mkdir(processDirectory, 0o755))
	mountinfo := strings.Join(mountinfoLines, "\n") + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(processDirectory, "mountinfo"), []byte(mountinfo), 0o644))
	require.NoError(t, os.Symlink("1", filepath.Join(procRoot, "self")))
	procFS, err := procfs.NewFS(procRoot)
	require.NoError(t, err)
	return procFS
}

// mountinfoLine formats one /proc/self/mountinfo line. The mount point must be
// a real directory, because the collector measures it with statfs.
func mountinfoLine(mountID, parentID int, mountPoint, fsType, device string) string {
	return fmt.Sprintf("%d %d 253:0 / %s rw,relatime shared:1 - %s %s rw", mountID, parentID, mountPoint, fsType, device)
}

func gatherFilesystem(t *testing.T, procFS procfs.FS) ([]*dto.MetricFamily, error) {
	t.Helper()
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(&filesystemCollector{procFS: procFS, logger: zaptest.NewLogger(t)})
	return registry.Gather()
}

// devicesReportedFor returns the device label of every node_filesystem_size_bytes
// series for mountPoint.
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

func TestFilesystemCollectorReadsMountsFromItsProcfs(t *testing.T) {
	mountPoint := t.TempDir()
	procFS := procWithMountinfo(t, mountinfoLine(40, 1, mountPoint, "ext4", "/dev/vdz"))

	families, err := gatherFilesystem(t, procFS)

	require.NoError(t, err)
	assert.Equal(t, []string{"/dev/vdz"}, devicesReportedFor(families, mountPoint))
}

// A bind mount of a path on top of itself lists the same device, type and
// path twice. Two series with the same labels make Gather fail.
func TestFilesystemCollectorReportsAPathMountedTwiceOnce(t *testing.T) {
	mountPoint := t.TempDir()
	procFS := procWithMountinfo(t,
		mountinfoLine(40, 1, mountPoint, "ext4", "/dev/vda"),
		mountinfoLine(41, 40, mountPoint, "ext4", "/dev/vda"),
	)

	families, err := gatherFilesystem(t, procFS)

	require.NoError(t, err)
	assert.Equal(t, []string{"/dev/vda"}, devicesReportedFor(families, mountPoint))
}

// statfs measures only the top mount, which mountinfo lists last, so the series
// must carry that mount's device, not the hidden one's.
func TestFilesystemCollectorLabelsAStackedPathWithTheTopMount(t *testing.T) {
	mountPoint := t.TempDir()
	procFS := procWithMountinfo(t,
		mountinfoLine(40, 1, mountPoint, "ext4", "/dev/vda"),
		mountinfoLine(41, 40, mountPoint, "xfs", "/dev/vdb"),
	)

	families, err := gatherFilesystem(t, procFS)

	require.NoError(t, err)
	assert.Equal(t, []string{"/dev/vdb"}, devicesReportedFor(families, mountPoint))
}

// Under a tmpfs, statfs measures the tmpfs, so reporting the disk below it
// would label tmpfs numbers as that disk.
func TestFilesystemCollectorSkipsADiskHiddenUnderAnIgnoredMount(t *testing.T) {
	mountPoint := t.TempDir()
	procFS := procWithMountinfo(t,
		mountinfoLine(40, 1, mountPoint, "ext4", "/dev/vda"),
		mountinfoLine(41, 40, mountPoint, "tmpfs", "tmpfs"),
	)

	families, err := gatherFilesystem(t, procFS)

	require.NoError(t, err)
	assert.Empty(t, devicesReportedFor(families, mountPoint))
}
