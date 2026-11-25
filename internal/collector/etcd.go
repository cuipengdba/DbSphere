package collector

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cuipengdba/dbsphere/pkg/collectoradapter"
)

var (
	ErrEtcdVersion    = errors.New("etcd monitoring requires etcd 3.5 or 3.6")
	ErrEtcdTLS        = errors.New("etcd credential or TLS configuration is invalid")
	ErrEtcdPermission = errors.New("etcd monitoring endpoint rejected the credential")
	etcdVersion       = regexp.MustCompile(`^3\.[56]\.[0-9]{1,2}$`)
	etcdMemberName    = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)
)

type EtcdBucket struct {
	UpperSeconds float64 `json:"upperSeconds"`
	Count        float64 `json:"count"`
}

type EtcdHistogram struct {
	Count   *float64     `json:"count"`
	Sum     *float64     `json:"sumSeconds"`
	Buckets []EtcdBucket `json:"buckets"`
}

type EtcdMember struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Endpoint string `json:"endpoint,omitempty"`
	Learner  bool   `json:"learner"`
}

type EtcdKubeMetrics struct {
	Context  string        `json:"context"`
	Latency  EtcdHistogram `json:"latency"`
	Requests *float64      `json:"requests"`
	Errors   *float64      `json:"errors"`
	Start    *float64      `json:"processStarted"`
}

type EtcdMetrics struct {
	Version     string           `json:"version"`
	Health      *bool            `json:"health"`
	ClusterID   string           `json:"clusterId,omitempty"`
	MemberID    string           `json:"memberId,omitempty"`
	LeaderID    string           `json:"leaderId,omitempty"`
	RaftTerm    string           `json:"raftTerm,omitempty"`
	RaftIndex   string           `json:"raftIndex,omitempty"`
	RaftApplied string           `json:"raftApplied,omitempty"`
	HasLeader   *float64         `json:"hasLeader"`
	IsLeader    *float64         `json:"isLeader"`
	Started     *float64         `json:"processStarted"`
	Elections   *float64         `json:"leaderChanges"`
	Pending     *float64         `json:"proposalsPending"`
	Committed   *float64         `json:"proposalsCommitted"`
	Applied     *float64         `json:"proposalsApplied"`
	Failed      *float64         `json:"proposalsFailed"`
	Size        *float64         `json:"sizeBytes"`
	InUse       *float64         `json:"inUseBytes"`
	Quota       *float64         `json:"quotaBytes"`
	WAL         EtcdHistogram    `json:"wal"`
	Backend     EtcdHistogram    `json:"backend"`
	CertUntil   *time.Time       `json:"serverCertUntil"`
	ClientUntil *time.Time       `json:"clientCertUntil"`
	Members     []EtcdMember     `json:"members"`
	Kubernetes  *EtcdKubeMetrics `json:"kubernetes,omitempty"`
}

type EtcdAdapter struct{}

var _ collectoradapter.Adapter = EtcdAdapter{}

func (EtcdAdapter) Collect(ctx context.Context, target Target, token string) (collectoradapter.Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 13*time.Second)
	defer cancel()
	detail := EtcdMetrics{}
	scheme := "http"
	transport := &http.Transport{DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
		TLSHandshakeTimeout: 2 * time.Second, ResponseHeaderTimeout: 3 * time.Second}
	defer transport.CloseIdleConnections()
	if target.SSLMode == "require" {
		scheme = "https"
		config := &tls.Config{ServerName: target.Host, MinVersion: tls.VersionTLS12}
		if path := os.Getenv("DBSPHERE_ETCD_CA_FILE"); path != "" {
			pem, err := os.ReadFile(path)
			roots := x509.NewCertPool()
			if err != nil || !roots.AppendCertsFromPEM(pem) {
				return collectoradapter.Snapshot{}, ErrEtcdTLS
			}
			config.RootCAs = roots
		}
		cert, key := os.Getenv("DBSPHERE_ETCD_CERT_FILE"), os.Getenv("DBSPHERE_ETCD_KEY_FILE")
		if cert != "" || key != "" {
			identity, err := tls.LoadX509KeyPair(cert, key)
			if err != nil || identity.Leaf == nil {
				return collectoradapter.Snapshot{}, ErrEtcdTLS
			}
			config.Certificates = []tls.Certificate{identity}
			detail.ClientUntil = &identity.Leaf.NotAfter
		}
		transport.TLSClientConfig = config
	} else if token != "" {
		return collectoradapter.Snapshot{}, ErrEtcdTLS
	}
	client := &http.Client{Transport: transport, Timeout: 4 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	base := url.URL{Scheme: scheme, Host: net.JoinHostPort(target.Host, strconv.Itoa(target.Port))}
	read := func(method, path string, allowUnhealthy bool) ([]byte, error) {
		endpoint := base
		endpoint.Path = path
		var body io.Reader
		if method == http.MethodPost {
			body = strings.NewReader("{}")
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
		if err != nil {
			return nil, err
		}
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", token)
		}
		response, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		if response.TLS != nil && len(response.TLS.PeerCertificates) > 0 {
			detail.CertUntil = &response.TLS.PeerCertificates[0].NotAfter
		}
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			return nil, ErrEtcdPermission
		}
		if response.StatusCode != http.StatusOK && !(allowUnhealthy && response.StatusCode == http.StatusServiceUnavailable) {
			return nil, errors.New("etcd monitoring endpoint returned an error")
		}
		return readBoundedMetrics(response.Body, 4<<20)
	}
	body, err := read(http.MethodGet, "/version", false)
	versionErr := err
	var version struct {
		Server string `json:"etcdserver"`
	}
	var metricsBody []byte
	if err != nil {
		metricsBody, err = read(http.MethodGet, "/metrics", false)
		if err != nil {
			return collectoradapter.Snapshot{}, versionErr
		}
		version.Server = etcdMetricLabel(metricsBody, "etcd_server_version", "server_version")
	} else if json.Unmarshal(body, &version) != nil {
		return collectoradapter.Snapshot{}, ErrEtcdVersion
	}
	if !etcdVersion.MatchString(version.Server) {
		return collectoradapter.Snapshot{}, ErrEtcdVersion
	}
	detail.Version = version.Server
	minor, _ := strconv.Atoi(version.Server[2:3])
	patch, _ := strconv.Atoi(version.Server[4:])
	report := collectoradapter.Report{Contract: collectoradapter.ContractVersion, Engine: "etcd", Product: "etcd",
		Profile: "etcd-http-metrics", Version: 30000 + minor*100 + patch, Signals: []collectoradapter.Signal{},
		Transport: collectoradapter.Transport{Encrypted: detail.CertUntil != nil, Verified: detail.CertUntil != nil, ReadOnly: true, Protocol: scheme}}
	record := func(key, source string, err error) {
		state := collectoradapter.Signal{Key: key, Source: source, Status: "ok"}
		if err != nil {
			state.Status, state.Reason = "unavailable", "端点未返回可用数据"
		}
		if errors.Is(err, ErrEtcdPermission) {
			state.Status, state.Reason = "permission_denied", "端点未授权"
		}
		report.Signals = append(report.Signals, state)
	}
	body, err = read(http.MethodGet, "/health", true)
	if err == nil {
		var health struct {
			Value json.RawMessage `json:"health"`
		}
		if json.Unmarshal(body, &health) == nil {
			v := strings.Trim(string(health.Value), `"`)
			if v == "true" || v == "false" {
				value := v == "true"
				detail.Health = &value
			}
		}
		if detail.Health == nil {
			err = errors.New("invalid etcd health response")
		}
	}
	record("etcdHealth", "/health", err)
	body, err = read(http.MethodPost, "/v3/maintenance/status", false)
	if err == nil {
		var status struct {
			Header struct {
				Cluster string `json:"cluster_id"`
				Member  string `json:"member_id"`
			} `json:"header"`
			Leader  string `json:"leader"`
			Term    string `json:"raftTerm"`
			Index   string `json:"raftIndex"`
			Applied string `json:"raftAppliedIndex"`
		}
		err = json.Unmarshal(body, &status)
		if err == nil {
			detail.ClusterID, detail.MemberID, detail.LeaderID = etcdHexID(status.Header.Cluster), etcdHexID(status.Header.Member), etcdHexID(status.Leader)
			detail.RaftTerm, detail.RaftIndex, detail.RaftApplied = etcdDecimal(status.Term), etcdDecimal(status.Index), etcdDecimal(status.Applied)
			if detail.MemberID == "" || detail.ClusterID == "" {
				err = errors.New("missing etcd identity")
			}
		}
	}
	record("etcdRaft", "/v3/maintenance/status", err)
	body, err = read(http.MethodPost, "/v3/cluster/member/list", false)
	if err == nil {
		var membership struct {
			Members []struct {
				ID      string   `json:"ID"`
				Name    string   `json:"name"`
				URLs    []string `json:"clientURLs"`
				Learner bool     `json:"isLearner"`
			} `json:"members"`
		}
		err = json.Unmarshal(body, &membership)
		if err == nil && len(membership.Members) <= 32 {
			detail.Members = []EtcdMember{}
			for _, member := range membership.Members {
				id := etcdHexID(member.ID)
				if id == "" {
					continue
				}
				node := EtcdMember{ID: id, Learner: member.Learner, Name: redisEnum(member.Name, etcdMemberName)}
				if len(member.URLs) > 0 {
					address, e := url.Parse(member.URLs[0])
					if e == nil && (address.Scheme == "http" || address.Scheme == "https") && address.User == nil && len(address.Host) <= 260 {
						node.Endpoint = address.Scheme + "://" + address.Host
					}
				}
				detail.Members = append(detail.Members, node)
			}
		} else if err == nil {
			err = errors.New("etcd membership exceeds 32 members")
		}
	}
	record("etcdMembers", "/v3/cluster/member/list", err)
	body, err = metricsBody, nil
	if body == nil {
		body, err = read(http.MethodGet, "/metrics", false)
	}
	if err == nil {
		id := etcdMetricLabel(body, "etcd_server_id", "server_id")
		if value, parseErr := strconv.ParseUint(id, 16, 64); parseErr == nil && value > 0 {
			id = strconv.FormatUint(value, 16)
		} else {
			id = ""
		}
		if id != "" && detail.MemberID != "" && id != detail.MemberID {
			err = errors.New("metrics and member status came from different etcd members")
		} else {
			parseEtcdMetrics(body, &detail)
			if detail.MemberID == "" {
				detail.MemberID = id
			}
			if detail.HasLeader == nil || detail.Started == nil {
				err = errors.New("missing etcd metrics")
			}
		}
	}
	record("etcdMetrics", "/metrics", err)
	if target.KubernetesCluster != "" {
		detail.Kubernetes, err = readEtcdAPIServer(ctx, target.KubernetesCluster)
		record("etcdAPIServer", "Kubernetes /metrics", err)
	}
	if detail.HasLeader != nil && *detail.HasLeader == 0 {
		report.Role = "no-leader"
	} else if detail.IsLeader != nil && *detail.IsLeader == 1 || detail.LeaderID != "" && detail.LeaderID == detail.MemberID {
		report.Role = "leader"
	} else if detail.LeaderID != "" || detail.IsLeader != nil && detail.HasLeader != nil {
		report.Role = "follower"
	}
	return collectoradapter.Snapshot{Report: report, Metrics: map[string]any{
		"etcd": detail, "role": nullableCollectorRole(report.Role), "version_num": report.Version,
	}}, nil
}

func readBoundedMetrics(r io.Reader, max int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, max+1))
	if err == nil && int64(len(body)) > max {
		err = errors.New("monitoring response exceeded its size limit")
	}
	return body, err
}

func etcdHexID(raw string) string {
	if value, err := strconv.ParseUint(raw, 10, 64); err == nil && value != 0 {
		return strconv.FormatUint(value, 16)
	}
	return ""
}

func etcdDecimal(raw string) string {
	if value, err := strconv.ParseUint(raw, 10, 64); err == nil {
		return strconv.FormatUint(value, 10)
	}
	return ""
}

func etcdMetricLabel(body []byte, name, label string) string {
	for line := range strings.SplitSeq(string(body), "\n") {
		if !strings.HasPrefix(line, name+"{") {
			continue
		}
		if parts := metricLine.FindStringSubmatch(line); parts != nil {
			return parseMetricLabels(parts[2])[label]
		}
	}
	return ""
}

func parseEtcdMetrics(body []byte, d *EtcdMetrics) {
	fields := map[string]**float64{
		"etcd_server_has_leader": &d.HasLeader, "etcd_server_is_leader": &d.IsLeader,
		"etcd_server_leader_changes_seen_total": &d.Elections, "etcd_server_proposals_pending": &d.Pending,
		"etcd_server_proposals_committed_total": &d.Committed, "etcd_server_proposals_applied_total": &d.Applied,
		"etcd_server_proposals_failed_total": &d.Failed, "etcd_server_quota_backend_bytes": &d.Quota,
		"etcd_mvcc_db_total_size_in_bytes": &d.Size, "etcd_mvcc_db_total_size_in_use_in_bytes": &d.InUse,
		"process_start_time_seconds": &d.Started,
	}
	observationMetrics(body, fields, map[string]*EtcdHistogram{
		"etcd_disk_wal_fsync_duration_seconds": &d.WAL, "etcd_disk_backend_commit_duration_seconds": &d.Backend,
	})
}

func observationMetrics(body []byte, fields map[string]**float64, histograms map[string]*EtcdHistogram) {
	buckets := map[string]map[float64]float64{}
	add := func(into **float64, value float64) {
		if *into != nil {
			value += **into
		}
		*into = &value
	}
	for line := range strings.SplitSeq(string(body), "\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		key := line
		if split := strings.IndexAny(line, "{ \t"); split >= 0 {
			key = line[:split]
		}
		var family, kind string
		for _, s := range []string{"bucket", "count", "sum"} {
			if prefix, ok := strings.CutSuffix(key, "_"+s); ok {
				family, kind = prefix, s
				break
			}
		}
		field, hist := fields[key], histograms[family]
		if field == nil && hist == nil {
			continue
		}
		match := metricLine.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		value, err := strconv.ParseFloat(match[3], 64)
		if err != nil || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		if field != nil {
			add(field, value)
			continue
		}
		switch kind {
		case "sum":
			add(&hist.Sum, value)
		case "count":
			add(&hist.Count, value)
		case "bucket":
			upper, err := strconv.ParseFloat(parseMetricLabels(match[2])["le"], 64)
			if err != nil || upper < 0 || math.IsNaN(upper) || math.IsInf(upper, 0) {
				continue
			}
			if buckets[family] == nil {
				buckets[family] = map[float64]float64{}
			}
			if _, exists := buckets[family][upper]; len(buckets[family]) < 64 || exists {
				buckets[family][upper] += value
			}
		}
	}
	for name, points := range buckets {
		hist := histograms[name]
		for upper, count := range points {
			hist.Buckets = append(hist.Buckets, EtcdBucket{UpperSeconds: upper, Count: count})
		}
		sort.Slice(hist.Buckets, func(i, j int) bool { return hist.Buckets[i].UpperSeconds < hist.Buckets[j].UpperSeconds })
	}
}

func readEtcdAPIServer(ctx context.Context, contextName string) (*EtcdKubeMetrics, error) {
	kube, err := kubeCredentials(KubernetesTarget{Cluster: contextName})
	if err != nil {
		return nil, err
	}
	endpoint := *kube.base
	endpoint.Path = "/metrics"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+kube.token)
	req.Header.Set("Accept", "text/plain")
	response, err := kube.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return nil, ErrEtcdPermission
	}
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("Kubernetes metrics endpoint unavailable")
	}
	body, err := readBoundedMetrics(response.Body, 16<<20)
	if err != nil {
		return nil, err
	}
	result := &EtcdKubeMetrics{Context: contextName}
	observationMetrics(body, map[string]**float64{"etcd_requests_total": &result.Requests, "etcd_request_errors_total": &result.Errors,
		"process_start_time_seconds": &result.Start}, map[string]*EtcdHistogram{"etcd_request_duration_seconds": &result.Latency})
	if result.Requests == nil && result.Latency.Count == nil {
		return nil, errors.New("API server has no observed etcd request metrics")
	}
	return result, nil
}
