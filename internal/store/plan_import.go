package store

import (
	"bytes"
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
	valueNumber, ok := value.(json.Number)
	if !ok {
		return nil
	}
	n, err := valueNumber.Float64()
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		return nil
	}
	return n
}

func sanitizeSQLPlan(queryID, observed string, raw json.RawMessage) (time.Time, string, []byte, error) {
	queryID = canonicalSQLQueryID(queryID)
	if len(raw) == 0 || len(raw) > 50000 || queryID == "" || !json.Valid(raw) {
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
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&input) != nil {
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
	queryIdentity := "operator"
	if native, ok := root["Query Identifier"].(json.Number); ok && native != "0" {
		if canonicalSQLQueryID(native.String()) != queryID {
			return time.Time{}, "", nil, ErrInvalidSQLPlan
		}
		queryIdentity = "explain"
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
		join, _ := node["Join Type"].(string)
		switch join {
		case "Inner", "Left", "Right", "Full", "Semi", "Anti", "Right Anti", "Right Semi":
		default:
			join = ""
		}
		nodes = append(nodes, map[string]any{
			"depth": depth, "type": typeName,
			"joinType":  join,
			"totalCost": planNumber(node["Total Cost"]), "planRows": planNumber(node["Plan Rows"]),
			"actualTotalMs": planNumber(node["Actual Total Time"]), "actualRows": planNumber(node["Actual Rows"]),
			"actualLoops": planNumber(node["Actual Loops"]), "sharedHitBlocks": planNumber(node["Shared Hit Blocks"]),
			"sharedReadBlocks": planNumber(node["Shared Read Blocks"]), "tempWrittenBlocks": planNumber(node["Temp Written Blocks"]),
			"tempReadBlocks": planNumber(node["Temp Read Blocks"]),
		})
		shape = append(shape, strconv.Itoa(depth)+":"+typeName)
		if join != "" {
			shape[len(shape)-1] += ":" + join
		}
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
		"executionMs": planNumber(root["Execution Time"]), "nodes": nodes, "queryIdentity": queryIdentity,
	})
	if err != nil {
		return time.Time{}, "", nil, err
	}
	return at.UTC(), hex.EncodeToString(hash[:]), summary, nil
}

func (s *Postgres) ImportSQLPlan(ctx context.Context, assetID, queryID, observed string, raw json.RawMessage) (any, error) {
	return s.ImportScopedSQLPlan(ctx, assetID, "", queryID, observed, raw)
}

func (s *Postgres) ImportScopedSQLPlan(ctx context.Context, assetID, database, queryID, observed string, raw json.RawMessage) (any, error) {
	at, hash, summary, err := sanitizeSQLPlan(queryID, observed, raw)
	if err != nil {
		return nil, err
	}
	var configuredDatabase string
	if err := s.pool.QueryRow(ctx, `SELECT database_name FROM assets WHERE id=$1`, assetID).Scan(&configuredDatabase); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAssetNotFound
		}
		return nil, err
	}
	if database != "" && database != configuredDatabase {
		return nil, ErrInvalidSQLPlan
	}
	var output []byte
	err = s.pool.QueryRow(ctx, `INSERT INTO sql_plan_snapshots
		(id,asset_id,query_id,observed_at,source,shape_hash,summary_json,database_name,imported_at)
		SELECT gen_random_uuid(),id,$2,$3,'operator_import',$4,$5::jsonb,database_name,now()
		FROM assets WHERE id=$1 AND database_name=$6
		RETURNING jsonb_build_object('id',id,'assetId',asset_id,'queryId',query_id,
			'observedAt',observed_at,'source',source,'shapeHash',shape_hash,'summary',summary_json,'database',database_name)`,
		assetID, canonicalSQLQueryID(queryID), at, hash, summary, configuredDatabase).Scan(&output)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrAssetNotFound
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(output), nil
}
