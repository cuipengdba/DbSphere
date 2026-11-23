package store

import (
	"context"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/cuipengdba/dbsphere/internal/trace"
	"github.com/jackc/pgx/v5"
)

func migrateConnectionStages(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=51)").Scan(&applied); err != nil || applied {
		return err
	}
	_, err := tx.Exec(ctx, `ALTER TABLE trace_spans ADD COLUMN IF NOT EXISTS endpoint_name text;
		INSERT INTO dbsphere_schema_migrations(version) VALUES(51)`)
	return err
}

type connectionStageEvidence struct {
	Stage          string    `json:"stage"`
	Source         string    `json:"source"`
	Outcome        string    `json:"outcome"`
	At             time.Time `json:"at"`
	DurationMs     *int64    `json:"durationMs,omitempty"`
	Service        string    `json:"service,omitempty"`
	NearbyServices []string  `json:"nearbyServices,omitempty"`
	Endpoint       string    `json:"endpoint,omitempty"`
	Route          string    `json:"route,omitempty"`
	Clock          string    `json:"clock,omitempty"`
	TraceID        string    `json:"traceId,omitempty"`
	SpanID         string    `json:"spanId,omitempty"`
	SampleID       string    `json:"sampleId,omitempty"`
	WaitingClients float64   `json:"waitingClients,omitempty"`
	BackendPID     int       `json:"backendPid,omitempty"`
	BlockerPID     int       `json:"blockerPid,omitempty"`
}

type connectionStageReport struct {
	Rows         []connectionStageEvidence `json:"rows"`
	TraceAtLimit bool                      `json:"traceAtLimit"`
	HasAppSpans  bool                      `json:"hasAppSpans"`
}

func connectionStages(spans []requestSpanObservation, samples map[string]sampleObservation, atLimit bool) connectionStageReport {
	report := connectionStageReport{Rows: []connectionStageEvidence{}, TraceAtLimit: atLimit}
	locks := map[string]bool{}
	for _, span := range spans {
		stage, timedOut := trace.ConnectionStage(span.Kind)
		if stage == "" && span.Kind != "postgresql_client" {
			continue
		}
		report.HasAppSpans = true
		duration := span.End.Sub(span.Start).Milliseconds()
		row := connectionStageEvidence{
			Stage: stage, Source: "application", Outcome: span.Status, At: span.Start, DurationMs: &duration,
			Service: span.Service, Endpoint: span.Endpoint, Route: span.Route,
			TraceID: span.TraceID, SpanID: span.SpanID, Clock: "observed",
		}
		if timedOut {
			row.Outcome = "timeout"
		}
		if span.TimingAnomaly {
			row.Clock = "unreliable"
		} else if span.ReceivedAt == nil {
			row.Clock = "unavailable"
		}
		if span.Kind == "postgresql_client" {
			row.Stage, row.BackendPID = "execute", span.BackendPID
			var best sampleObservation
			if row.Clock == "observed" {
				for _, sample := range samples {
					if sample.At.Before(span.Start) || sample.At.After(span.End) ||
						(best.ID != "" && (sample.At.Before(best.At) || (sample.At.Equal(best.At) && sample.ID >= best.ID))) {
						continue
					}
					if blocker := matchedLockWaitBlocker(sample, span.Start, span.End, span.BackendPID); blocker > 0 {
						best, row.BlockerPID = sample, blocker
					}
				}
			}
			if best.ID != "" {
				row.Stage, row.Source, row.SampleID = "database_wait", "pid_match", best.ID
				locks[best.ID] = true
			}
		}
		report.Rows = append(report.Rows, row)
	}
	for _, sample := range samples {
		pooler := readGoSignal("pgbouncer.waiting_clients", sample.Metrics, sample.At)
		if pooler.Available && pooler.Value > 0 {
			at := sample.At
			if value, err := time.Parse(time.RFC3339Nano, stringValue(object(sample.Metrics["pgbouncer"])["observedAt"])); err == nil {
				at = value
			}
			row := connectionStageEvidence{Stage: "pgbouncer_queue", Source: "pool_sample", Outcome: "waiting",
				At: at, SampleID: sample.ID, WaitingClients: pooler.Value, Route: "pgbouncer"}
			for _, span := range spans {
				stage, _ := trace.ConnectionStage(span.Kind)
				if span.Route == "pgbouncer" && (stage == "connect" || span.Kind == "postgresql_client") &&
					span.ReceivedAt != nil && !span.TimingAnomaly && !at.Before(span.Start) && !at.After(span.End) &&
					!slices.Contains(row.NearbyServices, span.Service) {
					row.NearbyServices = append(row.NearbyServices, span.Service)
				}
			}
			sort.Strings(row.NearbyServices)
			report.Rows = append(report.Rows, row)
		}
		chain := object(object(sample.Metrics["diagnostics"])["lockChain"])
		if !locks[sample.ID] && sample.Status == "ok" && stringValue(chain["status"]) == "ok" && len(list(chain["data"])) > 0 {
			report.Rows = append(report.Rows, connectionStageEvidence{Stage: "database_wait", Source: "database_sample",
				Outcome: "waiting", At: sample.At, SampleID: sample.ID})
		}
	}
	slices.SortFunc(report.Rows, func(a, b connectionStageEvidence) int {
		if order := b.At.Compare(a.At); order != 0 {
			return order
		}
		return strings.Compare(a.Stage+a.TraceID+a.SpanID+a.SampleID, b.Stage+b.TraceID+b.SpanID+b.SampleID)
	})
	return report
}
