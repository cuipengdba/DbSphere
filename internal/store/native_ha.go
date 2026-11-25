package store

import (
	"context"
	"encoding/json"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cuipengdba/dbsphere/internal/collector"
	"github.com/cuipengdba/dbsphere/pkg/collectoradapter"
	"github.com/jackc/pgx/v5"
)

type nativeInfraTarget struct {
	host         *string
	exporterPort *int
	kube         collector.KubernetesTarget
}

func (s *Postgres) collectNativeObservation(ctx context.Context, asset string, port int, infra nativeInfraTarget,
	lease *CollectionLease, collect func() (collectoradapter.Snapshot, error)) (collectoradapter.Snapshot, error) {
	if lease != nil {
		lease.startedAt = time.Now()
	}
	var wait sync.WaitGroup
	var snapshot collectoradapter.Snapshot
	var queryErr error
	var linux, kube, storage, node map[string]any
	wait.Go(func() {
		started := time.Now()
		snapshot, queryErr = collect()
		lease.recordQueryDuration(started)
	})
	if infra.host != nil && infra.exporterPort != nil {
		wait.Go(func() {
			var err error
			linux, err = collector.Linux(ctx, *infra.host, *infra.exporterPort)
			if err != nil {
				linux = map[string]any{"status": "error", "reason": "Linux node_exporter collection failed"}
			} else {
				linux["endpoint"] = net.JoinHostPort(*infra.host, strconv.Itoa(*infra.exporterPort))
			}
		})
	}
	if infra.kube.Cluster != "" && infra.kube.Namespace != "" {
		wait.Go(func() {
			kube = collector.Kubernetes(ctx, infra.kube)
			storage, node = s.collectContainerStorage(ctx, asset, "k8s", port, kube)
		})
	}
	wait.Wait()
	if snapshot.Metrics == nil {
		snapshot.Metrics = make(map[string]any)
	}
	if kube != nil {
		snapshot.Metrics["k8s"] = kube
	}
	if storage != nil {
		snapshot.Metrics["containerStorage"] = storage
	}
	if node != nil && linux["status"] != "ok" {
		linux = node
	}
	if linux != nil {
		snapshot.Metrics["linux"] = linux
	}
	return snapshot, queryErr
}

func nativeHAEvent(kind string) (engine, action string) {
	engine, action, _ = strings.Cut(kind, "_ha_")
	if (engine == "kingbase" || engine == "dm8") && (action == "role" || action == "replication" || action == "archive") {
		return engine, action
	}
	return "", ""
}

func nativeHALabel(engine string) string {
	if engine == "dm8" {
		return "达梦 DM8"
	}
	return "金仓 V9"
}

func recordNativeHAEvents(ctx context.Context, tx pgx.Tx, assetID, sampleID, previousID, engine string,
	now, previousAt time.Time, current *collector.NativeHA, previous map[string]any, interval time.Duration) error {
	if current == nil || current.Node == "" {
		return nil
	}
	old := collector.NativeHA{}
	encoded, err := json.Marshal(object(previous[engine])["ha"])
	if err == nil {
		_ = json.Unmarshal(encoded, &old)
	}
	maxGap := 15 * time.Minute
	if interval > 0 {
		maxGap = 2 * interval
	}
	continuous := old.Node == current.Node && previousID != "" && now.Sub(previousAt) <= maxGap
	title := nativeHALabel(engine)
	group := engine + "_ha_"
	payload := func() map[string]any {
		p := map[string]any{"engine": engine, "topology": current}
		if continuous {
			p["previousSampleId"], p["previousRole"] = previousID, old.Role
		}
		return p
	}
	record := func(action, name, source string, isActive bool, value *int64) error {
		observed, recovered := payload(), payload()
		if value != nil {
			observed["value"], recovered["value"] = *value, *value
		}
		return recordEngineObservationEvent(ctx, tx, assetID, sampleID, now, group+action,
			title+" "+name, source, isActive, observed, recovered)
	}
	roleKnown := current.Role == "primary" || current.Role == "standby"
	stable := engine == "kingbase" || strings.EqualFold(current.State, "open")
	if roleKnown && continuous && (old.Role == "primary" || old.Role == "standby") {
		if moved := old.Role != current.Role; moved || stable {
			if err := record("role", "主备角色发生变化", current.Source, moved, nil); err != nil {
				return err
			}
		}
	}
	if observed, abnormal, gap := nativeReplicationState(engine, current, old, continuous); observed {
		if err := record("replication", "复制异常或回放积压", current.Source, abnormal, gap); err != nil {
			return err
		}
	}
	if current.Archive != nil && current.Archive.Error != nil {
		source := "sys_catalog.sys_stat_archiver"
		if engine == "dm8" {
			source = "V$ARCH_STATUS"
		}
		return record("archive", "归档异常", source, *current.Archive.Error, current.Archive.Failed)
	}
	return nil
}

func nativeReplicationState(engine string, current *collector.NativeHA, old collector.NativeHA, continuous bool) (observed, abnormal bool, gap *int64) {
	gap = current.ReplayGap
	threshold := int64(1024 * 1024)
	if engine == "dm8" {
		threshold = 1000
	}
	peerOK, peerAffected := false, false
	for _, peer := range current.Peers {
		if peer.State == "streaming" || peer.State == "VALID" {
			peerOK = true
		}
		if peer.State == "INVALID" || peer.State == "ASYNC_SEND" || peer.State == "catchup" ||
			(peer.Kind == "upstream" && peer.State != "" && peer.State != "streaming") {
			peerAffected = true
		}
		if peer.ReplayGap != nil && peer.ApplySource == "" && (gap == nil || *peer.ReplayGap > *gap) {
			gap = peer.ReplayGap
		}
	}
	if current.PeersObserved && !current.PeersTruncated && len(current.Peers) == 0 && current.Role == "standby" && engine == "kingbase" {
		// The server read the receiver view successfully. An absent receiver is
		// meaningful for a standby, including repeated samples during an outage.
		return true, true, gap
	}
	if engine == "kingbase" && current.Role == "primary" && current.PeersObserved && continuous &&
		old.Role == current.Role && old.PeersObserved && len(old.Peers) > 0 && len(current.Peers) == 0 {
		return true, true, gap
	}
	observed = gap != nil || (current.PeersObserved && !current.PeersTruncated && (peerOK || peerAffected))
	return observed, peerAffected || (gap != nil && *gap >= threshold), gap
}
