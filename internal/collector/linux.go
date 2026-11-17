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
	var cpuTotal, cpuIdle, diskRead, diskWritten, netReceive, netTransmit float64
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
		switch match[1] {
		case "node_cpu_seconds_total":
			cpuTotal += value
			if labels["mode"] == "idle" {
				cpuIdle += value
			}
		case "node_disk_read_bytes_total":
			if !strings.HasPrefix(labels["device"], "loop") && !strings.HasPrefix(labels["device"], "ram") &&
				!strings.HasPrefix(labels["device"], "zram") {
				diskRead += value
			}
		case "node_disk_written_bytes_total":
			if !strings.HasPrefix(labels["device"], "loop") && !strings.HasPrefix(labels["device"], "ram") &&
				!strings.HasPrefix(labels["device"], "zram") {
				diskWritten += value
			}
		case "node_network_receive_bytes_total":
			if !strings.HasPrefix(labels["device"], "lo") && !strings.HasPrefix(labels["device"], "veth") &&
				!strings.HasPrefix(labels["device"], "docker") {
				netReceive += value
			}
		case "node_network_transmit_bytes_total":
			if !strings.HasPrefix(labels["device"], "lo") && !strings.HasPrefix(labels["device"], "veth") &&
				!strings.HasPrefix(labels["device"], "docker") {
				netTransmit += value
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
	return map[string]any{"status": "ok", "source": "node_exporter", "observedAt": time.Now().UTC(),
		"cpu": map[string]any{"totalSeconds": cpuTotal, "idleSeconds": cpuIdle,
			"load1": scalar["node_load1"]},
		"memory": map[string]any{"totalBytes": scalar["node_memory_MemTotal_bytes"],
			"availableBytes": scalar["node_memory_MemAvailable_bytes"], "usedPercent": memoryUsed},
		"filesystems": fs,
		"disk":        map[string]any{"readBytesTotal": diskRead, "writtenBytesTotal": diskWritten},
		"network":     map[string]any{"receiveBytesTotal": netReceive, "transmitBytesTotal": netTransmit}}, nil
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
