package collector

import (
	"context"
	"os"
	"strconv"
	"testing"
)

func TestNodeExporterParsesHostAndFiltersVirtualFilesystem(t *testing.T) {
	data := `node_cpu_seconds_total{cpu="0",mode="idle"} 100
node_cpu_seconds_total{cpu="0",mode="user"} 50
node_memory_MemTotal_bytes 1000
node_memory_MemAvailable_bytes 400
node_filesystem_size_bytes{device="/dev/sda",fstype="ext4",mountpoint="/data"} 100
node_filesystem_avail_bytes{device="/dev/sda",fstype="ext4",mountpoint="/data"} 25
node_filesystem_size_bytes{device="overlay",fstype="overlay",mountpoint="/"} 100
node_filesystem_avail_bytes{device="overlay",fstype="overlay",mountpoint="/"} 1
node_disk_read_bytes_total{device="sda"} 120
node_disk_read_bytes_total{device="loop0"} 999
`
	value, err := parseNodeExporter(data)
	if err != nil {
		t.Fatal(err)
	}
	if value["memory"].(map[string]any)["usedPercent"] != 60.0 ||
		value["disk"].(map[string]any)["readBytesTotal"] != 120.0 {
		t.Fatalf("host metrics were not parsed: %v", value)
	}
	filesystems := value["filesystems"].([]map[string]any)
	if len(filesystems) != 1 || filesystems[0]["mountpoint"] != "/data" ||
		filesystems[0]["usedPercent"] != 75.0 {
		t.Fatalf("virtual filesystem leaked into disk signals: %v", filesystems)
	}
}

func TestRealNodeExporter(t *testing.T) {
	portText := os.Getenv("DBSPHERE_TEST_NODE_EXPORTER_PORT")
	if portText == "" {
		t.Skip("node_exporter endpoint is required")
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	value, err := Linux(context.Background(), "127.0.0.1", port)
	if err != nil {
		t.Fatal(err)
	}
	if value["status"] != "ok" || value["source"] != "node_exporter" ||
		value["cpu"].(map[string]any)["totalSeconds"].(float64) <= 0 {
		t.Fatalf("real node_exporter returned no host metrics: %v", value)
	}
}
