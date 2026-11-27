package store

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrInvalidRestoreDrill = errors.New("invalid restore drill input")
var ErrRestoreDrillConflict = errors.New("restore drill state or evidence does not match")
var drillIdentifier = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,100}$`)

type RestoreDrillRequest struct {
	EventID          string    `json:"eventId"`
	Title            string    `json:"title"`
	TargetAddress    string    `json:"targetAddress"`
	TargetPort       int       `json:"targetPort"`
	TargetID         string    `json:"targetId"`
	TargetAt         time.Time `json:"targetAt"`
	TargetRPOSeconds int       `json:"targetRpoSeconds"`
	TargetRTOSeconds int       `json:"targetRtoSeconds"`
}

type RestoreDrillUpdate struct {
	TargetID string `json:"targetId"`
	Isolated bool   `json:"isolated"`
	SampleID string `json:"sampleId"`
	ProofID  string `json:"proofId"`
	Note     string `json:"note"`
}

func migrateRestoreDrill(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS restore_drills (
		id uuid PRIMARY KEY DEFAULT gen_random_uuid(), asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
		organization_id uuid NOT NULL REFERENCES organizations(id) ON DELETE CASCADE,
		event_id uuid REFERENCES event_keys(id) ON DELETE SET NULL, title text NOT NULL,
		requested_at timestamptz NOT NULL DEFAULT clock_timestamp(), requester text NOT NULL,
		status text NOT NULL DEFAULT 'requested' CHECK(status IN ('requested','approved','rejected','started','verified','failed')),
		source_revision text NOT NULL, target_address inet NOT NULL, target_port integer NOT NULL,
		target_id text NOT NULL, target_at timestamptz NOT NULL, target_rpo integer NOT NULL, target_rto integer NOT NULL,
		reviewer text, reviewed_at timestamptz, executor text, started_at timestamptz,
		finished_at timestamptz, submitted_by text, note text, sample_id uuid, report jsonb
	);
	CREATE INDEX IF NOT EXISTS restore_drills_asset ON restore_drills(asset_id, requested_at DESC, id);
	CREATE UNIQUE INDEX IF NOT EXISTS restore_drill_active_target ON restore_drills(organization_id, target_id) WHERE status='started';
	CREATE UNIQUE INDEX IF NOT EXISTS restore_drill_active_endpoint ON restore_drills(organization_id, target_address, target_port) WHERE status='started';
	INSERT INTO dbsphere_schema_migrations(version) VALUES(74) ON CONFLICT DO NOTHING`)
	return err
}

func validDrillTarget(address string, port int) bool {
	ip, err := netip.ParseAddr(address)
	return err == nil && ip.Zone() == "" && ip.IsLoopback() && port >= 1024 && port <= 65535
}

const restoreDrillJSON = `jsonb_build_object('id',d.id,'assetId',d.asset_id,'eventId',d.event_id,
	'title',d.title,'requestedAt',d.requested_at,'requester',d.requester,'status',d.status,
	'targetAddress',host(d.target_address),'targetPort',d.target_port,'targetId',d.target_id,'targetAt',d.target_at,
	'targetRpoSeconds',d.target_rpo,'targetRtoSeconds',d.target_rto,'reviewer',d.reviewer,'reviewedAt',d.reviewed_at,
	'executor',d.executor,'startedAt',d.started_at,'finishedAt',d.finished_at,'submittedBy',d.submitted_by,
	'note',d.note,'sampleId',d.sample_id,'report',d.report)`

func (s *Postgres) RestoreDrills(ctx context.Context, assetID string) (any, error) {
	var org string
	err := s.pool.QueryRow(ctx, `SELECT organization_id::text FROM assets WHERE id=$1 AND ($2::uuid IS NULL OR organization_id=$2)`,
		assetID, nullable(OrganizationFromContext(ctx))).Scan(&org)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAssetNotFound
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.queryJSON(ctx, `SELECT `+restoreDrillJSON+` FROM restore_drills d
		WHERE d.asset_id=$1 AND d.organization_id=$2 ORDER BY d.requested_at DESC,d.id LIMIT 50`, assetID, org)
	return map[string]any{"drills": rows}, err
}

func (s *Postgres) RequestRestoreDrill(ctx context.Context, assetID string, input RestoreDrillRequest) (any, error) {
	input.Title = strings.TrimSpace(input.Title)
	if !evidenceUUID.MatchString(input.EventID) || !validChangeText(input.Title, 120) || !drillIdentifier.MatchString(input.TargetID) ||
		!validDrillTarget(input.TargetAddress, input.TargetPort) || input.TargetAt.IsZero() || input.TargetAt.After(time.Now()) ||
		input.TargetRPOSeconds < 0 || input.TargetRTOSeconds < 1 || input.TargetRPOSeconds > 604800 || input.TargetRTOSeconds > 604800 {
		return nil, ErrInvalidRestoreDrill
	}
	var id string
	err := s.pool.QueryRow(ctx, `INSERT INTO restore_drills(asset_id,organization_id,event_id,title,requester,source_revision,
		target_address,target_port,target_id,target_at,target_rpo,target_rto)
		SELECT a.id,a.organization_id,e.id,$4,$5,`+changeTargetRevision+`,$6::text::inet,$7,$8,$9,$10,$11
		FROM assets a JOIN event_keys e ON e.id=$3 AND e.asset_id=a.id WHERE a.id=$1
		AND ($2::uuid IS NULL OR a.organization_id=$2) AND a.runtime IN ('linux','k8s','managed') AND a.port<>$7
		RETURNING id::text`, assetID, nullable(OrganizationFromContext(ctx)), input.EventID, input.Title, accessActor(ctx, "admin"),
		input.TargetAddress, input.TargetPort, input.TargetID, input.TargetAt.UTC(), input.TargetRPOSeconds, input.TargetRTOSeconds).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRestoreDrillConflict
	}
	return map[string]string{"id": id}, err
}

func (s *Postgres) UpdateRestoreDrill(ctx context.Context, assetID, id, action string, input RestoreDrillUpdate) (any, error) {
	actor := accessActor(ctx, "admin")
	args := []any{id, assetID, nullable(OrganizationFromContext(ctx)), actor}
	var changedID string
	var err error
	switch action {
	case "approve", "reject":
		status := "approved"
		if action == "reject" {
			status = "rejected"
		}
		err = s.pool.QueryRow(ctx, `UPDATE restore_drills d SET status=$5,reviewer=$4,reviewed_at=clock_timestamp()
			FROM assets a WHERE d.id=$1 AND d.asset_id=$2 AND ($3::uuid IS NULL OR d.organization_id=$3)
			AND a.id=d.asset_id AND a.organization_id=d.organization_id AND d.requester<>$4 AND d.status='requested'
			AND ($5='rejected' OR (d.event_id IS NOT NULL AND d.source_revision=`+changeTargetRevision+`)) RETURNING d.id::text`,
			append(args, status)...).Scan(&changedID)
	case "start":
		if !input.Isolated || !drillIdentifier.MatchString(input.TargetID) {
			return nil, ErrInvalidRestoreDrill
		}
		err = s.pool.QueryRow(ctx, `UPDATE restore_drills d SET status='started',executor=$4,started_at=clock_timestamp()
			FROM assets a WHERE d.id=$1 AND d.asset_id=$2 AND ($3::uuid IS NULL OR d.organization_id=$3)
			AND a.id=d.asset_id AND a.organization_id=d.organization_id AND d.event_id IS NOT NULL
			AND d.status='approved' AND d.target_id=$5 AND d.source_revision=`+changeTargetRevision+`
			RETURNING d.id::text`, append(args, input.TargetID)...).Scan(&changedID)
	case "complete", "fail":
		return s.finishRestoreDrill(ctx, assetID, id, action, input, actor)
	default:
		return nil, ErrInvalidRestoreDrill
	}
	var constraint *pgconn.PgError
	if errors.Is(err, pgx.ErrNoRows) || (errors.As(err, &constraint) && constraint.Code == "23505") {
		return nil, ErrRestoreDrillConflict
	}
	return map[string]string{"id": changedID}, err
}

type sampledRestoreProof struct {
	ID           string    `json:"id"`
	Verification string    `json:"verification"`
	TargetID     string    `json:"targetId"`
	TargetAt     time.Time `json:"targetAt"`
	StartedAt    time.Time `json:"startedAt"`
	FinishedAt   time.Time `json:"finishedAt"`
	RPO          *float64  `json:"rpoSeconds"`
	RTO          *float64  `json:"rtoSeconds"`
	Chain        []string  `json:"chain"`
}

func matchDrillProof(raw []byte, id, target string, targetAt, startedAt, collectedAt time.Time) (sampledRestoreProof, json.RawMessage, bool) {
	var catalog struct {
		Restores []json.RawMessage `json:"restoreTests"`
	}
	if json.Unmarshal(raw, &catalog) != nil {
		return sampledRestoreProof{}, nil, false
	}
	var proof sampledRestoreProof
	var evidence json.RawMessage
	for _, item := range catalog.Restores {
		var row sampledRestoreProof
		if json.Unmarshal(item, &row) != nil || row.ID != id {
			continue
		}
		if evidence != nil || row.Verification != "verified" || row.TargetID != target ||
			!row.TargetAt.Equal(targetAt) || row.StartedAt.Before(startedAt) || row.FinishedAt.Before(row.StartedAt) ||
			row.FinishedAt.After(collectedAt) || row.RPO == nil || row.RTO == nil || len(row.Chain) == 0 ||
			*row.RPO < 0 || *row.RTO < 0 || math.Abs(*row.RTO-row.FinishedAt.Sub(row.StartedAt).Seconds()) > 0.001 {
			return sampledRestoreProof{}, nil, false
		}
		proof, evidence = row, item
	}
	return proof, evidence, evidence != nil
}

func (s *Postgres) finishRestoreDrill(ctx context.Context, assetID, id, action string, input RestoreDrillUpdate, actor string) (any, error) {
	input.Note = strings.TrimSpace(input.Note)
	if !validChangeText(input.Note, 500) || (action == "complete" && (!evidenceUUID.MatchString(input.SampleID) || !drillIdentifier.MatchString(input.ProofID))) {
		return nil, ErrInvalidRestoreDrill
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var eventID *string
	var targetID string
	var targetAt, startedAt time.Time
	var targetRPO, targetRTO int
	err = tx.QueryRow(ctx, `SELECT d.event_id::text,d.target_id,d.target_at,d.started_at,d.target_rpo,d.target_rto
		FROM restore_drills d JOIN assets a ON a.id=d.asset_id AND a.organization_id=d.organization_id
		WHERE d.id=$1 AND d.asset_id=$2 AND ($3::uuid IS NULL OR d.organization_id=$3) AND d.status='started'
		AND ($4='fail' OR (d.event_id IS NOT NULL AND d.source_revision=`+changeTargetRevision+`)) FOR UPDATE OF d,a`,
		id, assetID, nullable(OrganizationFromContext(ctx)), action).Scan(&eventID, &targetID, &targetAt, &startedAt, &targetRPO, &targetRTO)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRestoreDrillConflict
	}
	if err != nil {
		return nil, err
	}
	status := "failed"
	var report []byte
	var sample any
	if action == "complete" {
		var raw []byte
		var at time.Time
		err = tx.QueryRow(ctx, `SELECT collected_at,metrics_json->'backup' FROM samples
			WHERE asset_id=$1 AND id=$2 AND status='ok' AND collected_at>=$3 ORDER BY collected_at DESC LIMIT 1`,
			assetID, input.SampleID, startedAt).Scan(&at, &raw)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrRestoreDrillConflict
		}
		if err != nil {
			return nil, err
		}
		proof, evidence, ok := matchDrillProof(raw, input.ProofID, targetID, targetAt, startedAt, at)
		if !ok {
			return nil, ErrRestoreDrillConflict
		}
		report, err = json.Marshal(map[string]any{
			"source": "pgBackRest", "observedAt": at, "proof": evidence,
			"rpoDifferenceSeconds": *proof.RPO - float64(targetRPO), "rtoDifferenceSeconds": *proof.RTO - float64(targetRTO),
			"targetsMet": *proof.RPO <= float64(targetRPO) && *proof.RTO <= float64(targetRTO),
		})
		if err != nil {
			return nil, err
		}
		status, sample = "verified", input.SampleID
	}
	var finishedAt time.Time
	err = tx.QueryRow(ctx, `UPDATE restore_drills SET status=$2,submitted_by=$3,note=$4,sample_id=$5,report=$6,finished_at=clock_timestamp()
		WHERE id=$1 RETURNING finished_at`, id, status, actor, input.Note, sample, report).Scan(&finishedAt)
	if err != nil {
		return nil, err
	}
	if eventID != nil {
		payload := map[string]any{"drillId": id, "status": status, "actor": actor, "note": input.Note}
		if sample != nil {
			payload["sampleId"] = sample
		}
		if err := insertChangeEvidence(ctx, tx, *eventID, "restore_drill", finishedAt, payload); err != nil {
			return nil, err
		}
	}
	return map[string]string{"id": id, "status": status}, tx.Commit(ctx)
}
