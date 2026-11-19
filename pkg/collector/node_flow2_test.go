package collector

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/procfs"
	"github.com/prometheus/procfs/blockdevice"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// fakeVMF2 is a stand-in for one VM's /proc and /sys, plus its mount table.
type fakeVMF2 struct {
	procDir string
	sysDir  string
	mounts  []*procfs.MountInfo
	// mountsErr makes the mount table unreadable.
	mountsErr error
}

const (
	fakeStatF2 = "cpu  1000 200 300 4000 500 60 70 80 0 0\n" +
		"cpu0 600 100 100 2000 250 30 35 40 0 0\n" +
		"cpu1 400 100 200 2000 250 30 35 40 0 0\n" +
		"btime 1700000000\n"
	fakeMeminfoF2 = "MemTotal:        2048000 kB\n" +
		"MemFree:          512000 kB\n" +
		"MemAvailable:    1024000 kB\n" +
		"Buffers:           64000 kB\n" +
		"Cached:           256000 kB\n" +
		"SwapTotal:       1000000 kB\n" +
		"SwapFree:         900000 kB\n"
	fakeLoadavgF2  = "0.50 1.25 2.75 1/123 4567\n"
	netDevHeaderF2 = "Inter-|   Receive                                                |  Transmit\n" +
		" face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n"
)

func diskstatsLineF2(name string, reads, readSectors, writes, writeSectors int) string {
	return fmt.Sprintf("253 0 %s %d 0 %d 0 %d 0 %d 0 0 0 0 0 0 0 0 0 0 0\n", name, reads, readSectors, writes, writeSectors)
}

func netDevLineF2(name string, rxBytes, rxPackets, txBytes, txPackets int) string {
	return fmt.Sprintf("%s: %d %d 0 0 0 0 0 0 %d %d 0 0 0 0 0 0\n", name, rxBytes, rxPackets, txBytes, txPackets)
}

// newFakeVMF2 writes every /proc source with known values. The mount table
// holds one ext4 filesystem on a real directory, so its size can be read.
func newFakeVMF2(t *testing.T) *fakeVMF2 {
	t.Helper()
	root := t.TempDir()
	vm := &fakeVMF2{procDir: filepath.Join(root, "proc"), sysDir: filepath.Join(root, "sys")}
	require.NoError(t, os.MkdirAll(filepath.Join(vm.procDir, "net"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(vm.sysDir, "block"), 0o755))
	vm.write(t, "stat", fakeStatF2)
	vm.write(t, "meminfo", fakeMeminfoF2)
	vm.write(t, "loadavg", fakeLoadavgF2)
	vm.write(t, "diskstats", diskstatsLineF2("vda1", 11, 22, 33, 44))
	vm.write(t, "net/dev", netDevHeaderF2+netDevLineF2("lo", 100, 1, 200, 2))
	vm.mounts = []*procfs.MountInfo{
		{MountID: 1, ParentID: 1, MountPoint: t.TempDir(), FSType: "ext4", Source: "/dev/vda1"},
	}
	return vm
}

func (vm *fakeVMF2) write(t *testing.T, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(vm.procDir, name), []byte(content), 0o644))
}

func (vm *fakeVMF2) remove(t *testing.T, name string) {
	t.Helper()
	require.NoError(t, os.Remove(filepath.Join(vm.procDir, name)))
}

// collectRoundF2 runs one round of all six readers against the fake VM.
func collectRoundF2(t *testing.T, vm *fakeVMF2, logger *zap.Logger) []*dto.MetricFamily {
	t.Helper()
	originalGetMounts, originalBlockDevices := getMounts, newBlockDevices
	t.Cleanup(func() { getMounts, newBlockDevices = originalGetMounts, originalBlockDevices })
	getMounts = func() ([]*procfs.MountInfo, error) { return vm.mounts, vm.mountsErr }
	newBlockDevices = func() (blockdevice.FS, error) { return blockdevice.NewFS(vm.procDir, vm.sysDir) }

	fs, err := procfs.NewFS(vm.procDir)
	require.NoError(t, err)
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		&cpuCollector{procFS: fs, logger: logger},
		&memoryCollector{procFS: fs, logger: logger},
		&loadAvgCollector{procFS: fs, logger: logger},
		&diskStatsCollector{procFS: fs, logger: logger},
		&networkCollector{procFS: fs, logger: logger},
		&filesystemCollector{procFS: fs, logger: logger},
	)
	round := &SystemCollector{registry: registry, logger: logger, enabled: map[string]bool{"all": true}}

	families, err := round.Collect(context.Background())
	require.NoError(t, err)
	return families
}

type seriesF2 struct {
	labels map[string]string
	value  float64
}

func seriesByNameF2(families []*dto.MetricFamily) map[string][]seriesF2 {
	out := map[string][]seriesF2{}
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, pair := range metric.GetLabel() {
				labels[pair.GetName()] = pair.GetValue()
			}
			value := metric.GetGauge().GetValue()
			if metric.Counter != nil {
				value = metric.GetCounter().GetValue()
			}
			out[family.GetName()] = append(out[family.GetName()], seriesF2{labels: labels, value: value})
		}
	}
	return out
}

func namesF2(families []*dto.MetricFamily) []string {
	var names []string
	for _, family := range families {
		names = append(names, family.GetName())
	}
	sort.Strings(names)
	return names
}

var (
	cpuMetricsF2        = []string{"node_cpu_seconds_total"}
	memoryMetricsF2     = []string{"node_memory_Buffers_bytes", "node_memory_Cached_bytes", "node_memory_MemAvailable_bytes", "node_memory_MemFree_bytes", "node_memory_MemTotal_bytes", "node_memory_SwapFree_bytes", "node_memory_SwapTotal_bytes"}
	loadMetricsF2       = []string{"node_load1", "node_load15", "node_load5"}
	diskMetricsF2       = []string{"node_disk_read_bytes_total", "node_disk_reads_completed_total", "node_disk_writes_completed_total", "node_disk_written_bytes_total"}
	networkMetricsF2    = []string{"node_network_receive_bytes_total", "node_network_receive_packets_total", "node_network_transmit_bytes_total", "node_network_transmit_packets_total"}
	filesystemMetricsF2 = []string{"node_filesystem_avail_bytes", "node_filesystem_free_bytes", "node_filesystem_size_bytes"}
)

var skippedFSTypesF2 = []string{
	"autofs", "binfmt_misc", "cgroup", "cgroup2", "configfs", "debugfs", "devpts", "devtmpfs",
	"efivarfs", "fusectl", "hugetlbfs", "mqueue", "nsfs", "overlay", "proc", "procfs",
	"pstore", "rpc_pipefs", "securityfs", "selinuxfs", "squashfs", "sysfs", "tmpfs", "tracefs",
	"nfs", "nfs4", "cifs", "smb",
}

func joinF2(groups ...[]string) []string {
	var all []string
	for _, group := range groups {
		all = append(all, group...)
	}
	sort.Strings(all)
	return all
}

func TestWhatOneRoundReadsFromTheVM(t *testing.T) {
	readers := []struct {
		name         string
		breakSource  func(t *testing.T, vm *fakeVMF2)
		lostMetrics  []string
		otherMetrics []string
	}{
		{
			name:         "CPU",
			breakSource:  func(t *testing.T, vm *fakeVMF2) { vm.remove(t, "stat") },
			lostMetrics:  cpuMetricsF2,
			otherMetrics: joinF2(memoryMetricsF2, loadMetricsF2, diskMetricsF2, networkMetricsF2, filesystemMetricsF2),
		},
		{
			name:         "memory",
			breakSource:  func(t *testing.T, vm *fakeVMF2) { vm.remove(t, "meminfo") },
			lostMetrics:  memoryMetricsF2,
			otherMetrics: joinF2(cpuMetricsF2, loadMetricsF2, diskMetricsF2, networkMetricsF2, filesystemMetricsF2),
		},
		{
			name:         "load",
			breakSource:  func(t *testing.T, vm *fakeVMF2) { vm.remove(t, "loadavg") },
			lostMetrics:  loadMetricsF2,
			otherMetrics: joinF2(cpuMetricsF2, memoryMetricsF2, diskMetricsF2, networkMetricsF2, filesystemMetricsF2),
		},
		{
			name:         "disk I/O",
			breakSource:  func(t *testing.T, vm *fakeVMF2) { vm.remove(t, "diskstats") },
			lostMetrics:  diskMetricsF2,
			otherMetrics: joinF2(cpuMetricsF2, memoryMetricsF2, loadMetricsF2, networkMetricsF2, filesystemMetricsF2),
		},
		{
			name:         "network",
			breakSource:  func(t *testing.T, vm *fakeVMF2) { vm.remove(t, "net/dev") },
			lostMetrics:  networkMetricsF2,
			otherMetrics: joinF2(cpuMetricsF2, memoryMetricsF2, loadMetricsF2, diskMetricsF2, filesystemMetricsF2),
		},
		{
			// The disk I/O reader reads the same mount table, so it is lost too.
			name:         "filesystems",
			breakSource:  func(t *testing.T, vm *fakeVMF2) { vm.mountsErr = errors.New("mountinfo unreadable") },
			lostMetrics:  joinF2(filesystemMetricsF2, diskMetricsF2),
			otherMetrics: joinF2(cpuMetricsF2, memoryMetricsF2, loadMetricsF2, networkMetricsF2),
		},
	}
	for _, reader := range readers {
		t.Run("a "+reader.name+" reader that cannot read its source adds no metric, logs at debug only, and the other readers' metrics are kept", func(t *testing.T) {
			vm := newFakeVMF2(t)
			reader.breakSource(t, vm)
			core, logs := observer.New(zapcore.DebugLevel)

			families := collectRoundF2(t, vm, zap.New(core))

			assert.Equal(t, reader.otherMetrics, namesF2(families))
			assert.Empty(t, logs.Filter(func(e observer.LoggedEntry) bool { return e.Level > zapcore.DebugLevel }).All())
			withError := logs.Filter(func(e observer.LoggedEntry) bool {
				_, ok := e.ContextMap()["error"]
				return ok
			})
			assert.NotEmpty(t, withError.All(), "the reader should log why at debug level")
		})
	}

	t.Run("a round whose six readers can all read their sources saves every metric of the table", func(t *testing.T) {
		vm := newFakeVMF2(t)

		families := collectRoundF2(t, vm, zap.NewNop())

		assert.Equal(t, joinF2(cpuMetricsF2, memoryMetricsF2, loadMetricsF2, diskMetricsF2, networkMetricsF2, filesystemMetricsF2), namesF2(families))
	})

	t.Run("the CPU reader reports the seconds of all cores added together, for each of the 8 modes", func(t *testing.T) {
		vm := newFakeVMF2(t)

		cpu := seriesByNameF2(collectRoundF2(t, vm, zap.NewNop()))["node_cpu_seconds_total"]

		// The fake /proc/stat total line, in hundredths of a second.
		want := map[string]float64{"user": 10, "nice": 2, "system": 3, "idle": 40, "iowait": 5, "irq": 0.6, "softirq": 0.7, "steal": 0.8}
		got := map[string]float64{}
		for _, s := range cpu {
			assert.Equal(t, []string{"mode"}, labelNamesF2(s.labels))
			got[s.labels["mode"]] = s.value
		}
		require.Len(t, got, len(want))
		for mode, seconds := range want {
			assert.InDelta(t, seconds, got[mode], 1e-9, "mode %s", mode)
		}
	})

	t.Run("the memory reader reports the 7 memory fields in bytes, with no labels", func(t *testing.T) {
		vm := newFakeVMF2(t)

		series := seriesByNameF2(collectRoundF2(t, vm, zap.NewNop()))

		const kibibyte = 1024
		want := map[string]float64{
			"node_memory_MemTotal_bytes":     2048000 * kibibyte,
			"node_memory_MemFree_bytes":      512000 * kibibyte,
			"node_memory_MemAvailable_bytes": 1024000 * kibibyte,
			"node_memory_Buffers_bytes":      64000 * kibibyte,
			"node_memory_Cached_bytes":       256000 * kibibyte,
			"node_memory_SwapTotal_bytes":    1000000 * kibibyte,
			"node_memory_SwapFree_bytes":     900000 * kibibyte,
		}
		for name, bytes := range want {
			require.Len(t, series[name], 1, name)
			assert.Empty(t, series[name][0].labels, name)
			assert.Equal(t, bytes, series[name][0].value, name)
		}
	})

	t.Run("the load reader reports the 1, 5 and 15 minute load, with no labels", func(t *testing.T) {
		vm := newFakeVMF2(t)

		series := seriesByNameF2(collectRoundF2(t, vm, zap.NewNop()))

		want := map[string]float64{"node_load1": 0.5, "node_load5": 1.25, "node_load15": 2.75}
		for name, load := range want {
			require.Len(t, series[name], 1, name)
			assert.Empty(t, series[name][0].labels, name)
			assert.Equal(t, load, series[name][0].value, name)
		}
	})

	t.Run("the disk I/O reader reports reads, writes, and bytes read and written, labelled with the device", func(t *testing.T) {
		vm := newFakeVMF2(t)
		const reads, readSectors, writes, writeSectors, sectorBytes = 11, 22, 33, 44, 512

		series := seriesByNameF2(collectRoundF2(t, vm, zap.NewNop()))

		want := map[string]float64{
			"node_disk_reads_completed_total":  reads,
			"node_disk_writes_completed_total": writes,
			"node_disk_read_bytes_total":       readSectors * sectorBytes,
			"node_disk_written_bytes_total":    writeSectors * sectorBytes,
		}
		for name, value := range want {
			require.Len(t, series[name], 1, name)
			assert.Equal(t, []string{"device"}, labelNamesF2(series[name][0].labels), name)
			assert.Equal(t, value, series[name][0].value, name)
		}
	})

	t.Run("the network reader reports bytes and packets received and sent, labelled with the device and ip_address", func(t *testing.T) {
		vm := newFakeVMF2(t)

		series := seriesByNameF2(collectRoundF2(t, vm, zap.NewNop()))

		want := map[string]float64{
			"node_network_receive_bytes_total":    100,
			"node_network_receive_packets_total":  1,
			"node_network_transmit_bytes_total":   200,
			"node_network_transmit_packets_total": 2,
		}
		for name, value := range want {
			require.Len(t, series[name], 1, name)
			assert.Equal(t, map[string]string{"device": "lo", "ip_address": "none"}, series[name][0].labels, name)
			assert.Equal(t, value, series[name][0].value, name)
		}
	})

	t.Run("the filesystem reader reports size, free and available bytes, labelled with the device, type and path", func(t *testing.T) {
		vm := newFakeVMF2(t)
		mountPoint := vm.mounts[0].MountPoint
		var stat syscall.Statfs_t
		require.NoError(t, syscall.Statfs(mountPoint, &stat))
		size := float64(stat.Blocks * uint64(stat.Bsize))
		free := float64(stat.Bfree * uint64(stat.Bsize))
		avail := float64(stat.Bavail * uint64(stat.Bsize))
		// Free space can move between the two reads on a shared machine.
		tolerance := size / 100

		series := seriesByNameF2(collectRoundF2(t, vm, zap.NewNop()))

		labels := map[string]string{"device": "/dev/vda1", "fstype": "ext4", "mountpoint": mountPoint}
		for name, value := range map[string]float64{"node_filesystem_size_bytes": size, "node_filesystem_free_bytes": free, "node_filesystem_avail_bytes": avail} {
			require.Len(t, series[name], 1, name)
			assert.Equal(t, labels, series[name][0].labels, name)
			assert.InDelta(t, value, series[name][0].value, tolerance, name)
		}
	})

	t.Run("an interface name that is not valid UTF-8 is reported with U+FFFD in its place", func(t *testing.T) {
		vm := newFakeVMF2(t)
		vm.write(t, "net/dev", netDevHeaderF2+netDevLineF2("veth\xff1", 1, 1, 1, 1))

		series := seriesByNameF2(collectRoundF2(t, vm, zap.NewNop()))

		require.Len(t, series["node_network_receive_bytes_total"], 1)
		assert.Equal(t, "veth�1", series["node_network_receive_bytes_total"][0].labels["device"])
	})

	t.Run("a mount path, device and type that are not valid UTF-8 are reported with U+FFFD in their place", func(t *testing.T) {
		vm := newFakeVMF2(t)
		mountPoint := filepath.Join(t.TempDir(), "data\xff")
		require.NoError(t, os.Mkdir(mountPoint, 0o755))
		vm.mounts = []*procfs.MountInfo{{MountID: 1, ParentID: 1, MountPoint: mountPoint, FSType: "fuse.ss\xffhfs", Source: "/dev/vd\xffb"}}

		series := seriesByNameF2(collectRoundF2(t, vm, zap.NewNop()))

		require.Len(t, series["node_filesystem_size_bytes"], 1)
		assert.Equal(t, map[string]string{
			"device":     "/dev/vd�b",
			"fstype":     "fuse.ss�hfs",
			"mountpoint": strings.ToValidUTF8(mountPoint, "�"),
		}, series["node_filesystem_size_bytes"][0].labels)
	})
}

func labelNamesF2(labels map[string]string) []string {
	var names []string
	for name := range labels {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// diskCountersF2 returns the disk I/O series whose device label is one of
// names, keyed by metric name.
func diskCountersF2(families []*dto.MetricFamily, names ...string) map[string]float64 {
	out := map[string]float64{}
	for name, series := range seriesByNameF2(families) {
		if !strings.HasPrefix(name, "node_disk_") {
			continue
		}
		for _, s := range series {
			for _, candidate := range names {
				if s.labels["device"] == candidate {
					out[name] = s.value
				}
			}
		}
	}
	return out
}

func diskDevicesF2(families []*dto.MetricFamily) []string {
	seen := map[string]bool{}
	var devices []string
	for name, series := range seriesByNameF2(families) {
		if !strings.HasPrefix(name, "node_disk_") {
			continue
		}
		for _, s := range series {
			if !seen[s.labels["device"]] {
				seen[s.labels["device"]] = true
				devices = append(devices, s.labels["device"])
			}
		}
	}
	sort.Strings(devices)
	return devices
}

func TestWhichDisksGetDiskIOMetrics(t *testing.T) {
	t.Run("a disk mounted as /dev/<disk name> with a type not on the skip list gets its 4 counters", func(t *testing.T) {
		vm := newFakeVMF2(t)
		vm.write(t, "diskstats", diskstatsLineF2("vda1", 11, 22, 33, 44))
		vm.mounts = []*procfs.MountInfo{{MountID: 1, ParentID: 1, MountPoint: t.TempDir(), FSType: "ext4", Source: "/dev/vda1"}}

		families := collectRoundF2(t, vm, zap.NewNop())

		assert.Equal(t, map[string]float64{
			"node_disk_reads_completed_total":  11,
			"node_disk_read_bytes_total":       22 * 512,
			"node_disk_writes_completed_total": 33,
			"node_disk_written_bytes_total":    44 * 512,
		}, diskCountersF2(families, "vda1", "/dev/vda1"))
	})

	t.Run("a disk with no filesystem mounted from it gets no metric and a debug log", func(t *testing.T) {
		vm := newFakeVMF2(t)
		vm.write(t, "diskstats", diskstatsLineF2("vda1", 1, 1, 1, 1)+diskstatsLineF2("vdb", 9, 9, 9, 9))
		vm.mounts = []*procfs.MountInfo{{MountID: 1, ParentID: 1, MountPoint: t.TempDir(), FSType: "ext4", Source: "/dev/vda1"}}
		core, logs := observer.New(zapcore.DebugLevel)

		families := collectRoundF2(t, vm, zap.New(core))

		assert.Equal(t, []string{"vda1"}, diskDevicesF2(families))
		assert.NotEmpty(t, logs.Filter(func(e observer.LoggedEntry) bool {
			return e.Level == zapcore.DebugLevel && e.ContextMap()["device"] == "vdb"
		}).All())
	})

	for _, fsType := range skippedFSTypesF2 {
		t.Run("a disk whose only filesystem is of skipped type "+fsType+" gets no metric", func(t *testing.T) {
			vm := newFakeVMF2(t)
			vm.write(t, "diskstats", diskstatsLineF2("vda1", 1, 1, 1, 1)+diskstatsLineF2("loop0", 9, 9, 9, 9))
			vm.mounts = []*procfs.MountInfo{
				{MountID: 1, ParentID: 1, MountPoint: t.TempDir(), FSType: "ext4", Source: "/dev/vda1"},
				{MountID: 2, ParentID: 1, MountPoint: t.TempDir(), FSType: fsType, Source: "/dev/loop0"},
			}

			families := collectRoundF2(t, vm, zap.NewNop())

			assert.Equal(t, []string{"vda1"}, diskDevicesF2(families))
		})
	}
}

func filesystemPathsF2(families []*dto.MetricFamily) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, s := range seriesByNameF2(families)["node_filesystem_size_bytes"] {
		out[s.labels["mountpoint"]] = s.labels
	}
	return out
}

func TestWhichFilesystemsGetSpaceMetrics(t *testing.T) {
	t.Run("a path with another mount stacked on it is reported once, for the top mount", func(t *testing.T) {
		vm := newFakeVMF2(t)
		path := t.TempDir()
		// The top mount is listed first: the order of mountinfo does not tell which is on top.
		vm.mounts = []*procfs.MountInfo{
			{MountID: 1, ParentID: 1, MountPoint: "/nonexistent-root-f2", FSType: "ext4", Source: "/dev/vda1"},
			{MountID: 3, ParentID: 2, MountPoint: path, FSType: "xfs", Source: "/dev/vdc"},
			{MountID: 2, ParentID: 1, MountPoint: path, FSType: "ext4", Source: "/dev/vdb"},
		}

		families := collectRoundF2(t, vm, zap.NewNop())

		assert.Len(t, seriesByNameF2(families)["node_filesystem_size_bytes"], 1)
		assert.Equal(t, map[string]string{"device": "/dev/vdc", "fstype": "xfs", "mountpoint": path}, filesystemPathsF2(families)[path])
	})

	t.Run("a path hidden under a mount of a skipped type gets no metric", func(t *testing.T) {
		vm := newFakeVMF2(t)
		path := t.TempDir()
		vm.mounts = []*procfs.MountInfo{
			{MountID: 2, ParentID: 1, MountPoint: path, FSType: "ext4", Source: "/dev/vdb"},
			{MountID: 3, ParentID: 2, MountPoint: path, FSType: "tmpfs", Source: "/dev/vdb"},
		}

		families := collectRoundF2(t, vm, zap.NewNop())

		assert.Empty(t, filesystemPathsF2(families))
	})

	t.Run("a path already reported this round is not reported again for a second mount", func(t *testing.T) {
		vm := newFakeVMF2(t)
		path := t.TempDir()
		vm.mounts = []*procfs.MountInfo{
			{MountID: 2, ParentID: 1, MountPoint: path, FSType: "ext4", Source: "/dev/vdb"},
			{MountID: 3, ParentID: 1, MountPoint: path, FSType: "xfs", Source: "/dev/vdc"},
		}

		families := collectRoundF2(t, vm, zap.NewNop())

		assert.Len(t, seriesByNameF2(families)["node_filesystem_size_bytes"], 1)
		assert.Equal(t, "/dev/vdb", filesystemPathsF2(families)[path]["device"])
	})

	for _, fsType := range skippedFSTypesF2 {
		t.Run("a mount of skipped type "+fsType+" gets no metric and a debug log", func(t *testing.T) {
			vm := newFakeVMF2(t)
			vm.mounts = []*procfs.MountInfo{{MountID: 1, ParentID: 1, MountPoint: t.TempDir(), FSType: fsType, Source: "/dev/vdb"}}
			core, logs := observer.New(zapcore.DebugLevel)

			families := collectRoundF2(t, vm, zap.New(core))

			assert.Empty(t, filesystemPathsF2(families))
			assert.NotEmpty(t, logs.Filter(func(e observer.LoggedEntry) bool {
				return e.Level == zapcore.DebugLevel && e.ContextMap()["mountpoint"] == vm.mounts[0].MountPoint
			}).All())
		})
	}

	t.Run("a mount whose source is not a /dev/ path gets no metric and a debug log", func(t *testing.T) {
		vm := newFakeVMF2(t)
		vm.mounts = []*procfs.MountInfo{{MountID: 1, ParentID: 1, MountPoint: t.TempDir(), FSType: "ext4", Source: "rootpool/data"}}
		core, logs := observer.New(zapcore.DebugLevel)

		families := collectRoundF2(t, vm, zap.New(core))

		assert.Empty(t, filesystemPathsF2(families))
		assert.NotEmpty(t, logs.Filter(func(e observer.LoggedEntry) bool {
			return e.Level == zapcore.DebugLevel && e.ContextMap()["source"] == "rootpool/data"
		}).All())
	})

	t.Run("a mount whose size the agent cannot read gets no metric and a debug log", func(t *testing.T) {
		vm := newFakeVMF2(t)
		missing := filepath.Join(t.TempDir(), "gone")
		vm.mounts = []*procfs.MountInfo{{MountID: 1, ParentID: 1, MountPoint: missing, FSType: "ext4", Source: "/dev/vdb"}}
		core, logs := observer.New(zapcore.DebugLevel)

		families := collectRoundF2(t, vm, zap.New(core))

		assert.Empty(t, filesystemPathsF2(families))
		assert.NotEmpty(t, logs.Filter(func(e observer.LoggedEntry) bool {
			return e.Level == zapcore.DebugLevel && e.ContextMap()["mountpoint"] == missing
		}).All())
	})

	t.Run("a /dev/ mount of a type not on the skip list whose size is readable gets size, free and available bytes", func(t *testing.T) {
		vm := newFakeVMF2(t)
		path := t.TempDir()
		vm.mounts = []*procfs.MountInfo{{MountID: 1, ParentID: 1, MountPoint: path, FSType: "btrfs", Source: "/dev/vdb"}}

		series := seriesByNameF2(collectRoundF2(t, vm, zap.NewNop()))

		for _, name := range filesystemMetricsF2 {
			require.Len(t, series[name], 1, name)
			assert.Equal(t, map[string]string{"device": "/dev/vdb", "fstype": "btrfs", "mountpoint": path}, series[name][0].labels, name)
		}
	})
}

// routableInterfaceF2 finds an interface of this machine with an IPv4 address
// that is neither loopback nor link-local.
func routableInterfaceF2(t *testing.T) (name, address string) {
	t.Helper()
	interfaces, err := net.Interfaces()
	require.NoError(t, err)
	for _, iface := range interfaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if ok && ipNet.IP.To4() != nil && !ipNet.IP.IsLoopback() && !ipNet.IP.IsLinkLocalUnicast() {
				return iface.Name, ipNet.IP.String()
			}
		}
	}
	t.Skip("this machine has no interface with a routable IPv4 address")
	return "", ""
}

func TestWhichAddressANetworkInterfaceReports(t *testing.T) {
	networkSeriesFor := func(families []*dto.MetricFamily, device string) map[string]string {
		addresses := map[string]string{}
		for _, name := range networkMetricsF2 {
			for _, s := range seriesByNameF2(families)[name] {
				if s.labels["device"] == device {
					addresses[name] = s.labels["ip_address"]
				}
			}
		}
		return addresses
	}
	allFour := func(address string) map[string]string {
		out := map[string]string{}
		for _, name := range networkMetricsF2 {
			out[name] = address
		}
		return out
	}

	t.Run("the loopback interface is reported, and its 4 counters carry ip_address none", func(t *testing.T) {
		vm := newFakeVMF2(t)
		vm.write(t, "net/dev", netDevHeaderF2+netDevLineF2("lo", 1, 1, 1, 1))

		families := collectRoundF2(t, vm, zap.NewNop())

		assert.Equal(t, allFour("none"), networkSeriesFor(families, "lo"))
	})

	t.Run("an interface with no IPv4 address carries ip_address none on its 4 counters", func(t *testing.T) {
		vm := newFakeVMF2(t)
		vm.write(t, "net/dev", netDevHeaderF2+netDevLineF2("ghost0", 1, 1, 1, 1))

		families := collectRoundF2(t, vm, zap.NewNop())

		assert.Equal(t, allFour("none"), networkSeriesFor(families, "ghost0"))
	})

	t.Run("an interface with an IPv4 address that is not loopback or link-local carries that address on its 4 counters", func(t *testing.T) {
		name, address := routableInterfaceF2(t)
		vm := newFakeVMF2(t)
		vm.write(t, "net/dev", netDevHeaderF2+netDevLineF2(name, 1, 1, 1, 1))

		families := collectRoundF2(t, vm, zap.NewNop())

		assert.Equal(t, allFour(address), networkSeriesFor(families, name))
	})
}
