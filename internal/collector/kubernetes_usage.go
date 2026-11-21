package collector

import (
	"math"
	"strconv"
	"strings"
)

type kubeUsageTotals struct {
	containers, cpuCount, memoryCount int
	cpuCores, memoryBytes             float64
}

func (u *kubeUsageTotals) add(cpu, memory string) {
	u.containers++
	if cores, ok := kubeCPUCores(cpu); ok {
		u.cpuCount++
		u.cpuCores += cores
	}
	if bytes, err := KubernetesQuantityBytes(memory); err == nil || memory == "0" {
		u.memoryCount++
		u.memoryBytes += bytes
	}
}

func (u kubeUsageTotals) values() map[string]any {
	result := map[string]any{"containers": u.containers}
	if u.containers > 0 && u.cpuCount == u.containers {
		result["cpuCores"] = u.cpuCores
	}
	if u.containers > 0 && u.memoryCount == u.containers {
		result["memoryBytes"] = u.memoryBytes
	}
	return result
}

func kubeCPUCores(value string) (float64, bool) {
	factor := 1.0
	for suffix, scale := range map[string]float64{"n": 1e-9, "u": 1e-6, "m": 1e-3} {
		if strings.HasSuffix(value, suffix) {
			value = strings.TrimSuffix(value, suffix)
			factor = scale
			break
		}
	}
	amount, err := strconv.ParseFloat(value, 64)
	return amount * factor, err == nil && amount >= 0 && !math.IsNaN(amount) && !math.IsInf(amount, 0)
}
