package collector

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/procfs"
	"github.com/prometheus/procfs/blockdevice"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// collect runs one collection and returns, for each metric, the value of its
// label named label; an empty name returns one empty string per metric.
func collect(collector prometheus.Collector, label string) []string {
	// The registry calls Describe when a collector registers; it sets up the
	// metrics the collector reports.
	descriptions := make(chan *prometheus.Desc, 100)
	collector.Describe(descriptions)
	metrics := make(chan prometheus.Metric, 100)
	collector.Collect(metrics)
	close(metrics)
	var labels []string
	for metric := range metrics {
		var written dto.Metric
		if err := metric.Write(&written); err != nil {
			panic(err)
		}
		value := ""
		for _, pair := range written.Label {
			if pair.GetName() == label {
				value = pair.GetValue()
			}
		}
		labels = append(labels, value)
	}
	return labels
}

// fakeProcFS is a /proc holding only the given files.
func fakeProcFS(t *testing.T, files map[string]string) (procfs.FS, string) {
	t.Helper()
	directory := t.TempDir()
	for name, content := range files {
		path := filepath.Join(directory, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
	fs, err := procfs.NewFS(directory)
	require.NoError(t, err)
	return fs, directory
}

func useMounts(t *testing.T, mounts []*procfs.MountInfo, err error) {
	t.Helper()
	original := getMounts
	getMounts = func() ([]*procfs.MountInfo, error) { return mounts, err }
	t.Cleanup(func() { getMounts = original })
}

func useBlockDevices(t *testing.T, procDirectory string) {
	t.Helper()
	original := newBlockDevices
	newBlockDevices = func() (blockdevice.FS, error) { return blockdevice.NewFS(procDirectory, t.TempDir()) }
	t.Cleanup(func() { newBlockDevices = original })
}

// A /proc file the agent can't read costs that collector's metrics for one
// tick, nothing more.
func TestACollectorThatCantReadProcReportsNothing(t *testing.T) {
	emptyProc, _ := fakeProcFS(t, nil)
	useMounts(t, nil, errors.New("no mountinfo"))
	for name, collector := range map[string]prometheus.Collector{
		"cpu":        &cpuCollector{procFS: emptyProc, logger: zap.NewNop()},
		"memory":     &memoryCollector{procFS: emptyProc, logger: zap.NewNop()},
		"loadavg":    &loadAvgCollector{procFS: emptyProc, logger: zap.NewNop()},
		"network":    &networkCollector{procFS: emptyProc, logger: zap.NewNop()},
		"diskstats":  &diskStatsCollector{procFS: emptyProc, logger: zap.NewNop()},
		"filesystem": &filesystemCollector{procFS: emptyProc, logger: zap.NewNop()},
	} {
		assert.Empty(t, collect(collector, ""), name)
	}
}

func TestTheCPUCollectorNeedsPerCPULines(t *testing.T) {
	onlyTheTotal, _ := fakeProcFS(t, map[string]string{"stat": "cpu  1 2 3 4 5 6 7 8 0 0\nbtime 1700000000\n"})

	assert.Empty(t, collect(&cpuCollector{procFS: onlyTheTotal, logger: zap.NewNop()}, ""))
}

// Disk I/O is reported for mounted block devices only: not for pseudo or
// network filesystems, and not for disks nothing is mounted from.
func TestDiskStatsCoverMountedBlockDevicesOnly(t *testing.T) {
	_, procDirectory := fakeProcFS(t, map[string]string{"diskstats": "" +
		" 253       1 vda1 100 0 200 300 400 0 800 500 0 600 800 0 0 0 0\n" +
		" 253      16 vdb 100 0 200 300 400 0 800 500 0 600 800 0 0 0 0\n"})
	useBlockDevices(t, procDirectory)
	useMounts(t, []*procfs.MountInfo{
		{Source: "/dev/vda1", FSType: "ext4", MountPoint: "/"},
		{Source: "tmpfs", FSType: "tmpfs", MountPoint: "/run"},
		{Source: "server:/share", FSType: "fuse.sshfs", MountPoint: "/mnt/share"},
	}, nil)

	devices := collect(&diskStatsCollector{logger: zap.NewNop()}, "device")

	assert.Equal(t, []string{"vda1", "vda1", "vda1", "vda1"}, devices)
}

func TestDiskStatsNeedTheBlockDeviceStatistics(t *testing.T) {
	useMounts(t, []*procfs.MountInfo{{Source: "/dev/vda1", FSType: "ext4", MountPoint: "/"}}, nil)
	_, emptyProcDirectory := fakeProcFS(t, nil)
	useBlockDevices(t, emptyProcDirectory)
	assert.Empty(t, collect(&diskStatsCollector{logger: zap.NewNop()}, ""), "no diskstats file")

	original := newBlockDevices
	newBlockDevices = func() (blockdevice.FS, error) { return blockdevice.NewFS("/does/not/exist", "/does/not/exist") }
	t.Cleanup(func() { newBlockDevices = original })
	assert.Empty(t, collect(&diskStatsCollector{logger: zap.NewNop()}, ""), "no /proc or /sys")
}

// A filesystem the agent can't measure, such as one unmounted since the
// list was read, is skipped; the others are still reported.
func TestAFilesystemThatCantBeMeasuredIsSkipped(t *testing.T) {
	mounted := t.TempDir()
	// The one that can't be measured comes first, so stopping at it would
	// lose the one after it.
	useMounts(t, []*procfs.MountInfo{
		{Source: "/dev/vdb1", FSType: "ext4", MountPoint: filepath.Join(mounted, "unmounted")},
		{Source: "server:/share", FSType: "fuse.sshfs", MountPoint: "/mnt/share"},
		{Source: "/dev/vda1", FSType: "ext4", MountPoint: mounted},
	}, nil)

	mountPoints := collect(&filesystemCollector{logger: zap.NewNop()}, "mountpoint")

	assert.Equal(t, []string{mounted, mounted, mounted}, mountPoints)
}

func TestAnInterfaceThatIsGoneHasNoAddress(t *testing.T) {
	_, err := getInterfaceIPAddress("sc-test-gone0")

	assert.ErrorContains(t, err, "not found")
}
