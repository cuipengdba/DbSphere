package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrInvalidSilence = errors.New("invalid silence window")
var ErrSilenceReference = errors.New("silence asset or rule not found")
var ErrSilenceNotFound = errors.New("silence not found or already revoked")

var silenceUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var silenceRuleID = regexp.MustCompile(`^[a-z][a-z0-9_]{1,63}$`)

type SilenceInput struct {
	Kind     string `json:"kind"`
	AssetID  string `json:"assetId"`
	RuleID   string `json:"ruleId"`
	StartsAt string `json:"startsAt"`
	EndsAt   string `json:"endsAt"`
	Reason   string `json:"reason"`
}

func (input SilenceInput) normalize(now time.Time) (SilenceInput, error) {
	if input.Kind == "" {
		input.Kind = "silence"
	}
	input.Reason = strings.TrimSpace(input.Reason)
	if (input.Kind != "silence" && input.Kind != "maintenance") ||
		input.Reason == "" || len([]rune(input.Reason)) > 300 ||
		(input.AssetID != "" && !silenceUUID.MatchString(input.AssetID)) ||
		(input.RuleID != "" && !silenceRuleID.MatchString(input.RuleID)) {
		return input, ErrInvalidSilence
	}
	if input.StartsAt == "" {
		input.StartsAt = now.UTC().Format(time.RFC3339Nano)
	}
	if !strings.HasSuffix(input.StartsAt, "Z") || !strings.HasSuffix(input.EndsAt, "Z") {
		return input, ErrInvalidSilence
	}
	start, startErr := time.Parse(time.RFC3339Nano, input.StartsAt)
	end, endErr := time.Parse(time.RFC3339Nano, input.EndsAt)
	if startErr != nil || endErr != nil || !end.After(start) ||
		end.Sub(start) > 30*24*time.Hour || start.Before(now.Add(-5*time.Minute)) ||
		start.After(now.Add(30*24*time.Hour)) {
		return input, ErrInvalidSilence
	}
	input.StartsAt = start.UTC().Format(time.RFC3339Nano)
	input.EndsAt = end.UTC().Format(time.RFC3339Nano)
	return input, nil
}

func optionalSilenceID(value string) any {
	if value == "" {
		return nil
	}
	return value
}

const silenceJSON = `jsonb_build_object('id',id,'kind',kind,'assetId',asset_id,
	'ruleId',rule_id,'startsAt',starts_at,'endsAt',ends_at,'reason',reason,
	'createdAt',created_at,'revokedAt',revoked_at)`

func (s *Postgres) CreateSilence(ctx context.Context, input SilenceInput) (json.RawMessage, error) {
	input, err := input.normalize(time.Now().UTC())
	if err != nil {
		return nil, err
	}
	var assetValid, ruleValid bool
	if err = s.pool.QueryRow(ctx, `SELECT
		($1::uuid IS NULL OR EXISTS(SELECT 1 FROM assets WHERE id=$1)),
		($2::text IS NULL OR EXISTS(SELECT 1 FROM alert_rules WHERE id=$2))`,
		optionalSilenceID(input.AssetID), optionalSilenceID(input.RuleID)).Scan(&assetValid, &ruleValid); err != nil {
		return nil, fmt.Errorf("validate silence references: %w", err)
	}
	if !assetValid || !ruleValid {
		return nil, ErrSilenceReference
	}
	var raw []byte
	err = s.pool.QueryRow(ctx, `WITH added AS (
		INSERT INTO silences(id,kind,asset_id,rule_id,starts_at,ends_at,reason,created_at)
		VALUES(gen_random_uuid(),$1,$2,$3,$4,$5,$6,now()) RETURNING *)
		SELECT `+silenceJSON+` FROM added`, input.Kind, optionalSilenceID(input.AssetID),
		optionalSilenceID(input.RuleID), input.StartsAt, input.EndsAt, input.Reason).Scan(&raw)
	if err != nil {
		return nil, fmt.Errorf("create silence: %w", err)
	}
	return json.RawMessage(raw), nil
}

func (s *Postgres) RevokeSilence(ctx context.Context, id string) (json.RawMessage, error) {
	if !silenceUUID.MatchString(id) {
		return nil, ErrInvalidSilence
	}
	var raw []byte
	err := s.pool.QueryRow(ctx, `WITH changed AS (
		UPDATE silences SET revoked_at=now() WHERE id=$1 AND revoked_at IS NULL RETURNING *)
		SELECT `+silenceJSON+` FROM changed`, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrSilenceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("revoke silence: %w", err)
	}
	return json.RawMessage(raw), nil
}
