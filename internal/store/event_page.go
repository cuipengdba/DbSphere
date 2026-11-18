package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

var ErrInvalidEventPage = errors.New("invalid event page query")

type EventPageQuery struct {
	Status string
	Search string
	Cursor string
	Limit  int
}

// EventOverview counts all open events and returns their five most recent rows.
// Asset scope is applied in SQL before counting or limiting.
func (s *Postgres) EventOverview(ctx context.Context, assetIDs []string) (any, error) {
	where := "WHERE e.status='open'"
	args := []any{}
	if assetIDs != nil {
		where += " AND e.asset_id=ANY($1::uuid[])"
		args = append(args, assetIDs)
	}
	var count int64
	if err := s.pool.QueryRow(ctx, "SELECT count(*) FROM events e "+where, args...).Scan(&count); err != nil {
		return nil, fmt.Errorf("count open events: %w", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT jsonb_build_object(
		'id',e.id,'assetId',e.asset_id,'assetName',a.name,'kind',e.kind,
		'eventGroup',e.event_group,'title',e.title,'status',e.status,
		'startedAt',e.started_at,'endedAt',e.ended_at,'acknowledgedAt',e.acknowledged_at)
		FROM events e JOIN assets a ON a.id=e.asset_id `+where+
		" ORDER BY e.started_at DESC,e.id DESC LIMIT 5", args...)
	if err != nil {
		return nil, fmt.Errorf("read open events: %w", err)
	}
	defer rows.Close()
	latest := make([]json.RawMessage, 0, 5)
	for rows.Next() {
		var event json.RawMessage
		if err := rows.Scan(&event); err != nil {
			return nil, fmt.Errorf("scan open event: %w", err)
		}
		latest = append(latest, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read open events: %w", err)
	}
	return map[string]any{"openCount": count, "latestOpen": latest}, nil
}

// EventPage searches all retained events with a stable (time, ID) cursor.
// Asset scope is applied in SQL before the page limit.
func (s *Postgres) EventPage(ctx context.Context, query EventPageQuery, assetIDs []string) (any, error) {
	status := query.Status
	if status == "" {
		status = "all"
	}
	if status != "all" && status != "open" && status != "resolved" {
		return nil, ErrInvalidEventPage
	}
	search := strings.TrimSpace(query.Search)
	if utf8.RuneCountInString(search) > 128 || query.Limit < 1 || query.Limit > 100 || len(query.Cursor) > 256 {
		return nil, ErrInvalidEventPage
	}
	var beforeTime time.Time
	var beforeID string
	if query.Cursor != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(query.Cursor)
		if err != nil {
			return nil, ErrInvalidEventPage
		}
		parts := strings.Split(string(decoded), "|")
		if len(parts) != 2 || !evidenceUUID.MatchString(parts[1]) {
			return nil, ErrInvalidEventPage
		}
		beforeTime, err = time.Parse(time.RFC3339Nano, parts[0])
		if err != nil {
			return nil, ErrInvalidEventPage
		}
		beforeID = parts[1]
	}
	args := []any{}
	conditions := []string{}
	parameter := func(value any) string {
		args = append(args, value)
		return fmt.Sprintf("$%d", len(args))
	}
	if assetIDs != nil {
		conditions = append(conditions, "e.asset_id = ANY("+parameter(assetIDs)+"::uuid[])")
	}
	if status != "all" {
		conditions = append(conditions, "e.status = "+parameter(status))
	}
	if search != "" {
		p := parameter(strings.ToLower(search))
		conditions = append(conditions, "(strpos(lower(e.title), "+p+"::text)>0 OR "+
			"strpos(lower(a.name), "+p+"::text)>0 OR "+
			"strpos(e.id::text, "+p+"::text)>0)")
	}
	if !beforeTime.IsZero() {
		t := parameter(beforeTime)
		id := parameter(beforeID)
		conditions = append(conditions, "(e.started_at,e.id) < ("+t+","+id+"::uuid)")
	}
	where := ""
	if len(conditions) > 0 {
		where = " WHERE " + strings.Join(conditions, " AND ")
	}
	limit := parameter(query.Limit + 1)
	rows, err := s.pool.Query(ctx, `SELECT jsonb_build_object(
		'id',e.id,'assetId',e.asset_id,'assetName',a.name,'kind',e.kind,
		'eventGroup',e.event_group,'title',e.title,'status',e.status,
		'startedAt',e.started_at,'endedAt',e.ended_at,'acknowledgedAt',e.acknowledged_at),
		e.started_at,e.id::text
		FROM events e JOIN assets a ON a.id=e.asset_id`+where+
		" ORDER BY e.started_at DESC,e.id DESC LIMIT "+limit, args...)
	if err != nil {
		return nil, fmt.Errorf("read event page: %w", err)
	}
	defer rows.Close()
	type item struct {
		json json.RawMessage
		at   time.Time
		id   string
	}
	items := make([]item, 0, query.Limit+1)
	for rows.Next() {
		var row item
		if err := rows.Scan(&row.json, &row.at, &row.id); err != nil {
			return nil, fmt.Errorf("scan event page: %w", err)
		}
		items = append(items, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read event page: %w", err)
	}
	hasMore := len(items) > query.Limit
	if hasMore {
		items = items[:query.Limit]
	}
	events := make([]json.RawMessage, 0, len(items))
	for _, row := range items {
		events = append(events, row.json)
	}
	nextCursor := ""
	if hasMore {
		last := items[len(items)-1]
		nextCursor = base64.RawURLEncoding.EncodeToString([]byte(last.at.UTC().Format(time.RFC3339Nano) + "|" + last.id))
	}
	return map[string]any{"events": events, "hasMore": hasMore, "nextCursor": nextCursor}, nil
}
