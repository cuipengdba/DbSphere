package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrInvalidSQLPlan = errors.New("invalid SQL plan")

func planNumber(value any) any {
	n, ok := value.(float64)
	if !ok || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		return nil
	}
	return n
}

func sanitizeSQLPlan(queryID, observed string, raw json.RawMessage) (time.Time, string, []byte, error) {
	if len(raw) == 0 || len(raw) > 50000 || !slowQueryID.MatchString(queryID) || queryID == "0" {
		return time.Time{}, "", nil, ErrInvalidSQLPlan
	}
	if observed == "" {
		observed = time.Now().UTC().Format(time.RFC3339Nano)
	}
	at, err := time.Parse(time.RFC3339Nano, observed)
	if err != nil {
		return time.Time{}, "", nil, ErrInvalidSQLPlan
	}
	var input any
	if json.Unmarshal(raw, &input) != nil {
		return time.Time{}, "", nil, ErrInvalidSQLPlan
	}
	if list, ok := input.([]any); ok {
		if len(list) == 0 {
			return time.Time{}, "", nil, ErrInvalidSQLPlan
		}
		input = list[0]
	}
	root, ok := input.(map[string]any)
	if !ok {
		return time.Time{}, "", nil, ErrInvalidSQLPlan
	}
	plan, ok := root["Plan"].(map[string]any)
	if !ok {
		return time.Time{}, "", nil, ErrInvalidSQLPlan
	}
	nodes := make([]map[string]any, 0)
	shape := make([]string, 0)
	var walk func(map[string]any, int) error
	walk = func(node map[string]any, depth int) error {
		if depth > 12 || len(nodes) >= 100 {
			return ErrInvalidSQLPlan
		}
		typeName, ok := node["Node Type"].(string)
		if !ok || len(typeName) < 2 || len(typeName) > 80 {
			return ErrInvalidSQLPlan
		}
		for _, char := range typeName {
			if char != ' ' && (char < 'A' || char > 'Z') && (char < 'a' || char > 'z') {
				return ErrInvalidSQLPlan
			}
		}
		nodes = append(nodes, map[string]any{
			"depth": depth, "type": typeName,
			"totalCost": planNumber(node["Total Cost"]), "planRows": planNumber(node["Plan Rows"]),
			"actualTotalMs": planNumber(node["Actual Total Time"]), "actualRows": planNumber(node["Actual Rows"]),
			"actualLoops": planNumber(node["Actual Loops"]), "sharedHitBlocks": planNumber(node["Shared Hit Blocks"]),
			"sharedReadBlocks": planNumber(node["Shared Read Blocks"]), "tempWrittenBlocks": planNumber(node["Temp Written Blocks"]),
		})
		shape = append(shape, strconv.Itoa(depth)+":"+typeName)
		if children, found := node["Plans"]; found {
			list, ok := children.([]any)
			if !ok {
				return ErrInvalidSQLPlan
			}
			for _, child := range list {
				mapped, ok := child.(map[string]any)
				if !ok || walk(mapped, depth+1) != nil {
					return ErrInvalidSQLPlan
				}
			}
		}
		return nil
	}
	if walk(plan, 0) != nil {
		return time.Time{}, "", nil, ErrInvalidSQLPlan
	}
	hash := sha256.Sum256([]byte(strings.Join(shape, "|")))
	summary, err := json.Marshal(map[string]any{
		"nodeCount": len(nodes), "planningMs": planNumber(root["Planning Time"]),
		"executionMs": planNumber(root["Execution Time"]), "nodes": nodes,
	})
	if err != nil {
		return time.Time{}, "", nil, err
	}
	return at.UTC(), hex.EncodeToString(hash[:]), summary, nil
}

func (s *Postgres) ImportSQLPlan(ctx context.Context, assetID, queryID, observed string, raw json.RawMessage) (any, error) {
	at, hash, summary, err := sanitizeSQLPlan(queryID, observed, raw)
	if err != nil {
		return nil, err
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM assets WHERE id=$1)`, assetID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrAssetNotFound
	}
	var output []byte
	err = s.pool.QueryRow(ctx, `INSERT INTO sql_plan_snapshots
		(id,asset_id,query_id,observed_at,source,shape_hash,summary_json,imported_at)
		VALUES(gen_random_uuid(),$1,$2,$3,'operator_import',$4,$5::jsonb,now())
		RETURNING jsonb_build_object('id',id,'assetId',asset_id,'queryId',query_id,
			'observedAt',observed_at,'source',source,'shapeHash',shape_hash,'summary',summary_json)`,
		assetID, queryID, at, hash, summary).Scan(&output)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAssetNotFound
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(output), nil
}
