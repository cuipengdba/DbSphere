package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"time"
)

var cacheMetricNames = map[string]string{
	"active_file": "active_file_bytes", "inactive_file": "inactive_file_bytes", "file": "file_cache_bytes",
	"file_dirty": "file_dirty_bytes", "file_writeback": "file_writeback_bytes",
}
var rateMetricNames = map[string]string{
	"readBps": "read_bytes_per_second", "writeBps": "write_bytes_per_second", "readIops": "read_operations_per_second", "writeIops": "write_operations_per_second",
	"refaultPerSecond": "refault_per_second", "reclaimsPerSecond": "reclaims_per_second", "scansPerSecond": "scans_per_second",
}
var pressureMetricNames = map[string]string{
	"io_some_avg10": "io_pressure_percent", "io_full_avg10": "io_full_pressure_percent", "memory_some_avg10": "memory_pressure_percent",
}

func writeContainerMetrics(w io.Writer, assetLabels string, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var data struct {
		Storage struct {
			Node, Primary, Pod, Namespace string
			PGIOWait                      *float64
			CadenceSeconds                float64
			PVCs                          []struct {
				Name, Namespace, Phase string
				Usage                  map[string]any
			}
			Containers map[string]struct {
				Source, Status          string
				ObservedAt              time.Time
				Memory, Rates, Pressure map[string]float64
				DeviceRates             map[string]map[string]float64
			}
		}
		Node                    map[string]map[string]float64
		NodeNumbers             map[string]struct{ Number string }
		PGDatabase, Checkpoints map[string]any
		WAL                     map[string]any
	}
	if json.Unmarshal(raw, &data) != nil {
		return
	}
	quote := func(value string) string { return `"` + prometheusLabelEscapes.Replace(value) + `"` }
	base := assetLabels + ",node=" + quote(data.Storage.Node) + ",namespace=" + quote(data.Storage.Namespace) + ",pod=" + quote(data.Storage.Pod)
	write := func(name, labels string, value float64) {
		if !math.IsInf(value, 0) && !math.IsNaN(value) {
			fmt.Fprintf(w, "dbsphere_container_%s{%s} %g\n", name, labels, value)
		}
	}
	fields := func(values map[string]float64, keys map[string]string, labels string) {
		for key, metric := range keys {
			if value, ok := values[key]; ok {
				write(metric, labels, value)
			}
		}
	}
	for name, row := range data.Storage.Containers {
		labels := base + ",container=" + quote(name) + ",source=" + quote(row.Source)
		if name == data.Storage.Primary {
			labels += `,primary="true"`
		} else {
			labels += `,primary="false"`
		}
		success := row.Status == "ok" && time.Since(row.ObservedAt).Seconds() <= max(30, data.Storage.CadenceSeconds*2)
		if !success {
			write("sample_available", labels, 0)
			continue
		}
		write("sample_available", labels, 1)
		fields(row.Memory, cacheMetricNames, labels)
		fields(row.Rates, rateMetricNames, labels+`,device=""`)
		fields(row.Pressure, pressureMetricNames, labels)
		for device, rates := range row.DeviceRates {
			fields(rates, rateMetricNames, labels+",device="+quote(device))
		}
	}
	if data.Storage.PGIOWait != nil {
		write("pg_io_wait_sessions", base, *data.Storage.PGIOWait)
	}
	for _, signal := range []struct {
		values      map[string]any
		key, metric string
	}{
		{data.PGDatabase, "readTimeMs", "pg_read_time_seconds_total"}, {data.PGDatabase, "writeTimeMs", "pg_write_time_seconds_total"},
		{data.PGDatabase, "tempBytes", "pg_temp_bytes_total"}, {data.Checkpoints, "syncMs", "pg_checkpoint_sync_seconds_total"},
	} {
		if value, ok := signal.values[signal.key].(float64); ok {
			if signal.key != "tempBytes" {
				value /= 1000
			}
			write(signal.metric, base, value)
		}
	}
	if raw, ok := data.WAL["bytes"].(string); ok {
		if value, err := strconv.ParseFloat(raw, 64); err == nil {
			write("pg_wal_bytes_total", base, value)
		}
	}
	for device, values := range data.Node {
		labels := base + ",device=" + quote(device) + ",device_number=" + quote(data.NodeNumbers[device].Number)
		fields(values, map[string]string{"ioSeconds": "node_device_busy_ratio", "weightedIOSeconds": "node_device_average_queue", "readBytes": "node_read_bytes_per_second", "writtenBytes": "node_write_bytes_per_second"}, labels)
		read, readOK := values["readSeconds"]
		written, writeOK := values["writeSeconds"]
		reads, readsOK := values["reads"]
		writes, writesOK := values["writes"]
		if readOK && writeOK && readsOK && writesOK && reads+writes > 0 {
			write("node_device_wait_seconds", labels, (read+written)/(reads+writes))
		}
	}
	for _, pvc := range data.Storage.PVCs {
		if pvc.Name == "" {
			continue
		}
		labels := base + ",pvc=" + quote(pvc.Name)
		if pvc.Phase == "Bound" {
			write("pvc_bound", labels, 1)
		} else if pvc.Phase != "" {
			write("pvc_bound", labels, 0)
		}
		for key, name := range map[string]string{"usedBytes": "pvc_used_bytes", "capacityBytes": "pvc_capacity_bytes", "usedPercent": "pvc_used_percent"} {
			if value, ok := pvc.Usage[key].(float64); ok {
				write(name, labels, value)
			}
		}
	}
}
