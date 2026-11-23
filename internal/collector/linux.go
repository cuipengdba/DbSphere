package collector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var metricLine = regexp.MustCompile(`^([a-zA-Z_:][\w:]*)(\{[^}]*\})?\s+([^\s]+)`)
var metricLabel = regexp.MustCompile(`([a-zA-Z_][\w]*)="((?:\\.|[^"\\])*)"`)
var nodeDiskMetrics = map[string]string{
	"node_disk_read_bytes_total": "readBytes", "node_disk_written_bytes_total": "writtenBytes",
	"node_disk_io_time_seconds_total": "ioSeconds", "node_disk_reads_completed_total": "reads",
	"node_disk_writes_completed_total": "writes", "node_disk_io_time_weighted_seconds_total": "weightedIOSeconds",
	"node_disk_read_time_seconds_total": "readSeconds", "node_disk_write_time_seconds_total": "writeSeconds",
}
var nodeNetworkMetrics = map[string]string{
	"node_network_receive_bytes_total": "receiveBytes", "node_network_transmit_bytes_total": "transmitBytes",
	"node_network_receive_errs_total": "receiveErrors", "node_network_transmit_errs_total": "transmitErrors",
	"node_network_receive_drop_total": "receiveDrops", "node_network_transmit_drop_total": "transmitDrops",
}
var excludedFS = map[string]bool{"tmpfs": true, "devtmpfs": true, "overlay": true,
	"squashfs": true, "proc": true, "sysfs": true, "cgroup": true, "cgroup2": true,
	"nsfs": true, "tracefs": true, "autofs": true, "securityfs": true, "configfs": true,
	"debugfs": true, "fusectl": true, "mqueue": true, "ramfs": true, "devpts": true,
	"efivarfs": true}

type filesystemSample struct {
	Mountpoint string
	Device     string
	Size       float64
	Available  float64
	HasSize    bool
	HasAvail   bool
}

func parseMetricLabels(raw string) map[string]string {
	labels := map[string]string{}
	for _, match := range metricLabel.FindAllStringSubmatch(raw, -1) {
		value, err := strconv.Unquote(`"` + match[2] + `"`)
		if err == nil {
			labels[match[1]] = value
		}
	}
	return labels
}

func parseNodeExporter(raw string) (map[string]any, error) {
	scalar := map[string]float64{}
	filesystems := map[string]*filesystemSample{}
	collected := map[string]bool{}
	disks, interfaces := map[string]map[string]float64{}, map[string]map[string]float64{}
	deviceNumbers := map[string]any{}
	var cpuTotal, cpuIdle, diskRead, diskWritten, netReceive, netTransmit float64
	var cpuIOWait float64
	for _, line := range strings.Split(raw, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		match := metricLine.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		value, err := strconv.ParseFloat(match[3], 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		labels := parseMetricLabels(match[2])
		device := labels["device"]
		validDisk := device != "" && !strings.HasPrefix(device, "loop") && !strings.HasPrefix(device, "ram") && !strings.HasPrefix(device, "zram")
		validNetwork := device != "" && device != "lo"
		if key := nodeDiskMetrics[match[1]]; key != "" && validDisk {
			addNodeDeviceCounter(disks, device, key, value)
		}
		if key := nodeNetworkMetrics[match[1]]; key != "" && validNetwork {
			addNodeDeviceCounter(interfaces, device, key, value)
		}
		switch match[1] {
		case "node_disk_info":
			if number := labels["major"] + ":" + labels["minor"]; validDisk && blockDevice.MatchString(number) && len(deviceNumbers) < 128 {
				deviceNumbers[device] = map[string]any{"number": number}
			}
		case "node_cpu_seconds_total":
			cpuTotal += value
			if labels["mode"] == "idle" {
				cpuIdle += value
			}
			if labels["mode"] == "iowait" {
				cpuIOWait += value
				collected["iowait"] = true
			}
		case "node_disk_read_bytes_total":
			if !strings.HasPrefix(labels["device"], "loop") && !strings.HasPrefix(labels["device"], "ram") &&
				!strings.HasPrefix(labels["device"], "zram") {
				diskRead += value
				collected[match[1]] = true
			}
		case "node_disk_written_bytes_total":
			if !strings.HasPrefix(labels["device"], "loop") && !strings.HasPrefix(labels["device"], "ram") &&
				!strings.HasPrefix(labels["device"], "zram") {
				diskWritten += value
				collected[match[1]] = true
			}
		case "node_network_receive_bytes_total":
			if !strings.HasPrefix(labels["device"], "lo") && !strings.HasPrefix(labels["device"], "veth") &&
				!strings.HasPrefix(labels["device"], "docker") {
				netReceive += value
				collected[match[1]] = true
			}
		case "node_network_transmit_bytes_total":
			if !strings.HasPrefix(labels["device"], "lo") && !strings.HasPrefix(labels["device"], "veth") &&
				!strings.HasPrefix(labels["device"], "docker") {
				netTransmit += value
				collected[match[1]] = true
			}
		case "node_filesystem_size_bytes", "node_filesystem_avail_bytes":
			mount := labels["mountpoint"]
			if mount == "" || excludedFS[labels["fstype"]] ||
				mount == "/etc/hosts" || mount == "/etc/hostname" || mount == "/etc/resolv.conf" {
				continue
			}
			entry := filesystems[mount]
			if entry == nil {
				entry = &filesystemSample{Mountpoint: mount, Device: labels["device"]}
				filesystems[mount] = entry
			}
			if match[1] == "node_filesystem_size_bytes" {
				entry.Size, entry.HasSize = value, true
			} else {
				entry.Available, entry.HasAvail = value, true
			}
		default:
			if match[2] == "" {
				scalar[match[1]] = value
			}
		}
	}
	if scalar["node_memory_MemTotal_bytes"] == 0 && cpuTotal == 0 {
		return nil, errors.New("node_exporter host metrics not found")
	}
	fs := []map[string]any{}
	for _, entry := range filesystems {
		if !entry.HasSize || !entry.HasAvail || entry.Size <= 0 {
			continue
		}
		fs = append(fs, map[string]any{"mountpoint": entry.Mountpoint, "device": entry.Device,
			"sizeBytes": entry.Size, "availableBytes": entry.Available,
			"usedPercent": math.Round((1-entry.Available/entry.Size)*1000) / 10})
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i]["sizeBytes"].(float64) > fs[j]["sizeBytes"].(float64) })
	if len(fs) > 10 {
		fs = fs[:10]
	}
	var memoryUsed any
	if total := scalar["node_memory_MemTotal_bytes"]; total > 0 {
		if available, ok := scalar["node_memory_MemAvailable_bytes"]; ok {
			memoryUsed = math.Round((1-available/total)*1000) / 10
		}
	}
	metric := func(name string) any {
		if value, ok := scalar[name]; ok {
			return value
		}
		return nil
	}
	counter := func(name string, value float64) any {
		if collected[name] {
			return value
		}
		return nil
	}
	return map[string]any{"status": "ok", "source": "node_exporter", "observedAt": time.Now().UTC(),
		"bootTimeSeconds": metric("node_boot_time_seconds"),
		"deviceNumbers":   deviceNumbers,
		"cpu": map[string]any{"totalSeconds": cpuTotal, "idleSeconds": cpuIdle,
			"load1": metric("node_load1"), "ioWaitSeconds": counter("iowait", cpuIOWait)},
		"memory": map[string]any{"totalBytes": scalar["node_memory_MemTotal_bytes"],
			"availableBytes": scalar["node_memory_MemAvailable_bytes"], "usedPercent": memoryUsed},
		"filesystems": fs, "devices": map[string]any{"disk": nodeDevices(disks), "network": nodeDevices(interfaces),
			"diskTotal": len(disks), "networkTotal": len(interfaces)},
		"pressure": map[string]any{"cpuWaitSeconds": metric("node_pressure_cpu_waiting_seconds_total"),
			"memoryWaitSeconds": metric("node_pressure_memory_waiting_seconds_total"), "ioWaitSeconds": metric("node_pressure_io_waiting_seconds_total")},
		"disk": map[string]any{"readBytesTotal": counter("node_disk_read_bytes_total", diskRead),
			"writtenBytesTotal": counter("node_disk_written_bytes_total", diskWritten)},
		"network": map[string]any{"receiveBytesTotal": counter("node_network_receive_bytes_total", netReceive),
			"transmitBytesTotal": counter("node_network_transmit_bytes_total", netTransmit)}}, nil
}

func addNodeDeviceCounter(devices map[string]map[string]float64, name, key string, value float64) {
	if len(name) > 128 || value < 0 {
		return
	}
	if devices[name] == nil {
		devices[name] = map[string]float64{}
	}
	devices[name][key] = value
}

func nodeDevices(devices map[string]map[string]float64) map[string]any {
	names := make([]string, 0, len(devices))
	for name := range devices {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) > 32 {
		names = names[:32]
	}
	result := make(map[string]any, len(names))
	for _, name := range names {
		counters := map[string]any{}
		for key, value := range devices[name] {
			counters[key] = value
		}
		result[name] = counters
	}
	return result
}

func Linux(ctx context.Context, host string, port int) (map[string]any, error) {
	if host == "" || port < 1 || port > 65535 {
		return nil, errors.New("Linux node_exporter endpoint is not configured")
	}
	url := "http://" + net.JoinHostPort(host, strconv.Itoa(port)) + "/metrics"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 4 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("node_exporter request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("node_exporter HTTP %d", response.StatusCode)
	}
	const maxBytes = 5 * 1024 * 1024
	data, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return nil, errors.New("node_exporter response could not be read")
	}
	if len(data) > maxBytes {
		return nil, errors.New("node_exporter response exceeds 5 MiB")
	}
	return parseNodeExporter(string(data))
}
