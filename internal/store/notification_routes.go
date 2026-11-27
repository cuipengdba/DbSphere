package store

import (
	"context"
	"errors"
	"regexp"

	"github.com/jackc/pgx/v5"
)

var ErrNotificationRoute = errors.New("invalid notification route")

var notificationGroup = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

type NotificationRoute struct {
	Organization string   `json:"organization"`
	Group        string   `json:"group"`
	Initial      []string `json:"initial"`
	Escalation   []string `json:"escalation"`
	AfterMinutes int      `json:"afterMinutes"`
	Reset        bool     `json:"reset"`
}

func migrateNotificationRoutes(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS notification_routes (
		organization_id uuid NOT NULL REFERENCES organizations(id),
		event_group text NOT NULL, initial_channels text[] NOT NULL,
		escalation_channels text[] NOT NULL, escalation_minutes int NOT NULL CHECK(escalation_minutes BETWEEN 1 AND 1440),
		updated_at timestamptz NOT NULL DEFAULT now(), updated_by text NOT NULL,
		PRIMARY KEY (organization_id,event_group)
	);
	INSERT INTO dbsphere_schema_migrations(version) VALUES(76) ON CONFLICT DO NOTHING`)
	return err
}

func (s *Postgres) NotificationRoutes(ctx context.Context, requested string) (any, error) {
	org, err := requestedOrganization(ctx, requested, DefaultOrganizationID)
	if err != nil {
		return nil, ErrNotificationRoute
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT FROM organizations WHERE id=$1)`, org).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotificationRoute
	}
	routes, err := s.queryJSON(ctx, `SELECT jsonb_build_object('group',event_group,'initial',initial_channels,
		'escalation',escalation_channels,'afterMinutes',escalation_minutes,'updatedAt',updated_at,'updatedBy',updated_by)
		FROM notification_routes WHERE organization_id=$1 ORDER BY event_group`, org)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT definition_json->>'group' AS name FROM alert_rules
		WHERE definition_json->>'group' ~ '^[a-z][a-z0-9_]{0,63}$' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	groups, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	channels := []string{"webhook", "email"}
	if s.operationsWebhookOrganization == org {
		channels = append(channels, "operations_webhook")
	}
	return map[string]any{"organization": org, "routes": routes, "groups": groups, "channels": channels, "defaultAfterMinutes": 15}, nil
}

func validNotificationChannels(channels []string, operationsAllowed bool) bool {
	if channels == nil || len(channels) > 3 {
		return false
	}
	seen := make(map[string]bool, len(channels))
	for _, channel := range channels {
		if seen[channel] || (channel != "webhook" && channel != "email" && !(channel == "operations_webhook" && operationsAllowed)) {
			return false
		}
		seen[channel] = true
	}
	return true
}

func (s *Postgres) SaveNotificationRoute(ctx context.Context, input NotificationRoute) error {
	org, err := requestedOrganization(ctx, input.Organization, DefaultOrganizationID)
	if err != nil || !notificationGroup.MatchString(input.Group) {
		return ErrNotificationRoute
	}
	if input.Reset {
		_, err := s.pool.Exec(ctx, `DELETE FROM notification_routes WHERE organization_id=$1 AND event_group=$2`, org, input.Group)
		return err
	}
	if input.AfterMinutes < 1 || input.AfterMinutes > 1440 ||
		!validNotificationChannels(input.Initial, org == s.operationsWebhookOrganization) ||
		!validNotificationChannels(input.Escalation, org == s.operationsWebhookOrganization) {
		return ErrNotificationRoute
	}
	result, err := s.pool.Exec(ctx, `INSERT INTO notification_routes(organization_id,event_group,initial_channels,escalation_channels,escalation_minutes,updated_by)
		SELECT $1,$2,$3,$4,$5,$6 WHERE EXISTS(SELECT FROM organizations WHERE id=$1)
		AND EXISTS(SELECT FROM alert_rules WHERE definition_json->>'group'=$2)
		ON CONFLICT(organization_id,event_group) DO UPDATE SET initial_channels=excluded.initial_channels,
			escalation_channels=excluded.escalation_channels,escalation_minutes=excluded.escalation_minutes,
			updated_by=excluded.updated_by,updated_at=now()`, org, input.Group, input.Initial, input.Escalation,
		input.AfterMinutes, accessActor(ctx, "admin"))
	if err == nil && result.RowsAffected() != 1 {
		return ErrNotificationRoute
	}
	return err
}
