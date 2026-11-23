package store

import (
	"math"
	"sort"
	"time"
)

func containerClaimLinks(kube map[string]any, targets []storageTarget) []map[string]any {
	mounts, _ := kube["containerClaims"].(map[string][]string)
	pvcs := map[string]map[string]any{}
	for _, raw := range list(kube["pvcs"]) {
		claim := object(raw)
		pvcs[stringValue(claim["name"])] = claim
	}
	users := map[string]int{}
	for _, claims := range mounts {
		for _, claim := range claims {
			users[claim]++
		}
	}
	links := []map[string]any{}
	for _, container := range targets {
		for _, name := range mounts[container.name] {
			claim := pvcs[name]
			status := "declared_only"
			if claim["status"] == "ok" && stringValue(claim["uid"]) != "" {
				status = "claim_verified"
			}
			links = append(links, map[string]any{
				"container": container.name, "containerId": container.id, "pvc": name,
				"pvcUID": claim["uid"], "pvUID": object(claim["volume"])["uid"], "status": status,
				"sharedInPod": users[name] > 1, "observedAt": kube["observedAt"],
			})
		}
	}
	return links
}

func containerDeviceLinks(storage, linux, container map[string]any) []map[string]any {
	links := []map[string]any{}
	endpoint := stringValue(storage["nodeMetricsEndpoint"])
	if endpoint == "" || linux["endpoint"] != endpoint {
		return links
	}
	for device := range object(container["devices"]) {
		names := []string{}
		for name, raw := range object(linux["deviceNumbers"]) {
			if object(raw)["number"] == device {
				names = append(names, name)
			}
		}
		if len(names) != 1 {
			continue
		}
		rates := object(object(object(linux["deviceRates"])["disk"])[names[0]])
		links = append(links, map[string]any{"device": device, "nodeDevice": names[0], "scope": "node_device",
			"from": container["intervalStart"], "to": container["observedAt"], "queueDepth": rates["weightedIOSeconds"],
			"busyRatio": rates["ioSeconds"]})
	}
	sort.Slice(links, func(i, j int) bool {
		return stringValue(links[i]["device"]) < stringValue(links[j]["device"])
	})
	return links
}

type pvcCapacityPoint struct {
	At        time.Time `json:"at"`
	SampleID  string    `json:"sampleId"`
	UID       string    `json:"uid"`
	Name      string    `json:"name"`
	VolumeUID string    `json:"volumeUid"`
	Source    string    `json:"source"`
	Used      float64   `json:"used"`
	Capacity  float64   `json:"capacity"`
}

// Points are returned newest first, one point per observed hour and claim UID.
// Start a new baseline when the metric source, volume or capacity changes.
func pvcCapacityEstimates(points []pvcCapacityPoint) []map[string]any {
	type trace struct {
		latest, first pvcCapacityPoint
		count         int
		boundary      bool
	}
	history := map[string]*trace{}
	order := []string{}
	for _, point := range points {
		if point.At.IsZero() || point.UID == "" || point.Source == "" || point.Capacity <= 0 || point.Used < 0 {
			continue
		}
		series := history[point.UID]
		if series == nil {
			history[point.UID] = &trace{latest: point, first: point, count: 1}
			order = append(order, point.UID)
			continue
		}
		if series.boundary {
			continue
		}
		if series.latest.Capacity != point.Capacity || series.latest.Source != point.Source ||
			series.latest.VolumeUID != point.VolumeUID {
			series.boundary = true
			continue
		}
		if point.At.Before(series.first.At) {
			series.first = point
			series.count++
		}
	}
	result := []map[string]any{}
	for _, uid := range order {
		series := history[uid]
		last, first := series.latest, series.first
		hours := last.At.Sub(first.At).Hours()
		remaining := max(0, last.Capacity-last.Used)
		row := map[string]any{"pvcUID": uid, "pvc": last.Name, "capacityBytes": last.Capacity, "remainingBytes": remaining,
			"at": last.At, "sampleId": last.SampleID, "observations": series.count, "status": "insufficient_history"}
		if series.count >= 4 && hours >= 24 {
			rate := (last.Used - first.Used) / hours
			row["baselineAt"], row["baselineSampleId"], row["status"] = first.At, first.SampleID, "no_growth"
			if rate > 0 {
				if hoursToFull := remaining / rate; !math.IsInf(hoursToFull, 0) && !math.IsInf(rate*24, 0) {
					row["bytesPerDay"], row["hoursToFull"], row["status"] = rate*24, hoursToFull, "estimated"
				}
			}
		}
		result = append(result, row)
	}
	return result
}
