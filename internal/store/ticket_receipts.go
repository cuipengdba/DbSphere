package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
)

type TicketReceipt struct {
	Source        string `json:"source"`
	ExternalID    string `json:"externalId"`
	ReceiptID     string `json:"receiptId"`
	DeliveryID    string `json:"deliveryId"`
	AssetID       string `json:"assetId"`
	EventID       string `json:"eventId"`
	State         string `json:"state"`
	ExternalOwner string `json:"externalOwner,omitempty"`
	LocalOwner    string `json:"localOwner,omitempty"`
}

func migrateTicketReceipts(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=68)`).Scan(&applied); err != nil || applied {
		return err
	}
	_, err := tx.Exec(ctx, `CREATE TABLE operations_ticket_receipts (
		organization_id uuid NOT NULL REFERENCES organizations(id), source text NOT NULL, external_id text NOT NULL, receipt_id text NOT NULL,
		evidence_id uuid NOT NULL REFERENCES evidence(id) ON DELETE CASCADE,
		PRIMARY KEY(organization_id,source,external_id,receipt_id));
	INSERT INTO dbsphere_schema_migrations(version) VALUES(68)`)
	return err
}

func (s *Postgres) RecordTicketReceipt(ctx context.Context, organization string, input TicketReceipt) (ExternalIncidentResult, error) {
	result := ExternalIncidentResult{EventID: input.EventID, State: input.State}
	if !externalSource.MatchString(input.Source) || !externalKey.MatchString(input.ExternalID) || !externalKey.MatchString(input.ReceiptID) ||
		!evidenceUUID.MatchString(input.DeliveryID) || !evidenceUUID.MatchString(input.AssetID) || !evidenceUUID.MatchString(input.EventID) ||
		len([]rune(input.ExternalOwner)) > 120 || strings.ContainsFunc(input.ExternalOwner, unicode.IsControl) ||
		input.LocalOwner != "" && !accessName.MatchString(input.LocalOwner) {
		return result, ErrExternalInput
	}
	switch input.State {
	case "acknowledged", "in_progress", "resolved", "reopened":
	default:
		return result, ErrExternalInput
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	var linkedEvent, linkedAsset string
	err = tx.QueryRow(ctx, `SELECT event_id::text,asset_id::text FROM external_incident_links
		WHERE organization_id=$1 AND source=$2 AND external_id=$3 AND kind='ticket' FOR UPDATE`, organization, input.Source, input.ExternalID).
		Scan(&linkedEvent, &linkedAsset)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, ErrExternalMissing
	}
	if err != nil {
		return result, err
	}
	if linkedEvent != input.EventID || linkedAsset != input.AssetID {
		return result, ErrExternalConflict
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return result, err
	}
	var identical bool
	err = tx.QueryRow(ctx, `SELECT x.id::text,x.payload_json=$5::jsonb FROM operations_ticket_receipts r JOIN evidence x ON x.id=r.evidence_id
		WHERE r.organization_id=$1 AND r.source=$2 AND r.external_id=$3 AND r.receipt_id=$4`, organization, input.Source,
		input.ExternalID, input.ReceiptID, payload).Scan(&result.EvidenceID, &identical)
	if err == nil {
		if !identical {
			return result, ErrExternalConflict
		}
		result.Duplicate = true
		return result, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	var valid bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM notification_outbox n JOIN evidence x ON x.id=n.evidence_id
		WHERE n.id=$1 AND n.channel='operations_webhook' AND x.event_id=$2)`, input.DeliveryID, input.EventID).Scan(&valid); err != nil {
		return result, err
	}
	if !valid {
		return result, ErrExternalMissing
	}
	if input.LocalOwner != "" {
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM access_accounts a JOIN access_memberships m ON m.account_id=a.id
			JOIN access_team_assets t ON t.team_id=m.team_id WHERE a.username=$1 AND a.enabled AND a.role='operator'
			AND t.asset_id=$2 AND coalesce(a.organization_id,$3::uuid)=$4::uuid)`, input.LocalOwner, input.AssetID, DefaultOrganizationID, organization).Scan(&valid); err != nil {
			return result, err
		}
		if !valid {
			return result, ErrExternalMissing
		}
	}
	if err := tx.QueryRow(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
		VALUES(gen_random_uuid(),$1,'external_ticket_receipt',now(),$2) RETURNING id::text`, input.EventID, payload).Scan(&result.EvidenceID); err != nil {
		return result, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO operations_ticket_receipts(organization_id,source,external_id,receipt_id,evidence_id)
		VALUES($1,$2,$3,$4,$5)`, organization, input.Source, input.ExternalID, input.ReceiptID, result.EvidenceID); err != nil {
		return result, err
	}
	return result, tx.Commit(ctx)
}
