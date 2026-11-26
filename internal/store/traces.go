package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"sync"
	"time"

	assetconfig "github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/cuipengdba/dbsphere/internal/collector"
	"github.com/cuipengdba/dbsphere/internal/security"
	"github.com/cuipengdba/dbsphere/internal/trace"
	"github.com/jackc/pgx/v5"
)

func (s *Postgres) TraceAssets(ctx context.Context, key []byte) ([]trace.Asset, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text,host,port,database_name,pooler_url_enc,pooler_endpoint,
		CASE WHEN runtime IN ('mysql','redis','etcd','dm8','kingbase') THEN runtime ELSE 'postgres' END FROM assets`)
	if err != nil {
		return nil, fmt.Errorf("list OTLP asset identities: %w", err)
	}
	defer rows.Close()
	assets := []trace.Asset{}
	for rows.Next() {
		var asset trace.Asset
		var poolerEncrypted *string
		var poolerEndpoint *string
		if err := rows.Scan(&asset.ID, &asset.Host, &asset.Port, &asset.Database,
			&poolerEncrypted, &poolerEndpoint, &asset.Engine); err != nil {
			return nil, err
		}
		if poolerEndpoint != nil {
			asset.PoolerHost, asset.PoolerPort, _ = assetconfig.ParsePoolerEndpoint(*poolerEndpoint)
			assets = append(assets, asset)
			continue
		}
		if poolerEncrypted != nil {
			plain, err := security.Decrypt(key, *poolerEncrypted)
			if err != nil {
				assets = append(assets, asset)
				continue
			}
			endpoint, err := url.Parse(plain)
			if err != nil || endpoint.Hostname() == "" {
				assets = append(assets, asset)
				continue
			}
			asset.PoolerHost = endpoint.Hostname()
			asset.PoolerPort = 5432
			if endpoint.Port() != "" {
				asset.PoolerPort, err = strconv.Atoi(endpoint.Port())
				if err != nil || asset.PoolerPort < 1 || asset.PoolerPort > 65535 {
					asset.PoolerHost = ""
					asset.PoolerPort = 0
				}
			}
		}
		assets = append(assets, asset)
	}
	return assets, rows.Err()
}

func (s *Postgres) SaveTraceSpans(ctx context.Context, spans []trace.Span) (int, error) {
	imported, _, err := s.SaveTraceBatch(ctx, spans, nil)
	return imported, err
}

func insertTraceSpan(ctx context.Context, tx pgx.Tx, span trace.Span) (int, error) {
	route := span.Route
	if route == "" {
		route = "direct"
	}
	if route != "direct" && route != "pgbouncer" {
		return 0, fmt.Errorf("invalid OTLP database route")
	}
	var parent any
	if span.ParentSpanID != "" {
		parent = span.ParentSpanID
	}
	var queryID any
	if span.QueryID != "" {
		queryID = span.QueryID
	}
	var appPod any
	if span.Kind == "postgresql_client" && span.AppPodNamespace != "" &&
		span.AppPodName != "" && span.AppPodUID != "" {
		value, err := json.Marshal(map[string]string{"namespace": span.AppPodNamespace,
			"name": span.AppPodName, "uid": span.AppPodUID})
		if err != nil {
			return 0, fmt.Errorf("encode OTLP application Pod identity: %w", err)
		}
		appPod = string(value)
	}
	probability, err := traceSamplingValue(span)
	if err != nil {
		return 0, err
	}
	result, err := tx.Exec(ctx, `INSERT INTO trace_spans
		(asset_id,trace_id,span_id,parent_span_id,service,kind,started_at,ended_at,status,backend_pid,query_id,route,app_pod_reported,
		 received_at,sampling_probability,endpoint_name)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,nullif($16,''))
		ON CONFLICT(asset_id,trace_id,span_id) DO NOTHING`, span.AssetID,
		span.TraceID, span.SpanID, parent, span.Service, span.Kind,
		span.Start, span.End, span.Status, optionalBackendPID(span.BackendPID), queryID, route, appPod,
		traceReceivedAt(span), probability, span.Endpoint)
	if err != nil {
		return 0, fmt.Errorf("save OTLP span: %w", err)
	}
	return int(result.RowsAffected()), nil
}

func traceReceivedAt(span trace.Span) time.Time {
	if span.ReceivedAt.IsZero() {
		return time.Now().UTC()
	}
	return span.ReceivedAt.UTC()
}

func traceSamplingValue(span trace.Span) (any, error) {
	if span.SamplingProbability == nil {
		return nil, nil
	}
	value := *span.SamplingProbability
	if value <= 0 || value > 1 || math.IsNaN(value) || math.IsInf(value, 0) {
		return nil, fmt.Errorf("invalid declared trace sampling probability")
	}
	return value, nil
}

// SaveTraceBatch attaches service ancestors only to a mapped database chain.
// A short-lived sanitized buffer covers Collector batches arriving out of order.
func (s *Postgres) SaveTraceBatch(ctx context.Context, spans, candidates []trace.Span) (int, int, error) {
	imported, linked, err := s.saveTraceBatch(ctx, spans, candidates, true)
	if err == nil {
		s.verifyTracePods(ctx, spans)
	}
	return imported, linked, err
}

type tracePodCacheEntry struct {
	identity   collector.KubernetesPodIdentity
	observedAt time.Time
	verified   bool
}

func (s *Postgres) cachedTracePod(key string, now time.Time) (tracePodCacheEntry, bool) {
	s.tracePodMu.Lock()
	defer s.tracePodMu.Unlock()
	entry, ok := s.tracePodCache[key]
	if !ok {
		return tracePodCacheEntry{}, false
	}
	ttl := 30 * time.Second
	if entry.verified {
		ttl = 10 * time.Minute
	}
	if now.Sub(entry.observedAt) > ttl {
		delete(s.tracePodCache, key)
		return tracePodCacheEntry{}, false
	}
	return entry, true
}

func (s *Postgres) rememberTracePod(key string, entry tracePodCacheEntry) {
	s.tracePodMu.Lock()
	defer s.tracePodMu.Unlock()
	if s.tracePodCache == nil || len(s.tracePodCache) >= 1000 {
		s.tracePodCache = map[string]tracePodCacheEntry{}
	}
	s.tracePodCache[key] = entry
}

// verifyTracePods is best effort: a missing or denied Kubernetes API must not
// interrupt OTLP ingestion, and unverified metadata is never drawn as a link.
func (s *Postgres) verifyTracePods(ctx context.Context, spans []trace.Span) {
	if len(spans) == 0 {
		return
	}
	type podLookup struct {
		cluster, namespace, name, uid string
		spans                         []trace.Span
	}
	groups := map[string]*podLookup{}
	clusters := map[string]string{}
	now := time.Now().UTC()
	for _, span := range spans {
		if span.Kind != "postgresql_client" || span.AssetID == "" ||
			span.AppPodNamespace == "" || span.AppPodName == "" || span.AppPodUID == "" ||
			span.End.Before(now.Add(-2*time.Minute)) || span.End.After(now.Add(5*time.Second)) {
			continue
		}
		cluster, known := clusters[span.AssetID]
		if !known {
			var runtime string
			var contextName *string
			if err := s.pool.QueryRow(ctx, `SELECT runtime,k8s_cluster FROM assets WHERE id=$1`,
				span.AssetID).Scan(&runtime, &contextName); err != nil || runtime != "k8s" || contextName == nil {
				clusters[span.AssetID] = ""
				continue
			}
			cluster = *contextName
			clusters[span.AssetID] = cluster
		}
		if cluster == "" {
			continue
		}
		key := span.AssetID + ":" + cluster + ":" + span.AppPodNamespace + ":" +
			span.AppPodName + ":" + span.AppPodUID
		group := groups[key]
		if group == nil {
			if len(groups) >= 8 {
				continue
			}
			group = &podLookup{cluster: cluster, namespace: span.AppPodNamespace,
				name: span.AppPodName, uid: span.AppPodUID}
			groups[key] = group
		}
		group.spans = append(group.spans, span)
	}
	var workers sync.WaitGroup
	semaphore := make(chan struct{}, 4)
	for key, group := range groups {
		workers.Add(1)
		go func(key string, group *podLookup) {
			defer workers.Done()
			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-semaphore }()
			entry, cached := s.cachedTracePod(key, time.Now().UTC())
			if !cached {
				identity, err := collector.VerifyKubernetesPodIdentity(ctx,
					collector.KubernetesTarget{Cluster: group.cluster, Namespace: group.namespace,
						Pod: group.name}, group.uid)
				entry = tracePodCacheEntry{identity: identity, observedAt: time.Now().UTC(), verified: err == nil}
				s.rememberTracePod(key, entry)
			}
			if !entry.verified {
				return
			}
			verified, err := json.Marshal(map[string]any{"cluster": group.cluster, "namespace": entry.identity.Namespace,
				"name": entry.identity.Name, "uid": entry.identity.UID, "node": entry.identity.Node,
				"pvcs": entry.identity.PVCs, "pvcStates": entry.identity.PVCStates,
				"containers": entry.identity.Containers,
				"createdAt":  entry.identity.CreatedAt,
				"observedAt": entry.observedAt})
			if err != nil {
				return
			}
			for _, span := range group.spans {
				if entry.identity.CreatedAt.After(span.End.Add(5 * time.Second)) {
					continue
				}
				_, _ = s.pool.Exec(ctx, `UPDATE trace_spans SET app_pod_verified=$1::jsonb
					WHERE asset_id=$2 AND trace_id=$3 AND span_id=$4
					AND app_pod_reported->>'namespace'=$5 AND app_pod_reported->>'name'=$6
					AND app_pod_reported->>'uid'=$7`, string(verified), span.AssetID, span.TraceID,
					span.SpanID, group.namespace, group.name, group.uid)
			}
		}(key, group)
	}
	workers.Wait()
}

func (s *Postgres) saveTraceBatch(ctx context.Context, spans, candidates []trace.Span, reconcile bool) (int, int, error) {
	if len(spans) > 4096 || len(candidates) > 256 {
		return 0, 0, fmt.Errorf("too many OTLP spans")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback(ctx)
	imported := 0
	for _, span := range spans {
		count, err := insertTraceSpan(ctx, tx, span)
		if err != nil {
			return 0, 0, err
		}
		imported += count
	}
	byID := map[string]trace.Span{}
	currentCandidates := map[string]bool{}
	now := time.Now().UTC()
	for _, candidate := range candidates {
		if candidate.AssetID != "" || candidate.Kind != "service" {
			return 0, 0, fmt.Errorf("invalid unmapped service span")
		}
		key := candidate.TraceID + ":" + candidate.SpanID
		if currentCandidates[key] {
			return 0, 0, fmt.Errorf("duplicate service span identity")
		}
		currentCandidates[key] = true
		byID[key] = candidate
		if candidate.End.Before(now.Add(-2*time.Minute)) || candidate.End.After(now.Add(5*time.Second)) {
			continue
		}
		var parent any
		if candidate.ParentSpanID != "" {
			parent = candidate.ParentSpanID
		}
		probability, err := traceSamplingValue(candidate)
		if err != nil {
			return 0, 0, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO trace_pending_spans
			(trace_id,span_id,parent_span_id,service,started_at,ended_at,status,ingress_at,sampling_probability)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(trace_id,span_id) DO NOTHING`,
			candidate.TraceID, candidate.SpanID, parent, candidate.Service,
			candidate.Start, candidate.End, candidate.Status, traceReceivedAt(candidate), probability); err != nil {
			return 0, 0, fmt.Errorf("buffer unmapped OTLP service span: %w", err)
		}
	}
	loadPending := func(key, traceID, spanID string) (trace.Span, bool, error) {
		if span, ok := byID[key]; ok {
			return span, true, nil
		}
		var span trace.Span
		var parent *string
		err := tx.QueryRow(ctx, `SELECT parent_span_id,service,started_at,ended_at,status,
			coalesce(ingress_at,received_at),sampling_probability
			FROM trace_pending_spans WHERE trace_id=$1 AND span_id=$2
			AND received_at >= now()-interval '10 minutes'`, traceID, spanID).
			Scan(&parent, &span.Service, &span.Start, &span.End, &span.Status,
				&span.ReceivedAt, &span.SamplingProbability)
		if err == pgx.ErrNoRows {
			return trace.Span{}, false, nil
		}
		if err != nil {
			return trace.Span{}, false, fmt.Errorf("read pending OTLP parent: %w", err)
		}
		span.TraceID, span.SpanID, span.Kind = traceID, spanID, "service"
		if parent != nil {
			span.ParentSpanID = *parent
		}
		byID[key] = span
		return span, true, nil
	}
	links := map[string]map[string]bool{}
	queue := []string{}
	addLink := func(key, assetID string) {
		if links[key] == nil {
			links[key] = map[string]bool{}
		}
		if !links[key][assetID] {
			links[key][assetID] = true
			queue = append(queue, key)
		}
	}
	for key := range currentCandidates {
		candidate := byID[key]
		rows, err := tx.Query(ctx, `SELECT DISTINCT asset_id::text FROM trace_spans
			WHERE trace_id=$1 AND parent_span_id=$2 AND started_at BETWEEN $3 AND $4 LIMIT 33`,
			candidate.TraceID, candidate.SpanID, candidate.Start.Add(-5*time.Second), candidate.End.Add(5*time.Second))
		if err != nil {
			return 0, 0, fmt.Errorf("resolve OTLP parent asset: %w", err)
		}
		assets := map[string]bool{}
		for rows.Next() {
			var assetID string
			if err := rows.Scan(&assetID); err != nil {
				rows.Close()
				return 0, 0, err
			}
			assets[assetID] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return 0, 0, err
		}
		if len(assets) > 32 {
			return 0, 0, fmt.Errorf("OTLP parent matches too many assets")
		}
		for assetID := range assets {
			addLink(key, assetID)
		}
	}
	for _, child := range spans {
		if child.AssetID == "" || child.ParentSpanID == "" {
			continue
		}
		parentKey := child.TraceID + ":" + child.ParentSpanID
		parent, exists, err := loadPending(parentKey, child.TraceID, child.ParentSpanID)
		if err != nil {
			return 0, 0, err
		}
		if exists && !parent.Start.After(child.End.Add(5*time.Second)) &&
			!parent.End.Before(child.Start.Add(-5*time.Second)) {
			addLink(parentKey, child.AssetID)
		}
	}
	processed := map[string]map[string]bool{}
	for len(queue) > 0 {
		key := queue[0]
		queue = queue[1:]
		candidate := byID[key]
		if processed[key] == nil {
			processed[key] = map[string]bool{}
		}
		for assetID := range links[key] {
			if processed[key][assetID] {
				continue
			}
			processed[key][assetID] = true
			candidate.AssetID = assetID
			count, err := insertTraceSpan(ctx, tx, candidate)
			if err != nil {
				return 0, 0, err
			}
			imported += count
			if imported > 4096 {
				return 0, 0, fmt.Errorf("too many linked OTLP spans")
			}
			parentKey := candidate.TraceID + ":" + candidate.ParentSpanID
			if candidate.ParentSpanID == "" {
				continue
			}
			parent, exists, err := loadPending(parentKey, candidate.TraceID, candidate.ParentSpanID)
			if err != nil {
				return 0, 0, err
			}
			if !exists || parent.Start.After(candidate.End.Add(5*time.Second)) ||
				parent.End.Before(candidate.Start.Add(-5*time.Second)) {
				continue
			}
			addLink(parentKey, assetID)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	// The Collector can export parent and child requests concurrently. A second
	// transaction after commit observes whichever request committed first.
	if reconcile {
		traceIDs := map[string]bool{}
		for _, span := range spans {
			traceIDs[span.TraceID] = true
		}
		for _, candidate := range candidates {
			traceIDs[candidate.TraceID] = true
		}
		for traceID := range traceIDs {
			pending, err := s.recentPendingTraceSpans(ctx, traceID)
			if err != nil {
				return imported, 0, err
			}
			if len(pending) == 0 {
				continue
			}
			linked, _, err := s.saveTraceBatch(ctx, nil, pending, false)
			if err != nil {
				return imported, 0, err
			}
			imported += linked
		}
	}
	linkedCandidates := 0
	for key := range processed {
		if currentCandidates[key] {
			linkedCandidates++
		}
	}
	return imported, linkedCandidates, nil
}

func (s *Postgres) recentPendingTraceSpans(ctx context.Context, traceID string) ([]trace.Span, error) {
	rows, err := s.pool.Query(ctx, `SELECT span_id,parent_span_id,service,started_at,ended_at,status,
		coalesce(ingress_at,received_at),sampling_probability
		FROM trace_pending_spans WHERE trace_id=$1 AND received_at >= now()-interval '10 minutes'
		ORDER BY received_at DESC LIMIT 256`, traceID)
	if err != nil {
		return nil, fmt.Errorf("read recent pending OTLP spans: %w", err)
	}
	defer rows.Close()
	spans := []trace.Span{}
	for rows.Next() {
		span := trace.Span{TraceID: traceID, Kind: "service"}
		var parent *string
		if err := rows.Scan(&span.SpanID, &parent, &span.Service, &span.Start, &span.End,
			&span.Status, &span.ReceivedAt, &span.SamplingProbability); err != nil {
			return nil, err
		}
		if parent != nil {
			span.ParentSpanID = *parent
		}
		spans = append(spans, span)
	}
	return spans, rows.Err()
}

func optionalBackendPID(value int) any {
	if value > 0 {
		return value
	}
	return nil
}

func (s *Postgres) PruneTraceSpans(ctx context.Context, days int) (int64, error) {
	if days < 1 || days > 3650 {
		return 0, fmt.Errorf("invalid trace retention")
	}
	result, err := s.pool.Exec(ctx, `WITH expired AS (
		SELECT asset_id,trace_id,span_id FROM trace_spans
		WHERE ended_at < now() - ($1::int * interval '1 day')
		ORDER BY ended_at LIMIT 10000)
		DELETE FROM trace_spans t USING expired e
		WHERE t.asset_id=e.asset_id AND t.trace_id=e.trace_id AND t.span_id=e.span_id`, days)
	if err != nil {
		return 0, fmt.Errorf("prune OTLP spans: %w", err)
	}
	return result.RowsAffected(), nil
}

func (s *Postgres) PrunePendingTraceSpans(ctx context.Context) (int64, error) {
	result, err := s.pool.Exec(ctx, `WITH expired AS (
		SELECT trace_id,span_id FROM trace_pending_spans
		WHERE received_at < now()-interval '10 minutes'
		ORDER BY received_at LIMIT 10000)
		DELETE FROM trace_pending_spans p USING expired e
		WHERE p.trace_id=e.trace_id AND p.span_id=e.span_id`)
	if err != nil {
		return 0, fmt.Errorf("prune pending OTLP spans: %w", err)
	}
	return result.RowsAffected(), nil
}
