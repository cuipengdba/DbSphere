package store

import (
	"math"
	"sort"
	"time"
)

const riskGiB = float64(1024 * 1024 * 1024)

type riskHour struct {
	At            time.Time
	SampleID      string
	Connections   *float64
	DatabaseBytes *float64
	WALRate       *float64
	Planned       bool
}

type riskSignals struct {
	Hours          []riskHour
	Planned        bool
	RecentFailures int
	FailureEvent   string
}

type RiskForecast struct {
	Metric        string  `json:"metric"`
	Status        string  `json:"status"`
	Reason        string  `json:"reason"`
	Observed      float64 `json:"observed"`
	Baseline      float64 `json:"baseline"`
	Threshold     float64 `json:"threshold"`
	Projected     float64 `json:"projected"`
	BaselineHours int     `json:"baselineHours"`
	LeadMinutes   int     `json:"leadMinutes"`
	HorizonHours  int     `json:"horizonHours"`
	SampleID      string  `json:"sampleId,omitempty"`
	SourceEventID string  `json:"sourceEventId,omitempty"`
}

type riskSpec struct {
	name    string
	title   string
	group   string
	horizon time.Duration
	floor   func(float64) float64
	value   func(riskHour) *float64
}

var forecastSpecs = []riskSpec{
	{"connections", "连接占用预计接近阈值", "capacity", 4 * time.Hour,
		func(float64) float64 { return 85 }, func(h riskHour) *float64 { return h.Connections }},
	{"capacity", "数据库容量增长预计偏高", "storage", 48 * time.Hour,
		func(mid float64) float64 { return mid + math.Max(10*riskGiB, mid*.15) }, func(h riskHour) *float64 { return h.DatabaseBytes }},
	{"wal", "WAL 生成速率预计偏高", "storage", 4 * time.Hour,
		func(mid float64) float64 { return math.Max(16*1024*1024, mid*1.75) }, func(h riskHour) *float64 { return h.WALRate }},
}

type riskValue struct {
	bucket riskHour
	value  float64
}

type RiskQuality struct {
	Metric            string `json:"metric"`
	Evaluated         int    `json:"evaluated"`
	Unknown           int    `json:"unknown"`
	TruePositive      int    `json:"truePositive"`
	FalsePositive     int    `json:"falsePositive"`
	FalseNegative     int    `json:"falseNegative"`
	TrueNegative      int    `json:"trueNegative"`
	MedianLeadMinutes int    `json:"medianLeadMinutes"`
}

func assessRiskQuality(spec riskSpec, signals riskSignals, through time.Time, days int) RiskQuality {
	quality := RiskQuality{Metric: spec.name}
	var measuredLead []float64
	for at := through.AddDate(0, 0, -days); !at.Add(spec.horizon).After(through); at = at.Add(time.Hour) {
		historical := signals
		historical.Planned = false
		for _, bucket := range signals.Hours {
			if bucket.At.Equal(at.Add(-time.Hour)) && bucket.Planned {
				historical.Planned = true
			}
		}
		prediction := evaluateRiskHour(spec, at, historical)
		if prediction.Status != "risk" && prediction.Status != "clear" {
			quality.Unknown++
			continue
		}
		futureCount := 0
		var breached time.Time
		for _, bucket := range signals.Hours {
			value := spec.value(bucket)
			if bucket.At.Before(at) || !bucket.At.Before(at.Add(spec.horizon)) || bucket.Planned ||
				value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) {
				continue
			}
			futureCount++
			if breached.IsZero() && *value >= prediction.Threshold {
				breached = bucket.At
			}
		}
		if futureCount < int(math.Ceil(spec.horizon.Hours()*.75)) {
			quality.Unknown++
			continue
		}
		quality.Evaluated++
		switch {
		case prediction.Status == "risk" && !breached.IsZero():
			quality.TruePositive++
			measuredLead = append(measuredLead, breached.Sub(at).Minutes())
		case prediction.Status == "risk":
			quality.FalsePositive++
		case !breached.IsZero():
			quality.FalseNegative++
		default:
			quality.TrueNegative++
		}
	}
	if len(measuredLead) > 0 {
		quality.MedianLeadMinutes = int(riskMedian(measuredLead))
	}
	return quality
}

func riskMedian(values []float64) float64 {
	sort.Float64s(values)
	middle := len(values) / 2
	if len(values)%2 == 1 {
		return values[middle]
	}
	return (values[middle-1] + values[middle]) / 2
}

func riskWeekend(at time.Time) bool {
	return at.Weekday() == time.Saturday || at.Weekday() == time.Sunday
}

func riskCycleDistance(a, b int) int {
	diff := max(a, b) - min(a, b)
	return min(diff, 24-diff)
}

func evaluateRiskHour(spec riskSpec, hour time.Time, signals riskSignals) RiskForecast {
	result := RiskForecast{Metric: spec.name, Status: "insufficient", Reason: "recent_samples_missing", HorizonHours: int(spec.horizon.Hours())}
	if signals.Planned {
		result.Status, result.Reason = "planned", "maintenance_window"
		return result
	}
	var recent []riskValue
	for _, bucket := range signals.Hours {
		value := spec.value(bucket)
		if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || bucket.Planned || !bucket.At.Before(hour) ||
			bucket.At.Before(hour.Add(-5*time.Hour)) {
			continue
		}
		recent = append(recent, riskValue{bucket, *value})
	}
	if len(recent) < 3 || recent[len(recent)-1].bucket.At.Before(hour.Add(-2*time.Hour)) {
		return result
	}
	current := recent[len(recent)-1]
	result.Observed, result.SampleID = current.value, current.bucket.SampleID
	var typical []float64
	for _, bucket := range signals.Hours {
		value := spec.value(bucket)
		if value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) || bucket.Planned ||
			!bucket.At.Before(hour.Add(-8*time.Hour)) || riskWeekend(bucket.At) != riskWeekend(current.bucket.At) ||
			riskCycleDistance(bucket.At.Hour(), current.bucket.At.Hour()) > 1 {
			continue
		}
		typical = append(typical, *value)
	}
	result.BaselineHours = len(typical)
	if len(typical) < 6 {
		result.Reason = "seasonal_baseline_missing"
		return result
	}
	center := riskMedian(typical)
	for i, value := range typical {
		typical[i] = math.Abs(value - center)
	}
	spread := riskMedian(typical)
	result.Baseline = center
	result.Threshold = spec.floor(center)
	if spec.name != "connections" {
		result.Threshold = math.Max(result.Threshold, center+4*spread)
	}
	var slopes []float64
	for i := 1; i < len(recent); i++ {
		hours := recent[i].bucket.At.Sub(recent[i-1].bucket.At).Hours()
		if hours > 0 && hours <= 2 {
			slopes = append(slopes, (recent[i].value-recent[i-1].value)/hours)
		}
	}
	if len(slopes) < 2 {
		result.Reason = "continuous_samples_missing"
		return result
	}
	slope := riskMedian(slopes)
	result.Projected = math.Max(0, current.value+slope*spec.horizon.Hours())
	if spec.name == "connections" {
		result.Projected = math.Min(100, result.Projected)
	}
	result.Status, result.Reason = "clear", "within_baseline"
	if current.value >= result.Threshold {
		result.Status, result.Reason = "risk", "threshold_already_reached"
	} else if slope > 0 && result.Projected >= result.Threshold {
		result.Status, result.Reason = "risk", "rising_towards_threshold"
		result.LeadMinutes = int(math.Ceil((result.Threshold - current.value) / slope * 60))
	}
	return result
}

func evaluateRisks(hour time.Time, signals riskSignals) []RiskForecast {
	result := make([]RiskForecast, 0, len(forecastSpecs)+1)
	for _, spec := range forecastSpecs {
		result = append(result, evaluateRiskHour(spec, hour, signals))
	}
	repeat := RiskForecast{Metric: "recurrence", Status: "clear", Reason: "within_baseline", Observed: float64(signals.RecentFailures),
		Threshold: 3, Projected: float64(signals.RecentFailures), SourceEventID: signals.FailureEvent}
	switch {
	case signals.Planned:
		repeat.Status, repeat.Reason = "planned", "maintenance_window"
	case len(signals.Hours) == 0:
		repeat.Status, repeat.Reason = "insufficient", "recent_samples_missing"
	case signals.RecentFailures >= 3:
		repeat.Status, repeat.Reason = "risk", "similar_failures_this_week"
	}
	return append(result, repeat)
}
