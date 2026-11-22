package store

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
)

var (
	ErrExternalInput    = errors.New("invalid external incident input")
	ErrExternalMissing  = errors.New("asset, event or external incident not found")
	ErrExternalConflict = errors.New("external incident is linked to another event or asset")
	externalKey         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,179}$`)
	externalSource      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,47}$`)
)

type ExternalIncidentInput struct {
	Source     string `json:"source"`
	ExternalID string `json:"externalId"`
	Kind       string `json:"kind"`
	State      string `json:"state"`
	AssetID    string `json:"assetId"`
	EventID    string `json:"eventId,omitempty"`
	Title      string `json:"title,omitempty"`
	URL        string `json:"url,omitempty"`
}

type ExternalIncidentResult struct {
	EventID    string `json:"eventId"`
	EvidenceID string `json:"evidenceId"`
	State      string `json:"state"`
	Duplicate  bool   `json:"duplicate"`
}

func migrateOperations(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT FROM dbsphere_schema_migrations WHERE version=48)").Scan(&applied); err != nil || applied {
		return err
	}
	_, err := tx.Exec(ctx, `
		CREATE TABLE external_incident_links (
			organization_id uuid NOT NULL REFERENCES organizations(id),
			source text NOT NULL, external_id text NOT NULL, kind text NOT NULL,
			event_id uuid NOT NULL REFERENCES event_keys(id) ON DELETE CASCADE,
			evidence_id uuid NOT NULL REFERENCES evidence(id) ON DELETE CASCADE,
			asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
			state text NOT NULL, owned_event boolean NOT NULL,
			PRIMARY KEY(organization_id,source,external_id,kind)
		);
		CREATE INDEX external_incident_links_event ON external_incident_links(event_id);
		INSERT INTO dbsphere_schema_migrations(version) VALUES(48)`)
	return err
}

func (s *Postgres) OperationsAsset(ctx context.Context, organization, id string) (json.RawMessage, error) {
	var asset json.RawMessage
	err := s.pool.QueryRow(ctx, "SELECT "+assetJSON+" FROM assets WHERE id=$1 AND organization_id=$2", id, organization).Scan(&asset)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return asset, err
}

func (s *Postgres) OperationsEvent(ctx context.Context, organization, id string) (json.RawMessage, error) {
	var event json.RawMessage
	err := s.pool.QueryRow(ctx, `SELECT jsonb_build_object(
		'id',e.id,'assetId',e.asset_id,'assetName',a.name,'kind',e.kind,'title',e.title,'status',e.status,
		'startedAt',e.started_at,'endedAt',e.ended_at,'acknowledgedAt',e.acknowledged_at,
		'consolePath','/#incidents?event='||e.id::text,'external',coalesce((
			SELECT jsonb_agg(jsonb_build_object('id',x.id,'kind',x.kind,'observedAt',x.observed_at,'payload',x.payload_json)
			                ORDER BY x.observed_at DESC,x.id)
			FROM (SELECT id,kind,observed_at,payload_json FROM evidence WHERE event_id=e.id
			      AND kind IN ('external_alert_firing','external_alert_resolved','external_ticket_linked')
			      ORDER BY observed_at DESC,id LIMIT 100) x
		),'[]'::jsonb))
		FROM event_keys k JOIN events e ON e.id=k.id AND e.started_at=k.started_at
		JOIN assets a ON a.id=k.asset_id WHERE k.id=$1 AND a.organization_id=$2`, id, organization).Scan(&event)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return event, err
}

func validateExternalIncident(input ExternalIncidentInput) bool {
	if !externalSource.MatchString(input.Source) || !externalKey.MatchString(input.ExternalID) ||
		!evidenceUUID.MatchString(input.AssetID) || (input.EventID != "" && !evidenceUUID.MatchString(input.EventID)) ||
		len([]rune(input.Title)) > 180 || strings.ContainsFunc(input.Title, unicode.IsControl) || len(input.URL) > 2048 {
		return false
	}
	if input.URL != "" {
		u, err := url.Parse(input.URL)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
			return false
		}
	}
	switch input.Kind {
	case "alert":
		return (input.State == "firing" && strings.TrimSpace(input.Title) != "") || input.State == "resolved"
	case "ticket":
		return input.State == "linked" && input.EventID != ""
	default:
		return false
	}
}

func (s *Postgres) RecordExternalIncident(ctx context.Context, organization string, input ExternalIncidentInput) (ExternalIncidentResult, error) {
	var result ExternalIncidentResult
	if !validateExternalIncident(input) {
		return result, ErrExternalInput
	}
	now := time.Now().UTC()
	if input.Kind == "alert" && input.State == "firing" && input.EventID == "" {
		if err := s.ensureCollectionPartitions(ctx, now); err != nil {
			return result, err
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	var assetID string
	err = tx.QueryRow(ctx, "SELECT id::text FROM assets WHERE id=$1 AND organization_id=$2 FOR KEY SHARE",
		input.AssetID, organization).Scan(&assetID)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, ErrExternalMissing
	}
	if err != nil {
		return result, err
	}

	identity := strings.Join([]string{organization, input.Source, input.ExternalID, input.Kind}, "|")
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", identity); err != nil {
		return result, err
	}
	var linkedAsset string
	var ownedEvent bool
	err = tx.QueryRow(ctx, `SELECT event_id::text,evidence_id::text,asset_id::text,state,owned_event
		FROM external_incident_links WHERE organization_id=$1 AND source=$2 AND external_id=$3 AND kind=$4`,
		organization, input.Source, input.ExternalID, input.Kind).Scan(&result.EventID, &result.EvidenceID, &linkedAsset, &result.State, &ownedEvent)
	switch {
	case err == nil:
		if linkedAsset != assetID || (input.EventID != "" && result.EventID != input.EventID) {
			return ExternalIncidentResult{}, ErrExternalConflict
		}
		if result.State == input.State || result.State == "resolved" {
			result.Duplicate = true
			return result, nil
		}
	case !errors.Is(err, pgx.ErrNoRows):
		return result, err
	case input.State == "resolved":
		return result, ErrExternalMissing
	case input.EventID != "":
		err := tx.QueryRow(ctx, "SELECT id::text FROM event_keys WHERE id=$1 AND asset_id=$2 FOR KEY SHARE",
			input.EventID, assetID).Scan(&result.EventID)
		if errors.Is(err, pgx.ErrNoRows) {
			return result, ErrExternalMissing
		}
		if err != nil {
			return result, err
		}
	default:
		ownedEvent = true
		if err := tx.QueryRow(ctx, `INSERT INTO event_keys(id,asset_id,started_at)
			VALUES(gen_random_uuid(),$1,$2) RETURNING id::text`, assetID, now).Scan(&result.EventID); err != nil {
			return result, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
			VALUES($1,$2,'external_alert',$3,'open',$4,'external')`, result.EventID, assetID, strings.TrimSpace(input.Title), now); err != nil {
			return result, err
		}
	}
	if input.State == "resolved" && ownedEvent {
		if _, err := tx.Exec(ctx, `UPDATE events e SET status='resolved',ended_at=$2 FROM event_keys k
			WHERE k.id=$1 AND e.id=k.id AND e.started_at=k.started_at AND e.status='open'`, result.EventID, now); err != nil {
			return result, err
		}
	}
	kind := "external_" + input.Kind + "_" + input.State
	payload, err := json.Marshal(input)
	if err != nil {
		return result, err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
		VALUES(gen_random_uuid(),$1,$2,$3,$4) RETURNING id::text`, result.EventID, kind, now, payload).Scan(&result.EvidenceID); err != nil {
		return result, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO external_incident_links(organization_id,source,external_id,kind,event_id,evidence_id,
		asset_id,state,owned_event) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT(organization_id,source,external_id,kind) DO UPDATE SET evidence_id=excluded.evidence_id,state=excluded.state`,
		organization, input.Source, input.ExternalID, input.Kind, result.EventID, result.EvidenceID, assetID, input.State, ownedEvent); err != nil {
		return result, err
	}
	result.State = input.State
	return result, tx.Commit(ctx)
}

type OperationsQueue struct {
	Pending int64
	Failed  int64
}

func (s *Postgres) OperationsQueue(ctx context.Context) (OperationsQueue, error) {
	var status OperationsQueue
	err := s.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status='pending'),count(*) FILTER (WHERE status='failed')
		FROM notification_outbox WHERE status IN ('pending','failed')`).Scan(&status.Pending, &status.Failed)
	return status, err
}
