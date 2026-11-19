package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// StartAdminAudit is called before a browser administrator mutation. A failed
// insert prevents the mutation; no request body, cookie or credential is saved.
func (s *Postgres) StartAdminAudit(ctx context.Context, method, path string) (string, error) {
	return s.StartRoleAudit(ctx, "admin", method, path)
}

func (s *Postgres) StartRoleAudit(ctx context.Context, actor, method, path string) (string, error) {
	if actor != "admin" && actor != "approver" {
		return "", errors.New("unsupported audit actor")
	}
	if method != "POST" && method != "PUT" && method != "PATCH" && method != "DELETE" {
		return "", errors.New("unsupported audited method")
	}
	if !strings.HasPrefix(path, "/api/") || len(path) > 512 || strings.ContainsAny(path, "\r\n") {
		return "", errors.New("invalid audited path")
	}
	var eventID *string
	segments := strings.Split(path, "/")
	if len(segments) >= 4 && segments[1] == "api" && segments[2] == "events" && evidenceUUID.MatchString(segments[3]) {
		eventID = &segments[3]
	}
	if len(segments) >= 4 && segments[1] == "api" && segments[2] == "work-orders" && evidenceUUID.MatchString(segments[3]) {
		var linked string
		err := s.pool.QueryRow(ctx, "SELECT event_id::text FROM work_orders WHERE id=$1", segments[3]).Scan(&linked)
		if err == nil {
			eventID = &linked
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
	}
	var id string
	err := s.pool.QueryRow(ctx, `INSERT INTO admin_audit(id,actor,method,path,outcome,event_id)
		VALUES(gen_random_uuid(),$1,$2,$3,'started',$4) RETURNING id::text`, actor, method, path, eventID).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("start admin audit: %w", err)
	}
	return id, nil
}

func (s *Postgres) FinishAdminAudit(ctx context.Context, id string, status int) error {
	if !evidenceUUID.MatchString(id) || status < 100 || status > 599 {
		return errors.New("invalid admin audit completion")
	}
	outcome := "rejected"
	if status >= 200 && status < 400 {
		outcome = "accepted"
	}
	result, err := s.pool.Exec(ctx, `UPDATE admin_audit SET outcome=$2,status_code=$3
		WHERE id=$1 AND outcome='started'`, id, outcome, status)
	if err != nil {
		return fmt.Errorf("finish admin audit: %w", err)
	}
	if result.RowsAffected() != 1 {
		return errors.New("admin audit was already completed or missing")
	}
	return nil
}

func (s *Postgres) RecentAdminAudit(ctx context.Context) (any, error) {
	rows, err := s.pool.Query(ctx, `SELECT jsonb_build_object('id',id,'observedAt',observed_at,
		'actor',actor,'method',method,'path',path,'outcome',outcome,'statusCode',status_code)
		FROM admin_audit ORDER BY observed_at DESC,id DESC LIMIT 100`)
	if err != nil {
		return nil, fmt.Errorf("read admin audit: %w", err)
	}
	defer rows.Close()
	items, err := jsonRows(rows)
	if err != nil {
		return nil, err
	}
	return map[string]any{"entries": items}, nil
}

func (s *Postgres) RecentEventAdminAudit(ctx context.Context, eventID string) (any, error) {
	if !evidenceUUID.MatchString(eventID) {
		return nil, errors.New("invalid event audit ID")
	}
	rows, err := s.pool.Query(ctx, `SELECT jsonb_build_object('id',id,'observedAt',observed_at,
		'actor',actor,'method',method,'path',path,'outcome',outcome,'statusCode',status_code)
		FROM admin_audit WHERE event_id=$1 ORDER BY observed_at DESC,id DESC LIMIT 100`, eventID)
	if err != nil {
		return nil, fmt.Errorf("read event admin audit: %w", err)
	}
	defer rows.Close()
	items, err := jsonRows(rows)
	if err != nil {
		return nil, err
	}
	return map[string]any{"entries": items}, nil
}
