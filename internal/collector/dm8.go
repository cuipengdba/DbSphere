package collector

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cuipengdba/dbsphere/pkg/collectoradapter"
)

var (
	ErrDM8Unconfigured = errors.New("DM8 probe is not configured")
	ErrDM8Unavailable  = errors.New("DM8 probe or database is unavailable")
	ErrDM8Connection   = errors.New("DM8 connection or monitoring authorization was rejected")
	dm8Username        = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,99}$`)
	dm8Host            = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,252}$`)
)

type DM8Wait struct {
	WaitingTransaction  string `json:"waitingTransaction"`
	BlockingTransaction string `json:"blockingTransaction"`
	WaitingSession      string `json:"waitingSession,omitempty"`
	BlockingSession     string `json:"blockingSession,omitempty"`
	WaitMS              int64  `json:"waitMs"`
}

type DM8File struct {
	ID             string `json:"id"`
	Tablespace     string `json:"tablespace"`
	AllocatedBytes int64  `json:"allocatedBytes"`
	FreeBytes      int64  `json:"freeBytes"`
}

type DM8Metrics struct {
	Version        string                    `json:"version"`
	Build          string                    `json:"build"`
	Status         string                    `json:"status"`
	Mode           string                    `json:"mode"`
	StartedAt      string                    `json:"startedAt"`
	Sessions       *int64                    `json:"sessions"`
	ActiveSessions *int64                    `json:"activeSessions"`
	Transactions   *int64                    `json:"transactions"`
	Waits          []DM8Wait                 `json:"waits"`
	Files          []DM8File                 `json:"files"`
	WaitsTruncated bool                      `json:"waitsTruncated"`
	FilesTruncated bool                      `json:"filesTruncated"`
	Signals        []collectoradapter.Signal `json:"signals"`
}

type dm8Request struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type DM8Adapter struct{}

var _ collectoradapter.Adapter = DM8Adapter{}

func dm8Loopback(host string) bool {
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
}

func (DM8Adapter) Collect(ctx context.Context, target Target, password string) (collectoradapter.Snapshot, error) {
	endpoint, err := url.Parse(strings.TrimSpace(os.Getenv("DBSPHERE_DM8_PROBE_URL")))
	token := os.Getenv("DBSPHERE_DM8_PROBE_TOKEN")
	if err != nil || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" ||
		(endpoint.Path != "" && endpoint.Path != "/") || len(token) < 24 ||
		(endpoint.Scheme != "https" && !(endpoint.Scheme == "http" && dm8Loopback(endpoint.Hostname()))) {
		return collectoradapter.Snapshot{}, ErrDM8Unconfigured
	}
	input := dm8Request{Host: target.Host, Port: target.Port, Username: target.Username, Password: password}
	if !input.valid() || (target.SSLMode != "disable" && target.SSLMode != "") {
		return collectoradapter.Snapshot{}, ErrDM8Connection
	}
	body, _ := json.Marshal(input)
	endpoint.Path = "/v1/dm8/sample"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return collectoradapter.Snapshot{}, ErrDM8Unconfigured
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response, err := dm8HTTP.Do(req)
	if err != nil {
		return collectoradapter.Snapshot{}, ErrDM8Unavailable
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden ||
		response.StatusCode == http.StatusUnprocessableEntity {
		return collectoradapter.Snapshot{}, ErrDM8Connection
	}
	if response.StatusCode != http.StatusOK {
		return collectoradapter.Snapshot{}, ErrDM8Unavailable
	}
	var details DM8Metrics
	if err := json.NewDecoder(io.LimitReader(response.Body, 96<<10)).Decode(&details); err != nil ||
		!strings.Contains(details.Version, "DM Database Server") || !strings.HasSuffix(details.Version, " V8") {
		return collectoradapter.Snapshot{}, ErrDM8Unavailable
	}
	role := map[string]string{"NORMAL": "standalone", "PRIMARY": "primary", "STANDBY": "standby"}[details.Mode]
	report := collectoradapter.Report{Contract: collectoradapter.ContractVersion, Engine: "dm8", Product: "Dameng DM8",
		Version: 80000, Role: role, Profile: "dm8-disql", Signals: details.Signals,
		Transport: collectoradapter.Transport{ReadOnly: true, Protocol: "disql"}}
	return collectoradapter.Snapshot{Report: report, Metrics: map[string]any{
		"dm8": details, "role": role, "version_num": 80000,
	}}, nil
}

var dm8HTTP = &http.Client{
	Timeout: 14 * time.Second,
	Transport: func() http.RoundTripper {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		return transport
	}(),
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func (p dm8Request) valid() bool {
	return (dm8Host.MatchString(p.Host) || net.ParseIP(p.Host) != nil) && p.Port > 0 && p.Port <= 65535 &&
		dm8Username.MatchString(p.Username) && len(p.Password) > 0 && len(p.Password) <= 256 &&
		!strings.ContainsAny(p.Password, "\x00\r\n\t\"")
}

// NewDM8Probe runs on a Linux host with the site-provided DIsql client.
// The only operation exposed by the probe is reading fixed monitoring views.
func NewDM8Probe(disql, token, targets string) (http.Handler, error) {
	if !filepath.IsAbs(disql) || len(token) < 24 {
		return nil, ErrDM8Unconfigured
	}
	file, err := os.Stat(disql)
	if err != nil || !file.Mode().IsRegular() {
		return nil, ErrDM8Unconfigured
	}
	allowed := map[string]bool{}
	for _, raw := range strings.Split(targets, ",") {
		host, port, err := net.SplitHostPort(strings.TrimSpace(raw))
		n, numberErr := strconv.Atoi(port)
		if err != nil || numberErr != nil || n < 1 || n > 65535 || !(dm8Host.MatchString(host) || net.ParseIP(host) != nil) {
			return nil, ErrDM8Unconfigured
		}
		allowed[net.JoinHostPort(strings.ToLower(host), strconv.Itoa(n))] = true
	}
	slots := make(chan struct{}, 4)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/dm8/sample", func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var input dm8Request
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		var extra any
		if decoder.Decode(&input) != nil || decoder.Decode(&extra) != io.EOF || !input.valid() {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		if !allowed[net.JoinHostPort(strings.ToLower(input.Host), strconv.Itoa(input.Port))] {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		sample, err := readDM8(r.Context(), disql, input)
		if err != nil {
			if errors.Is(err, ErrDM8Connection) {
				w.WriteHeader(http.StatusUnprocessableEntity)
			} else {
				w.WriteHeader(http.StatusBadGateway)
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(sample)
	})
	return mux, nil
}

const dm8Queries = `SELECT 'DBS8|instance|' || SVR_VERSION || '|' || BUILD_VERSION || '|' || STATUS$ || '|' || MODE$ || '|' || TO_CHAR(START_TIME,'YYYY-MM-DD HH24:MI:SS') FROM V$INSTANCE;
SELECT 'DBS8|sessions|' || COUNT(*) || '|' || NVL(SUM(CASE WHEN STATE='ACTIVE' THEN 1 ELSE 0 END),0) FROM V$SESSIONS WHERE SESS_ID<>SESSID();
SELECT 'DBS8|transactions|' || COUNT(*) FROM V$TRX WHERE SESS_ID<>SESSID();
SELECT 'DBS8|wait|' || NVL(CAST(W.ID AS VARCHAR(24)),'none') || '|' || W.WAIT_FOR_ID || '|' || T.SESS_ID || '|' || B.SESS_ID || '|' || W.WAIT_TIME FROM DUAL LEFT JOIN (SELECT TOP 41 ID, WAIT_FOR_ID, WAIT_TIME FROM V$TRXWAIT ORDER BY WAIT_TIME DESC) W ON 1=1 LEFT JOIN V$TRX T ON T.ID=W.ID LEFT JOIN V$TRX B ON B.ID=W.WAIT_FOR_ID;
SELECT TOP 65 'DBS8|file|' || F.GROUP_ID || ':' || F.ID || '|' || REPLACE(TS.NAME,'|',' ') || '|' || CAST(F.TOTAL_SIZE AS BIGINT)*F.PAGE_SIZE || '|' || CAST(F.FREE_SIZE AS BIGINT)*F.PAGE_SIZE FROM V$DATAFILE F JOIN V$TABLESPACE TS ON TS.ID=F.GROUP_ID ORDER BY F.GROUP_ID,F.ID;
EXIT
`

func readDM8(ctx context.Context, disql string, input dm8Request) (DM8Metrics, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn := net.JoinHostPort(input.Host, strconv.Itoa(input.Port))
	commands := fmt.Sprintf("SET DEFINE OFF\nSET ECHO OFF\nSET HEADING OFF\nSET FEEDBACK OFF\nSET LINESHOW OFF\nSET TIMING OFF\nSET PAGESIZE 0\nSET LINESIZE 4096\nCONNECT %s/\"%s\"@%s\n", input.Username, input.Password, conn)
	cmd := exec.CommandContext(ctx, disql, "/nolog")
	cmd.Dir = filepath.Dir(disql)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "TZ=UTC", "DM_HOME=" + filepath.Dir(cmd.Dir), "LD_LIBRARY_PATH=" + cmd.Dir}
	cmd.Stdin = strings.NewReader(commands + dm8Queries)
	var output dm8Output
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Run(); err != nil {
		return DM8Metrics{}, ErrDM8Unavailable
	}
	return parseDM8(output.data.String())
}

type dm8Output struct{ data bytes.Buffer }

func (out *dm8Output) Write(p []byte) (int, error) {
	if out.data.Len()+len(p) > 128<<10 {
		return 0, ErrDM8Unavailable
	}
	return out.data.Write(p)
}

func parseDM8(output string) (DM8Metrics, error) {
	result := DM8Metrics{Waits: []DM8Wait{}, Files: []DM8File{}}
	var locks bool
	for line := range strings.SplitSeq(output, "\n") {
		_, fields, found := strings.Cut(line, "DBS8|")
		if !found {
			continue
		}
		tokens := strings.Split(strings.TrimSpace(fields), "|")
		for index := range tokens {
			tokens[index] = strings.TrimSpace(tokens[index])
		}
		switch tokens[0] {
		case "instance":
			if len(tokens) == 6 {
				result.Version, result.Build, result.Status, result.Mode, result.StartedAt = tokens[1], tokens[2], tokens[3], tokens[4], tokens[5]
			}
		case "sessions":
			if len(tokens) == 3 {
				result.Sessions, result.ActiveSessions = dm8Number(tokens[1]), dm8Number(tokens[2])
			}
		case "transactions":
			if len(tokens) == 2 {
				result.Transactions = dm8Number(tokens[1])
			}
		case "wait":
			if len(tokens) != 6 {
				continue
			}
			if tokens[1] == "none" {
				locks = true
			} else if millis := dm8Number(tokens[5]); dm8Number(tokens[1]) != nil && dm8Number(tokens[2]) != nil && millis != nil {
				locks = true
				if len(result.Waits) == 40 {
					result.WaitsTruncated = true
					continue
				}
				result.Waits = append(result.Waits, DM8Wait{tokens[1], tokens[2], tokens[3], tokens[4], *millis})
			}
		case "file":
			if len(tokens) != 5 {
				continue
			}
			total, free := dm8Number(tokens[3]), dm8Number(tokens[4])
			if total != nil && free != nil && *total >= *free && tokens[2] != "" {
				if len(result.Files) == 64 {
					result.FilesTruncated = true
					continue
				}
				result.Files = append(result.Files, DM8File{tokens[1], tokens[2], *total, *free})
			}
		}
	}
	if result.Version == "" {
		return DM8Metrics{}, ErrDM8Connection
	}
	for _, item := range []struct {
		key, source string
		ok          bool
	}{
		{"instance", "V$INSTANCE", true}, {"sessions", "V$SESSIONS", result.Sessions != nil && result.ActiveSessions != nil},
		{"transactions", "V$TRX", result.Transactions != nil}, {"lockWaits", "V$TRXWAIT / V$TRX", locks},
		{"datafiles", "V$DATAFILE / V$TABLESPACE", len(result.Files) > 0},
	} {
		signal := collectoradapter.Signal{Key: item.key, Source: item.source, Status: "ok"}
		if !item.ok {
			signal.Status, signal.Reason = "unavailable", "未取得有效结果，请核对视图权限和构建兼容性"
		}
		result.Signals = append(result.Signals, signal)
	}
	return result, nil
}

func dm8Number(value string) *int64 {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < 0 {
		return nil
	}
	return &n
}
