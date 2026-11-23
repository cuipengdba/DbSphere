package store

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/cuipengdba/dbsphere/internal/collector"
)

type storageTarget struct{ name, id string }

func (s *Postgres) collectContainerStorage(ctx context.Context, asset, runtime string, port int, kube map[string]any) (map[string]any, map[string]any) {
	cfg := s.containerStorage
	meta := map[string]any{"status": "unbound", "attributionVersion": 1}
	var targets []storageTarget
	var nodeKey, primary string
	if runtime == "k8s" {
		pod := object(kube["pod"])
		node, cluster := stringValue(pod["node"]), stringValue(kube["cluster"])
		meta["runtime"], meta["podUID"], meta["node"] = "k8s", pod["uid"], node
		meta["cluster"], meta["namespace"], meta["pod"] = cluster, kube["namespace"], pod["name"]
		meta["nodeUID"] = object(kube["node"])["uid"]
		meta["workload"], meta["pvcs"] = kube["workload"], kube["pvcs"]
		meta["volumeMapping"] = "claim_attachment_only"
		if pod["uid"] == nil || kube["status"] != "ok" {
			return meta, nil
		}
		ports, _ := kube["containerPorts"].(map[string][]int)
		candidates := []string{}
		for _, raw := range list(kube["containers"]) {
			row := object(raw)
			name, id := stringValue(row["name"]), stringValue(row["runtimeId"])
			if id == "" || name == "" || len(targets) == 8 {
				continue
			}
			targets = append(targets, storageTarget{name, id})
			if slices.Contains(ports[name], port) {
				candidates = append(candidates, name)
			}
		}
		primary = cfg.Primary[asset]
		if primary == "" && len(candidates) == 1 {
			primary = candidates[0]
		}
		if primary == "" && len(targets) == 1 {
			primary = targets[0].name
		}
		nodeKey = cluster + "/" + node
		meta["volumeLinks"] = containerClaimLinks(kube, targets)
	} else {
		binding, exists := cfg.Docker[asset]
		if runtime != "linux" || !exists {
			return nil, nil
		}
		meta["runtime"], meta["node"] = "docker", binding.Host
		primary, nodeKey = "postgresql", binding.Host
		targets = []storageTarget{{primary, binding.ContainerID}}
	}
	meta["primary"] = primary
	source := cfg.Nodes[nodeKey]
	meta["nodeMetricsEndpoint"] = source.NodeExporter
	if source.URL == "" || len(targets) == 0 {
		return meta, nil
	}
	var wait sync.WaitGroup
	observed := make([]collector.ContainerPoll, len(targets))
	for index, target := range targets {
		wait.Go(func() { observed[index] = cfg.Poll(ctx, source, target.id) })
	}
	var host map[string]any
	if source.NodeExporter != "" {
		wait.Go(func() { host, _ = source.Linux(ctx) })
	}
	wait.Wait()
	meta["status"] = "unavailable"
	containers := map[string]any{}
	for index, target := range targets {
		poll := observed[index]
		raw, _ := json.Marshal(poll)
		row := map[string]any{}
		if json.Unmarshal(raw, &row) != nil {
			continue
		}
		containers[target.name] = row
		if target.name == primary || (primary == "" && poll.Status == "ok") {
			meta["status"] = poll.Status
		}
	}
	meta["containers"] = containers
	return meta, host
}

var storageCounters = map[string]string{"rbytes": "readBps", "wbytes": "writeBps", "rios": "readIops", "wios": "writeIops"}
var storageMemoryRates = map[string]string{"workingset_refault_file": "refaultPerSecond", "pgsteal": "reclaimsPerSecond", "pgscan": "scansPerSecond"}

func storageDelta(next, previous map[string]any, key string) (float64, bool) {
	a, aOK := numeric(next[key])
	b, bOK := numeric(previous[key])
	return a - b, aOK && bOK
}

func storageTime(value any) time.Time {
	at, _ := time.Parse(time.RFC3339Nano, stringValue(value))
	return at
}

func recordContainerStorage(current, previous map[string]any, cadence time.Duration) {
	data, before := object(current["containerStorage"]), object(previous["containerStorage"])
	if data == nil {
		return
	}
	maxGap := 2 * cadence
	if maxGap < 30*time.Second {
		maxGap = 30 * time.Second
	}
	if maxGap > 10*time.Minute {
		maxGap = 10 * time.Minute
	}
	data["cadenceSeconds"] = cadence.Seconds()
	if data["cadenceSeconds"] == 0.0 {
		data["cadenceSeconds"] = 60
	}
	ioWait, waitsKnown := storageIOWait(current)
	if waitsKnown {
		data["pgIOWait"] = ioWait
	}
	for name, entry := range object(data["containers"]) {
		cur, prev := object(entry), object(object(before["containers"])[name])
		if len(prev) == 0 || cur["id"] != prev["id"] || data["node"] != before["node"] ||
			data["podUID"] != before["podUID"] || cur["generation"] != prev["generation"] {
			if len(prev) > 0 {
				cur["identityChanged"] = true
			}
			continue
		}
		if cur["source"] != prev["source"] || cur["status"] != "ok" || prev["status"] != "ok" {
			continue
		}
		elapsed := storageTime(cur["observedAt"]).Sub(storageTime(prev["observedAt"]))
		if elapsed < time.Second || elapsed > maxGap {
			continue
		}
		cur["intervalStart"] = prev["observedAt"]
		seconds := elapsed.Seconds()
		deviceRates, rates := map[string]any{}, map[string]any{}
		for id, raw := range object(cur["devices"]) {
			old := object(object(prev["devices"])[id])
			points := map[string]any{}
			for counter, rate := range storageCounters {
				if delta, ok := storageDelta(object(raw), old, counter); ok && delta >= 0 {
					points[rate] = delta / seconds
					// A reset or missing device leaves a partial total explicitly marked.
					total, _ := numeric(rates[rate])
					rates[rate] = total + delta/seconds
				} else if delta < 0 {
					cur["counterReset"] = true
				}
			}
			if len(points) > 0 {
				deviceRates[id] = points
			}
		}
		for _, rate := range storageCounters {
			for device := range object(cur["devices"]) {
				if object(deviceRates[device])[rate] == nil {
					delete(rates, rate)
					break
				}
			}
		}
		cur["deviceRates"], cur["rates"] = deviceRates, rates
		for counter, rate := range storageMemoryRates {
			if delta, ok := storageDelta(object(cur["memory"]), object(prev["memory"]), counter); ok && delta >= 0 {
				rates[rate] = delta / seconds
			}
		}
		for _, name := range []string{"active_file", "inactive_file"} {
			if delta, ok := storageDelta(object(cur["memory"]), object(prev["memory"]), name); ok {
				rates[name+"Delta"] = delta
			}
		}
	}
	for _, entry := range object(data["containers"]) {
		row := object(entry)
		row["deviceLinks"] = containerDeviceLinks(data, object(current["linux"]), row)
	}
	data["finding"] = containerStorageFinding(data, current, previous)
}

func storageIOWait(metrics map[string]any) (float64, bool) {
	activity := object(object(metrics["diagnostics"])["activity"])
	if activity["status"] != "ok" || (metrics["monitor_role"] != true && metrics["stats_visible"] != true) {
		return 0, false
	}
	count := 0.0
	for _, row := range list(activity["data"]) {
		m := object(row)
		if m["state"] == "active" && m["waitType"] == "IO" {
			n, _ := numeric(m["sessions"])
			count += n
		}
	}
	return count, true
}

var storageFindings = map[string]string{
	"cache_reread": "文件缓存回收伴随重读", "writeback": "持续写回压力",
	"io_limit": "容器块设备 I/O 接近已配置限流", "node_congestion": "同一节点设备出现 I/O 等待",
}

func containerStorageFinding(data, current, previous map[string]any) map[string]any {
	row := object(object(data["containers"])[stringValue(data["primary"])])
	if row["status"] != "ok" || row["rates"] == nil || row["counterReset"] == true {
		return nil
	}
	rates, memory, pressure := object(row["rates"]), object(row["memory"]), object(row["pressure"])
	psi, psiKnown := numeric(pressure["io_some_avg10"])
	waits, waitsKnown := numeric(data["pgIOWait"])
	if !psiKnown || !waitsKnown {
		return nil
	}
	matched := []string{}
	busyDevices := storageBusyDevices(current, row)
	if psi >= 1 && waits > 0 {
		active, activeKnown := numeric(rates["active_fileDelta"])
		inactive, inactiveKnown := numeric(rates["inactive_fileDelta"])
		if number(rates["refaultPerSecond"]) > 0 && number(rates["readBps"]) > 0 &&
			(number(rates["reclaimsPerSecond"]) > 0 || (activeKnown && inactiveKnown && active+inactive < 0)) {
			matched = append(matched, "cache_reread")
		}
		oldStorage := object(previous["containerStorage"])
		oldPrimary := object(object(oldStorage["containers"])[stringValue(oldStorage["primary"])])
		if number(memory["file_writeback"]) > 0 && number(object(oldPrimary["memory"])["file_writeback"]) > 0 && number(rates["writeBps"]) > 0 {
			matched = append(matched, "writeback")
		}
		limitMatched := false
		for id, raw := range object(row["limits"]) {
			deviceRates := object(object(row["deviceRates"])[id])
			for limit, rate := range map[string]string{"rbps": "readBps", "wbps": "writeBps", "riops": "readIops", "wiops": "writeIops"} {
				if cap := number(object(raw)[limit]); cap > 0 && number(deviceRates[rate]) >= 0.9*cap {
					limitMatched = true
				}
			}
		}
		if limitMatched {
			matched = append(matched, "io_limit")
		}
		if len(busyDevices) > 0 {
			matched = append(matched, "node_congestion")
		}
	}
	return map[string]any{"codes": matched, "pgIOWait": waits, "ioPressure": psi, "rates": rates,
		"intervalStart": row["intervalStart"], "counterReset": row["counterReset"],
		"observedAt": row["observedAt"], "source": row["source"], "containerId": row["id"], "node": data["node"],
		"volumeMapping": data["volumeMapping"], "nodeDevices": busyDevices}
}

func storageBusyDevices(metrics, container map[string]any) []string {
	linux := object(metrics["linux"])
	storage := object(metrics["containerStorage"])
	if storage["attributionVersion"] != nil {
		matched := []string{}
		for _, link := range containerDeviceLinks(storage, linux, container) {
			if number(link["busyRatio"]) >= .8 && object(container["deviceRates"])[stringValue(link["device"])] != nil {
				matched = append(matched, stringValue(link["nodeDevice"]))
			}
		}
		return matched
	}
	matched := []string{}
	for device := range object(container["deviceRates"]) {
		for name, raw := range object(object(linux["deviceRates"])["disk"]) {
			if object(object(linux["deviceNumbers"])[name])["number"] != device {
				continue
			}
			if number(object(raw)["ioSeconds"]) >= 0.8 {
				matched = append(matched, name)
			}
		}
	}
	sort.Strings(matched)
	return matched
}

func containerStorageAlert(metrics map[string]any, key string) alertSignal {
	data := object(metrics["containerStorage"])
	finding := object(data["finding"])
	if finding == nil {
		return alertSignal{Source: "Container storage", Reason: "container, PostgreSQL, or pressure evidence unavailable"}
	}
	value := 0.0
	for _, code := range list(finding["codes"]) {
		if code == key {
			value = 1
		}
	}
	return alertSignal{Available: true, Value: value, Source: "Container storage", Detail: finding}
}
