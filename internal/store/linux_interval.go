package store

import "time"

func recordLinuxInterval(current, previous map[string]any, elapsed time.Duration) {
	if current["status"] != "ok" || previous["status"] != "ok" || elapsed < time.Second || elapsed > 10*time.Minute {
		return
	}
	boot, bootKnown := numeric(current["bootTimeSeconds"])
	priorBoot, priorBootKnown := numeric(previous["bootTimeSeconds"])
	if !bootKnown || !priorBootKnown || boot != priorBoot || current["endpoint"] != previous["endpoint"] {
		return
	}
	interval := map[string]any{}
	for _, counter := range []struct{ section, name, key string }{
		{"disk", "readBytesTotal", "diskReadBps"}, {"disk", "writtenBytesTotal", "diskWriteBps"},
		{"network", "receiveBytesTotal", "networkReceiveBps"}, {"network", "transmitBytesTotal", "networkTransmitBps"},
		{"pressure", "cpuWaitSeconds", "cpuWait"}, {"pressure", "memoryWaitSeconds", "memoryWait"},
		{"pressure", "ioWaitSeconds", "ioWait"},
	} {
		if change, ok := hostCounterChange(current, previous, counter.section, counter.name); ok {
			interval[counter.key] = change / elapsed.Seconds()
		}
	}
	total, totalKnown := hostCounterChange(current, previous, "cpu", "totalSeconds")
	idle, idleKnown := hostCounterChange(current, previous, "cpu", "idleSeconds")
	if totalKnown && idleKnown && total > 0 && idle <= total {
		object(current["cpu"])["usedPercent"] = 100 * (total - idle) / total
	}
	current["interval"] = interval
}

func hostCounterChange(current, previous map[string]any, section, key string) (float64, bool) {
	now, nowKnown := numeric(object(current[section])[key])
	prior, priorKnown := numeric(object(previous[section])[key])
	return now - prior, nowKnown && priorKnown && now >= prior
}
