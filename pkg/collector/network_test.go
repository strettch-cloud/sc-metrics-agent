package collector

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/procfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

const netDevHeader = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
`

// Linux allows any bytes except "/" and whitespace in an interface name, but a
// Prometheus label value must be valid UTF-8. Such a name used to panic inside
// Gather and crash the whole agent on every collection.
func TestNetworkCollectorKeepsAnInterfaceNameThatIsNotValidUTF8(t *testing.T) {
	procRoot := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(procRoot, "net"), 0o755))
	netDev := netDevHeader + "eth\xff1: 100 2 0 0 0 0 0 0 200 3 0 0 0 0 0 0\n"
	require.NoError(t, os.WriteFile(filepath.Join(procRoot, "net", "dev"), []byte(netDev), 0o644))
	procFS, err := procfs.NewFS(procRoot)
	require.NoError(t, err)
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(&networkCollector{procFS: procFS, logger: zaptest.NewLogger(t)})

	families, err := registry.Gather()

	require.NoError(t, err)
	var devices []string
	for _, family := range families {
		if family.GetName() != "node_network_receive_bytes_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "device" {
					devices = append(devices, label.GetValue())
				}
			}
		}
	}
	assert.Equal(t, []string{"eth�1"}, devices)
}
