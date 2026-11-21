package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cuipengdba/dbsphere"
	"github.com/cuipengdba/dbsphere/internal/ai"
	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/cuipengdba/dbsphere/internal/collectorstate"
	"github.com/cuipengdba/dbsphere/internal/security"
	storepkg "github.com/cuipengdba/dbsphere/internal/store"
	"github.com/cuipengdba/dbsphere/internal/trace"
)

type HealthStore interface{ Health(context.Context) error }
type CollectionReader interface {
	ReadCollection(context.Context, string) (any, bool, error)
}
type ScopedCollectionReader interface {
	ReadScopedCollection(context.Context, string, []string) (any, bool, error)
}
type EventPager interface {
	EventPage(context.Context, storepkg.EventPageQuery, []string) (any, error)
}
type AssetPager interface {
	AssetPage(context.Context, storepkg.AssetPageQuery, []string) (any, error)
}
type EventOverviewReader interface {
	EventOverview(context.Context, []string) (any, error)
}
type MonitoringSeriesReader interface {
	MonitoringSeries(context.Context, []string, url.Values) (any, error)
}
type AssetStore interface {
	AssetByID(context.Context, string) (json.RawMessage, error)
	CreateAsset(context.Context, assets.Input, []byte) (json.RawMessage, error)
	UpdateAsset(context.Context, string, assets.Input, []byte) (json.RawMessage, error)
	DeleteAsset(context.Context, string) (bool, error)
}

var assetPath = regexp.MustCompile(`^/api/assets/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)
var assetCheckPath = regexp.MustCompile(`^/api/assets/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/check$`)
var rawSamplePath = regexp.MustCompile(`^/api/assets/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/samples/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)
var rawTracePath = regexp.MustCompile(`^/api/assets/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/traces/([0-9a-f]{32})$`)
var eventPath = regexp.MustCompile(`^/api/events/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)
var eventReportPath = regexp.MustCompile(`^/api/events/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/report$`)
var eventAuditPath = regexp.MustCompile(`^/api/events/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/audit$`)
var eventAIPath = regexp.MustCompile(`^/api/events/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/ai-summary$`)
var eventAIQuestionPath = regexp.MustCompile(`^/api/events/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/ai-question$`)
var eventWorkOrdersPath = regexp.MustCompile(`^/api/events/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/work-orders$`)
var workOrderDecisionPath = regexp.MustCompile(`^/api/work-orders/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/(approve|reject)$`)
var workOrderExecutePath = regexp.MustCompile(`^/api/work-orders/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/execute$`)
var eventAckPath = regexp.MustCompile(`^/api/events/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/ack$`)
var eventDecisionPath = regexp.MustCompile(`^/api/events/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/rca-decision$`)
var eventResponsePath = regexp.MustCompile(`^/api/events/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/response$`)
var assetSectionPath = regexp.MustCompile(`^/api/assets/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/(samples|sql|sql-window|slow-logs|sql-plans|series|capacity|replication|wait-history|monitoring-context)$`)
var slowLogImportPath = regexp.MustCompile(`^/api/assets/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/slow-logs/import$`)
var rawSQLPath = regexp.MustCompile(`^/api/assets/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/(slow-logs|sql-plans)/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)
var alertRulePath = regexp.MustCompile(`^/api/alert-rules/([a-z][a-z0-9_]{1,63})$`)
var inspectionTemplatePath = regexp.MustCompile(`^/api/inspection-templates/([a-z][a-z0-9_]{1,63})$`)
var inspectionRetryPath = regexp.MustCompile(`^/api/inspection-runs/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/retry$`)
var inspectionReportPath = regexp.MustCompile(`^/api/inspection-runs/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/report$`)
var silencePath = regexp.MustCompile(`^/api/silences/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)
var notificationRetryPath = regexp.MustCompile(`^/api/notifications/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/retry$`)

type AssetChecker interface {
	CheckAsset(context.Context, string, []byte) (storepkg.CheckResult, error)
}
type EventStore interface {
	EventByID(context.Context, string) (any, error)
	AcknowledgeEvent(context.Context, string) (any, error)
	RawSample(context.Context, string, string) (any, error)
	RawTrace(context.Context, string, string) (any, error)
	RecordRCADecision(context.Context, string, string, string, string) error
}
type EventReportReader interface {
	EventReport(context.Context, string, string) (string, error)
}
type EventResponseStore interface {
	RecordEventResponse(context.Context, string, string, string, string) error
}
type EventAIStore interface {
	EventAIEvidence(context.Context, string) (string, []string, error)
	ReserveAIInvocation(context.Context, string, string, int) (string, error)
	ReserveAIQuestionInvocation(context.Context, string, string, int) (string, error)
	CompleteAIInvocation(context.Context, string, string, string, []string) error
}
type WorkOrderStore interface {
	CreateWorkOrder(context.Context, string, string) (json.RawMessage, error)
	EventWorkOrders(context.Context, string) (any, error)
	DecideWorkOrder(context.Context, string, string) (json.RawMessage, error)
	BeginWorkOrder(context.Context, string) (string, string, error)
	FinishWorkOrder(context.Context, string, storepkg.CheckResult, error) (json.RawMessage, error)
}
type AdminAuditStore interface {
	StartAdminAudit(context.Context, string, string) (string, error)
	StartRoleAudit(context.Context, string, string, string) (string, error)
	FinishAdminAudit(context.Context, string, int) error
	RecentAdminAudit(context.Context) (any, error)
	RecentEventAdminAudit(context.Context, string) (any, error)
}

type auditResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *auditResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *auditResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(body)
}

type AssetSectionReader interface {
	ReadAssetSection(context.Context, string, string, url.Values) (any, error)
	RawSQLEvidence(context.Context, string, string, string) (any, error)
}
type SlowLogImporter interface {
	ImportSlowLogs(context.Context, string, string, string) (any, error)
}
type PlanImporter interface {
	ImportSQLPlan(context.Context, string, string, string, json.RawMessage) (any, error)
}
type AlertRuleStore interface {
	UpdateAlertRule(context.Context, string, storepkg.AlertRulePatch) (any, error)
}
type InspectionConfigStore interface {
	UpdateInspectionTemplate(context.Context, string, storepkg.InspectionTemplatePatch) (any, error)
	UpsertInspectionSchedule(context.Context, storepkg.InspectionScheduleInput) (any, error)
}
type InspectionRunner interface {
	RunInspection(context.Context, string, string, []byte, string) (any, error)
	RetryInspection(context.Context, string, []byte) (any, error)
}
type InspectionReportReader interface {
	InspectionReport(context.Context, string, string) (string, error)
}
type SilenceStore interface {
	CreateSilence(context.Context, storepkg.SilenceInput) (json.RawMessage, error)
	RevokeSilence(context.Context, string) (json.RawMessage, error)
}
type NotificationRetryStore interface {
	RetryNotification(context.Context, string) (bool, error)
}
type TraceStore interface {
	TraceAssets(context.Context, []byte) ([]trace.Asset, error)
	SaveTraceSpans(context.Context, []trace.Span) (int, error)
}

type TraceBatchStore interface {
	SaveTraceBatch(context.Context, []trace.Span, []trace.Span) (int, int, error)
}

type AuthConfig struct {
	MasterKey            []byte
	PasswordHash         string
	ViewerPasswordHash   string
	ApproverPasswordHash string
	ViewerAssetIDs       []string
	AutoCollect          bool
	CollectEvery         int
	CollectorState       *collectorstate.Tracker
	SecureCookie         bool
	OTLPToken            string
	AI                   *ai.Client
	AIDailyLimit         int
}

type loginAttempt struct {
	count int
	until time.Time
}

func New(store HealthStore, auth ...AuthConfig) http.Handler {
	var credentials *AuthConfig
	if len(auth) > 0 {
		credentials = &auth[0]
		if credentials.AIDailyLimit == 0 {
			credentials.AIDailyLimit = 20
		}
	}
	var attemptsMu sync.Mutex
	attempts := map[string]loginAttempt{}
	var aiAttemptsMu sync.Mutex
	aiAttempts := map[string]time.Time{}
	static, err := fs.Sub(dbsphere.Assets, "public")
	if err != nil {
		panic(err)
	}
	files := http.FileServerFS(static)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		respond(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := store.Health(r.Context()); err != nil {
			respond(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
			return
		}
		respond(w, http.StatusOK, map[string]string{"status": "ready", "storage": "postgresql18"})
	})
	mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, _ *http.Request) {
		interval := 60
		if credentials != nil && credentials.CollectEvery > 0 {
			interval = credentials.CollectEvery
		}
		respond(w, http.StatusOK, map[string]any{"backend": "go", "version": dbsphere.Version,
			"collectorEnabled":          credentials != nil && credentials.AutoCollect,
			"collectionIntervalSeconds": interval, "serverTime": time.Now().UTC()})
	})
	mux.HandleFunc("POST /v1/traces", func(w http.ResponseWriter, r *http.Request) {
		ingressAt := time.Now().UTC()
		if credentials == nil || credentials.OTLPToken == "" {
			respond(w, http.StatusNotFound, map[string]string{"error": "Trace intake disabled"})
			return
		}
		authorization := r.Header.Get("Authorization")
		received := strings.TrimPrefix(authorization, "Bearer ")
		if !strings.HasPrefix(authorization, "Bearer ") ||
			len(received) != len(credentials.OTLPToken) ||
			subtle.ConstantTimeCompare([]byte(received), []byte(credentials.OTLPToken)) != 1 {
			respond(w, http.StatusUnauthorized, map[string]string{"error": "Invalid trace token"})
			return
		}
		mediaType, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mediaErr != nil || mediaType != "application/json" {
			respond(w, http.StatusUnsupportedMediaType, map[string]string{"error": "OTLP JSON required"})
			return
		}
		tracesDB, ok := store.(TraceStore)
		if !ok {
			respond(w, http.StatusNotImplemented, map[string]string{"error": "Trace storage unavailable"})
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			respond(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "Trace payload exceeds 1 MiB"})
			return
		}
		assets, err := tracesDB.TraceAssets(r.Context(), credentials.MasterKey)
		if err != nil {
			respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Trace asset mapping unavailable"})
			return
		}
		parsed, err := trace.Decode(body, assets, ingressAt)
		switch {
		case errors.Is(err, trace.ErrTooMany):
			respond(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "Too many spans or scopes"})
			return
		case err != nil:
			respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid OTLP resourceSpans"})
			return
		}
		linkedCandidates := 0
		if batch, ok := tracesDB.(TraceBatchStore); ok {
			_, linkedCandidates, err = batch.SaveTraceBatch(r.Context(), parsed.Spans, parsed.Candidates)
		} else {
			_, err = tracesDB.SaveTraceSpans(r.Context(), parsed.Spans)
		}
		if err != nil {
			respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Trace storage unavailable"})
			return
		}
		rejected := parsed.Rejected - linkedCandidates
		if rejected > 0 {
			respond(w, http.StatusOK, map[string]any{"partialSuccess": map[string]any{
				"rejectedSpans": rejected,
				"errorMessage":  "Invalid or unmapped PostgreSQL spans were discarded"}})
		} else {
			respond(w, http.StatusOK, map[string]any{})
		}
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		if credentials == nil {
			respond(w, http.StatusNotImplemented, map[string]string{"error": "Go API migration in progress"})
			return
		}
		if r.Method != http.MethodGet && r.Header.Get("Origin") != "" {
			origin, err := url.Parse(r.Header.Get("Origin"))
			if err != nil || origin.Host != r.Host || (origin.Scheme != "http" && origin.Scheme != "https") {
				respond(w, http.StatusForbidden, map[string]string{"error": "Origin mismatch"})
				return
			}
		}
		cookie, _ := r.Cookie("dbsphere_session")
		token := ""
		if cookie != nil {
			token = cookie.Value
		}
		adminKey := security.AdminSessionKey(credentials.MasterKey, credentials.PasswordHash)
		role, loggedIn := security.SessionRole(adminKey, token, time.Now())
		if !loggedIn && credentials.ViewerPasswordHash != "" {
			viewerKey := security.ViewerSessionKey(credentials.MasterKey, credentials.ViewerPasswordHash)
			role, loggedIn = security.SessionRole(viewerKey, token, time.Now())
			loggedIn = loggedIn && role == "viewer"
		}
		if !loggedIn && credentials.ApproverPasswordHash != "" {
			approverKey := security.ApproverSessionKey(credentials.MasterKey, credentials.ApproverPasswordHash)
			role, loggedIn = security.SessionRole(approverKey, token, time.Now())
			loggedIn = loggedIn && role == "approver"
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/login" {
			ip := r.RemoteAddr
			if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
				ip = host
			}
			attemptsMu.Lock()
			attempt := attempts[ip]
			if time.Now().Before(attempt.until) {
				attemptsMu.Unlock()
				respond(w, http.StatusTooManyRequests, map[string]string{"error": "Too many attempts"})
				return
			}
			attemptsMu.Unlock()
			var input struct {
				Password string `json:"password"`
			}
			if err := readJSON(w, r, &input); err != nil {
				respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid login request"})
				return
			}
			loginRole := "admin"
			if !security.VerifyPassword(input.Password, credentials.PasswordHash) {
				loginRole = ""
				if credentials.ViewerPasswordHash != "" && security.VerifyPassword(input.Password, credentials.ViewerPasswordHash) {
					loginRole = "viewer"
				} else if credentials.ApproverPasswordHash != "" &&
					security.VerifyPassword(input.Password, credentials.ApproverPasswordHash) {
					loginRole = "approver"
				}
			}
			if loginRole == "" {
				attemptsMu.Lock()
				attempt = attempts[ip]
				attempt.count++
				if attempt.count >= 5 {
					attempt.count = 0
					attempt.until = time.Now().Add(5 * time.Minute)
				}
				attempts[ip] = attempt
				attemptsMu.Unlock()
				respond(w, http.StatusUnauthorized, map[string]string{"error": "Invalid credentials"})
				return
			}
			attemptsMu.Lock()
			delete(attempts, ip)
			attemptsMu.Unlock()
			var session string
			var err error
			if loginRole == "viewer" {
				viewerKey := security.ViewerSessionKey(credentials.MasterKey, credentials.ViewerPasswordHash)
				session, err = security.CreateViewerSession(viewerKey, time.Now())
			} else if loginRole == "approver" {
				approverKey := security.ApproverSessionKey(credentials.MasterKey, credentials.ApproverPasswordHash)
				session, err = security.CreateApproverSession(approverKey, time.Now())
			} else {
				session, err = security.CreateSession(adminKey, time.Now())
			}
			if err != nil {
				respond(w, http.StatusInternalServerError, map[string]string{"error": "Session unavailable"})
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "dbsphere_session", Value: session, Path: "/", MaxAge: 43200,
				HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: credentials.SecureCookie})
			respond(w, http.StatusOK, map[string]any{"authenticated": true, "role": loginRole,
				"viewerScoped":    loginRole == "viewer" && len(credentials.ViewerAssetIDs) > 0,
				"approvalEnabled": credentials.ApproverPasswordHash != "",
				"csrf":            security.CSRFToken(credentials.MasterKey, session)})
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/session" {
			var csrf any
			if loggedIn {
				csrf = security.CSRFToken(credentials.MasterKey, token)
			}
			var sessionRole any
			if loggedIn {
				sessionRole = role
			}
			respond(w, http.StatusOK, map[string]any{"authenticated": loggedIn, "role": sessionRole,
				"viewerScoped":    loggedIn && role == "viewer" && len(credentials.ViewerAssetIDs) > 0,
				"approvalEnabled": credentials.ApproverPasswordHash != "",
				"csrf":            csrf})
			return
		}
		if !loggedIn {
			respond(w, http.StatusUnauthorized, map[string]string{"error": "Authentication required"})
			return
		}
		if r.Method != http.MethodGet && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")),
			[]byte(security.CSRFToken(credentials.MasterKey, token))) != 1 {
			respond(w, http.StatusForbidden, map[string]string{"error": "CSRF token required"})
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/logout" {
			http.SetCookie(w, &http.Cookie{Name: "dbsphere_session", Path: "/", MaxAge: -1,
				HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: credentials.SecureCookie})
			respond(w, http.StatusOK, map[string]bool{"ok": true})
			return
		}
		if role == "viewer" && r.Method != http.MethodGet {
			respond(w, http.StatusForbidden, map[string]string{"error": "Read-only account cannot change monitoring data"})
			return
		}
		if role == "approver" && r.Method != http.MethodGet &&
			!(r.Method == http.MethodPost && workOrderDecisionPath.MatchString(r.URL.Path)) {
			respond(w, http.StatusForbidden, map[string]string{"error": "Approver account can only review work orders"})
			return
		}
		viewerScope := map[string]bool(nil)
		if role == "viewer" && len(credentials.ViewerAssetIDs) > 0 {
			viewerScope = make(map[string]bool, len(credentials.ViewerAssetIDs))
			for _, id := range credentials.ViewerAssetIDs {
				viewerScope[id] = true
			}
			if !authorizeScopedViewerRead(w, r, store, viewerScope) {
				return
			}
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/admin/audit" {
			if role != "admin" {
				respond(w, http.StatusForbidden, map[string]string{"error": "Administrator access required"})
				return
			}
			auditor, ok := store.(AdminAuditStore)
			if !ok {
				respond(w, http.StatusNotImplemented, map[string]string{"error": "Audit unavailable"})
				return
			}
			entries, err := auditor.RecentAdminAudit(r.Context())
			if err != nil {
				respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Audit unavailable"})
				return
			}
			respond(w, http.StatusOK, entries)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/admin/collector" {
			if role != "admin" {
				respond(w, http.StatusForbidden, map[string]string{"error": "Administrator access required"})
				return
			}
			if credentials.CollectorState == nil {
				respond(w, http.StatusNotImplemented, map[string]string{"error": "Collector status unavailable"})
				return
			}
			respond(w, http.StatusOK, credentials.CollectorState.Snapshot(time.Now()))
			return
		}
		if r.Method == http.MethodGet {
			if match := eventAuditPath.FindStringSubmatch(r.URL.Path); match != nil {
				if role != "admin" {
					respond(w, http.StatusForbidden, map[string]string{"error": "Administrator access required"})
					return
				}
				auditor, ok := store.(AdminAuditStore)
				if !ok {
					respond(w, http.StatusNotImplemented, map[string]string{"error": "Audit unavailable"})
					return
				}
				entries, err := auditor.RecentEventAdminAudit(r.Context(), match[1])
				if err != nil {
					respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Audit unavailable"})
					return
				}
				respond(w, http.StatusOK, entries)
				return
			}
		}
		if (role == "admin" || role == "approver") && (r.Method == http.MethodPost || r.Method == http.MethodPut ||
			r.Method == http.MethodPatch || r.Method == http.MethodDelete) {
			if auditor, ok := store.(AdminAuditStore); ok {
				id, err := auditor.StartRoleAudit(r.Context(), role, r.Method, r.URL.Path)
				if err != nil {
					respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Audit unavailable; operation was not run"})
					return
				}
				tracked := &auditResponseWriter{ResponseWriter: w}
				w = tracked
				defer func() {
					status := tracked.status
					if status == 0 {
						status = http.StatusOK
					}
					auditCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if err := auditor.FinishAdminAudit(auditCtx, id, status); err != nil {
						slog.Error("administrator audit completion failed", "auditId", id, "error", err)
					}
				}()
			}
		}
		if match := eventWorkOrdersPath.FindStringSubmatch(r.URL.Path); match != nil &&
			(r.Method == http.MethodGet || r.Method == http.MethodPost) {
			if role != "admin" && role != "approver" {
				respond(w, http.StatusForbidden, map[string]string{"error": "Work order access required"})
				return
			}
			orders, ok := store.(WorkOrderStore)
			if !ok {
				respond(w, http.StatusNotImplemented, map[string]string{"error": "Work orders unavailable"})
				return
			}
			if r.Method == http.MethodGet {
				value, err := orders.EventWorkOrders(r.Context(), match[1])
				if err != nil {
					respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Work orders unavailable"})
					return
				}
				respond(w, http.StatusOK, value)
				return
			}
			if role != "admin" || credentials.ApproverPasswordHash == "" {
				respond(w, http.StatusForbidden, map[string]string{"error": "A separate approver account is required"})
				return
			}
			var input struct {
				Reason string `json:"reason"`
			}
			if readJSONLimit(w, r, &input, 2048) != nil {
				respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid work order request"})
				return
			}
			order, err := orders.CreateWorkOrder(r.Context(), match[1], input.Reason)
			switch {
			case errors.Is(err, storepkg.ErrInvalidWorkOrder):
				respond(w, http.StatusBadRequest, map[string]string{"error": "Reason must be 5–300 characters"})
			case errors.Is(err, storepkg.ErrEventNotFound):
				respond(w, http.StatusNotFound, map[string]string{"error": "Event not found"})
			case errors.Is(err, storepkg.ErrWorkOrderConflict):
				respond(w, http.StatusConflict, map[string]string{"error": "Event is closed or has too many active work orders"})
			case err != nil:
				respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Work order storage unavailable"})
			default:
				respond(w, http.StatusCreated, map[string]any{"order": order,
					"preview": "审批后仅对该事件的资产运行一次只读采样；不执行 SQL 变更或 Kubernetes 操作。"})
			}
			return
		}
		if match := workOrderDecisionPath.FindStringSubmatch(r.URL.Path); match != nil && r.Method == http.MethodPost {
			if role != "approver" {
				respond(w, http.StatusForbidden, map[string]string{"error": "Separate approver required"})
				return
			}
			orders, ok := store.(WorkOrderStore)
			if !ok {
				respond(w, http.StatusNotImplemented, map[string]string{"error": "Work orders unavailable"})
				return
			}
			decision := "approved"
			if match[2] == "reject" {
				decision = "rejected"
			}
			order, err := orders.DecideWorkOrder(r.Context(), match[1], decision)
			if errors.Is(err, storepkg.ErrWorkOrderConflict) {
				respond(w, http.StatusConflict, map[string]string{"error": "Work order expired or already decided"})
				return
			}
			if err != nil {
				respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Work order decision unavailable"})
				return
			}
			respond(w, http.StatusOK, map[string]any{"order": order})
			return
		}
		if match := workOrderExecutePath.FindStringSubmatch(r.URL.Path); match != nil && r.Method == http.MethodPost {
			if role != "admin" {
				respond(w, http.StatusForbidden, map[string]string{"error": "Administrator execution required"})
				return
			}
			orders, ok := store.(WorkOrderStore)
			checker, checkOK := store.(AssetChecker)
			if !ok || !checkOK {
				respond(w, http.StatusNotImplemented, map[string]string{"error": "Controlled collection unavailable"})
				return
			}
			_, assetID, err := orders.BeginWorkOrder(r.Context(), match[1])
			if errors.Is(err, storepkg.ErrWorkOrderConflict) {
				respond(w, http.StatusConflict, map[string]string{"error": "Approval required, event resolved, or work order expired"})
				return
			}
			if err != nil {
				respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Work order unavailable"})
				return
			}
			collectionCtx, cancel := context.WithTimeout(r.Context(), 35*time.Second)
			result, collectionErr := checker.CheckAsset(collectionCtx, assetID, credentials.MasterKey)
			cancel()
			auditCtx, auditCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer auditCancel()
			order, err := orders.FinishWorkOrder(auditCtx, match[1], result, collectionErr)
			if err != nil {
				respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Work order execution audit unavailable"})
				return
			}
			respond(w, http.StatusOK, map[string]any{"order": order, "result": result})
			return
		}
		if r.Method == http.MethodPost {
			if match := slowLogImportPath.FindStringSubmatch(r.URL.Path); match != nil {
				importer, ok := store.(SlowLogImporter)
				if !ok {
					respond(w, http.StatusNotImplemented, map[string]string{"error": "Slow log import unavailable"})
					return
				}
				var input struct {
					Format  string `json:"format"`
					Content string `json:"content"`
				}
				if readJSONLimit(w, r, &input, 128<<10) != nil {
					respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid slow log payload"})
					return
				}
				value, err := importer.ImportSlowLogs(r.Context(), match[1], input.Format, input.Content)
				switch {
				case errors.Is(err, storepkg.ErrAssetNotFound):
					respond(w, http.StatusNotFound, map[string]string{"error": "Asset not found"})
				case errors.Is(err, storepkg.ErrInvalidSlowLog):
					respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid slow log import"})
				case err != nil:
					respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Slow log storage unavailable"})
				default:
					respond(w, http.StatusOK, value)
				}
				return
			}
			if match := assetSectionPath.FindStringSubmatch(r.URL.Path); match != nil && match[2] == "sql-plans" {
				importer, ok := store.(PlanImporter)
				if !ok {
					respond(w, http.StatusNotImplemented, map[string]string{"error": "SQL plan import unavailable"})
					return
				}
				var input struct {
					QueryID    string          `json:"queryId"`
					ObservedAt string          `json:"observedAt"`
					Plan       json.RawMessage `json:"plan"`
				}
				if readJSONLimit(w, r, &input, 128<<10) != nil {
					respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid SQL plan payload"})
					return
				}
				value, err := importer.ImportSQLPlan(r.Context(), match[1], input.QueryID, input.ObservedAt, input.Plan)
				switch {
				case errors.Is(err, storepkg.ErrAssetNotFound):
					respond(w, http.StatusNotFound, map[string]string{"error": "Asset not found"})
				case errors.Is(err, storepkg.ErrInvalidSQLPlan):
					respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid SQL plan"})
				case err != nil:
					respond(w, http.StatusServiceUnavailable, map[string]string{"error": "SQL plan storage unavailable"})
				default:
					respond(w, http.StatusCreated, map[string]any{"plan": value})
				}
				return
			}
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/ai/status" {
			if credentials.AI == nil {
				respond(w, http.StatusOK, map[string]any{"enabled": false})
			} else {
				respond(w, http.StatusOK, map[string]any{"enabled": true, "model": credentials.AI.Model(),
					"dailyLimit": credentials.AIDailyLimit})
			}
			return
		}
		if r.Method == http.MethodPost {
			match := eventAIPath.FindStringSubmatch(r.URL.Path)
			questionMode := false
			if match == nil {
				match = eventAIQuestionPath.FindStringSubmatch(r.URL.Path)
				questionMode = match != nil
			}
			if match != nil {
				if credentials.AI == nil {
					respond(w, http.StatusNotImplemented, map[string]string{"error": "AI model is not configured"})
					return
				}
				aiStore, ok := store.(EventAIStore)
				if !ok {
					respond(w, http.StatusNotImplemented, map[string]string{"error": "AI evidence unavailable"})
					return
				}
				var question string
				if questionMode {
					var input struct {
						Question string `json:"question"`
					}
					if readJSONLimit(w, r, &input, 2048) != nil ||
						len([]rune(strings.TrimSpace(input.Question))) < 5 ||
						len([]rune(input.Question)) > 400 || strings.ContainsAny(input.Question, "\x00\r") {
						respond(w, http.StatusBadRequest, map[string]string{"error": "Question must be 5–400 characters"})
						return
					}
					question = input.Question
				}
				prompt, ids, err := aiStore.EventAIEvidence(r.Context(), match[1])
				if errors.Is(err, storepkg.ErrEventNotFound) {
					respond(w, http.StatusNotFound, map[string]string{"error": "Event not found"})
					return
				}
				if err != nil {
					respond(w, http.StatusServiceUnavailable, map[string]string{"error": "AI evidence unavailable"})
					return
				}
				aiAttemptsMu.Lock()
				last := aiAttempts[match[1]]
				if time.Since(last) < 30*time.Second {
					aiAttemptsMu.Unlock()
					respond(w, http.StatusTooManyRequests, map[string]string{"error": "Please wait before requesting another AI summary"})
					return
				}
				aiAttempts[match[1]] = time.Now()
				aiAttemptsMu.Unlock()
				var invocationID string
				if questionMode {
					invocationID, err = aiStore.ReserveAIQuestionInvocation(r.Context(), match[1],
						credentials.AI.Model(), credentials.AIDailyLimit)
				} else {
					invocationID, err = aiStore.ReserveAIInvocation(r.Context(), match[1],
						credentials.AI.Model(), credentials.AIDailyLimit)
				}
				if errors.Is(err, storepkg.ErrAIBudgetExceeded) {
					respond(w, http.StatusTooManyRequests, map[string]string{"error": "Daily AI analysis limit reached; rule-based diagnosis remains available"})
					return
				}
				if err != nil {
					respond(w, http.StatusServiceUnavailable, map[string]string{"error": "AI budget audit unavailable"})
					return
				}
				var result ai.Result
				var modelErr error
				if questionMode {
					result, modelErr = credentials.AI.Answer(r.Context(), prompt, question, ids)
				} else {
					result, modelErr = credentials.AI.Analyze(r.Context(), prompt, ids)
				}
				status := "success"
				cited := result.EvidenceIDs
				if modelErr != nil {
					status, cited = "failed", nil
				}
				auditCtx, auditCancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer auditCancel()
				if err := aiStore.CompleteAIInvocation(auditCtx, match[1], invocationID, status, cited); err != nil {
					respond(w, http.StatusServiceUnavailable, map[string]string{"error": "AI audit unavailable"})
					return
				}
				if modelErr != nil {
					respond(w, http.StatusServiceUnavailable, map[string]string{"error": "AI analysis unavailable; rule-based diagnosis remains available"})
					return
				}
				respond(w, http.StatusOK, map[string]any{"result": result})
				return
			}
		}
		if r.Method == http.MethodPatch {
			if match := inspectionTemplatePath.FindStringSubmatch(r.URL.Path); match != nil {
				configDB, ok := store.(InspectionConfigStore)
				if !ok {
					respond(w, http.StatusNotImplemented, map[string]string{"error": "Inspection configuration unavailable"})
					return
				}
				var patch storepkg.InspectionTemplatePatch
				if readJSON(w, r, &patch) != nil {
					respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid inspection thresholds"})
					return
				}
				value, err := configDB.UpdateInspectionTemplate(r.Context(), match[1], patch)
				switch {
				case errors.Is(err, storepkg.ErrInspectionTemplateNotFound):
					respond(w, http.StatusNotFound, map[string]string{"error": "Inspection template not found"})
				case errors.Is(err, storepkg.ErrInvalidInspection):
					respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid inspection thresholds"})
				case err != nil:
					respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Inspection template storage unavailable"})
				default:
					respond(w, http.StatusOK, value)
				}
				return
			}
			if match := alertRulePath.FindStringSubmatch(r.URL.Path); match != nil {
				rulesDB, ok := store.(AlertRuleStore)
				if !ok {
					respond(w, http.StatusNotImplemented, map[string]string{"error": "Alert rule updates unavailable"})
					return
				}
				var patch storepkg.AlertRulePatch
				if readJSON(w, r, &patch) != nil {
					respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid alert rule update"})
					return
				}
				value, err := rulesDB.UpdateAlertRule(r.Context(), match[1], patch)
				switch {
				case errors.Is(err, storepkg.ErrAlertRuleNotFound):
					respond(w, http.StatusNotFound, map[string]string{"error": "Alert rule not found"})
				case errors.Is(err, storepkg.ErrAlertRuleUnsupported):
					respond(w, http.StatusNotImplemented, map[string]string{"error": "This alert rule is not yet supported by Go"})
				case errors.Is(err, storepkg.ErrInvalidAlertRule):
					respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid alert rule update"})
				case err != nil:
					respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Alert rule storage unavailable"})
				default:
					respond(w, http.StatusOK, value)
				}
				return
			}
		}
		if r.Method == http.MethodPut && r.URL.Path == "/api/inspection-schedules" {
			configDB, ok := store.(InspectionConfigStore)
			if !ok {
				respond(w, http.StatusNotImplemented, map[string]string{"error": "Inspection scheduling unavailable"})
				return
			}
			var input storepkg.InspectionScheduleInput
			if readJSON(w, r, &input) != nil ||
				!assetPath.MatchString("/api/assets/"+input.AssetID) ||
				!inspectionTemplatePath.MatchString("/api/inspection-templates/"+input.TemplateID) {
				respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid inspection schedule"})
				return
			}
			value, err := configDB.UpsertInspectionSchedule(r.Context(), input)
			switch {
			case errors.Is(err, storepkg.ErrInspectionScheduleReference):
				respond(w, http.StatusNotFound, map[string]string{"error": "Inspection asset or template not found"})
			case errors.Is(err, storepkg.ErrInvalidInspection):
				respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid inspection schedule"})
			case err != nil:
				respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Inspection schedule storage unavailable"})
			default:
				respond(w, http.StatusOK, value)
			}
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/inspections/run" {
			runner, ok := store.(InspectionRunner)
			if !ok {
				respond(w, http.StatusNotImplemented, map[string]string{"error": "Inspection execution unavailable"})
				return
			}
			var input struct {
				AssetID    string `json:"assetId"`
				TemplateID string `json:"templateId"`
			}
			if readJSON(w, r, &input) != nil ||
				!assetPath.MatchString("/api/assets/"+input.AssetID) ||
				!inspectionTemplatePath.MatchString("/api/inspection-templates/"+input.TemplateID) {
				respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid inspection request"})
				return
			}
			value, err := runner.RunInspection(r.Context(), input.AssetID, input.TemplateID, credentials.MasterKey, "")
			switch {
			case errors.Is(err, storepkg.ErrInspectionScheduleReference):
				respond(w, http.StatusNotFound, map[string]string{"error": "Inspection asset or template not found"})
			case errors.Is(err, storepkg.ErrInvalidInspection):
				respond(w, http.StatusBadRequest, map[string]string{"error": "Inspection template does not match asset"})
			case err != nil:
				respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Inspection execution unavailable"})
			default:
				respond(w, http.StatusCreated, value)
			}
			return
		}
		if r.Method == http.MethodPost {
			if match := inspectionRetryPath.FindStringSubmatch(r.URL.Path); match != nil {
				runner, ok := store.(InspectionRunner)
				if !ok {
					respond(w, http.StatusNotImplemented, map[string]string{"error": "Inspection retry unavailable"})
					return
				}
				value, err := runner.RetryInspection(r.Context(), match[1], credentials.MasterKey)
				switch {
				case errors.Is(err, storepkg.ErrInspectionTemplateNotFound):
					respond(w, http.StatusNotFound, map[string]string{"error": "Inspection run not found"})
				case err != nil:
					respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Inspection retry unavailable"})
				default:
					respond(w, http.StatusCreated, value)
				}
				return
			}
		}
		if r.Method == http.MethodGet {
			if match := inspectionReportPath.FindStringSubmatch(r.URL.Path); match != nil {
				format := r.URL.Query().Get("format")
				if format == "" {
					format = "md"
				}
				if format != "md" && format != "html" {
					respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid report format"})
					return
				}
				reports, ok := store.(InspectionReportReader)
				if !ok {
					respond(w, http.StatusNotImplemented, map[string]string{"error": "Inspection reports unavailable"})
					return
				}
				body, err := reports.InspectionReport(r.Context(), match[1], format)
				if errors.Is(err, storepkg.ErrInspectionTemplateNotFound) {
					respond(w, http.StatusNotFound, map[string]string{"error": "Inspection run not found"})
					return
				}
				if err != nil {
					respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Inspection report unavailable"})
					return
				}
				contentType := "text/markdown; charset=utf-8"
				if format == "html" {
					contentType = "text/html; charset=utf-8"
				}
				w.Header().Set("Content-Type", contentType)
				w.Header().Set("Content-Disposition", "attachment; filename=\"dbsphere-inspection-"+match[1]+"."+format+"\"")
				w.Header().Set("Cache-Control", "no-store")
				_, _ = w.Write([]byte(body))
				return
			}
		}
		if silencesDB, ok := store.(SilenceStore); ok {
			if r.Method == http.MethodPost && r.URL.Path == "/api/silences" {
				var input storepkg.SilenceInput
				if readJSON(w, r, &input) != nil {
					respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid silence window"})
					return
				}
				value, err := silencesDB.CreateSilence(r.Context(), input)
				switch {
				case errors.Is(err, storepkg.ErrInvalidSilence):
					respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid silence window"})
				case errors.Is(err, storepkg.ErrSilenceReference):
					respond(w, http.StatusBadRequest, map[string]string{"error": "Silence asset or rule not found"})
				case err != nil:
					respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Silence storage unavailable"})
				default:
					respond(w, http.StatusCreated, map[string]any{"silence": value})
				}
				return
			}
			if r.Method == http.MethodDelete {
				if match := silencePath.FindStringSubmatch(r.URL.Path); match != nil {
					value, err := silencesDB.RevokeSilence(r.Context(), match[1])
					if errors.Is(err, storepkg.ErrSilenceNotFound) {
						respond(w, http.StatusNotFound, map[string]string{"error": "Silence not found or already revoked"})
					} else if err != nil {
						respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Silence storage unavailable"})
					} else {
						respond(w, http.StatusOK, map[string]any{"silence": value})
					}
					return
				}
			}
		}
		if r.Method == http.MethodPost {
			if match := notificationRetryPath.FindStringSubmatch(r.URL.Path); match != nil {
				retryDB, ok := store.(NotificationRetryStore)
				if !ok {
					respond(w, http.StatusNotImplemented, map[string]string{"error": "Notification retry unavailable"})
					return
				}
				retried, err := retryDB.RetryNotification(r.Context(), match[1])
				if err != nil {
					respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Notification storage unavailable"})
				} else if !retried {
					respond(w, http.StatusNotFound, map[string]string{"error": "Failed notification not found"})
				} else {
					respond(w, http.StatusOK, map[string]bool{"ok": true})
				}
				return
			}
		}
		if assetsDB, ok := store.(AssetStore); ok {
			if r.Method == http.MethodPost && r.URL.Path == "/api/assets" {
				var input assets.Input
				if readJSON(w, r, &input) != nil || input.Validate() != nil {
					respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid asset fields"})
					return
				}
				asset, err := assetsDB.CreateAsset(r.Context(), input, credentials.MasterKey)
				if err != nil {
					respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Asset storage unavailable"})
					return
				}
				respond(w, http.StatusCreated, map[string]any{"asset": asset})
				return
			}
			if match := assetPath.FindStringSubmatch(r.URL.Path); match != nil {
				id := match[1]
				switch r.Method {
				case http.MethodGet:
					asset, err := assetsDB.AssetByID(r.Context(), id)
					if err != nil {
						respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Asset storage unavailable"})
						return
					}
					if asset == nil {
						respond(w, http.StatusNotFound, map[string]string{"error": "Asset not found"})
						return
					}
					respond(w, http.StatusOK, map[string]any{"asset": asset})
					return
				case http.MethodPatch:
					var input assets.Input
					if readJSON(w, r, &input) != nil || input.Validate() != nil {
						respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid asset fields"})
						return
					}
					asset, err := assetsDB.UpdateAsset(r.Context(), id, input, credentials.MasterKey)
					if err != nil {
						respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Asset storage unavailable"})
						return
					}
					if asset == nil {
						respond(w, http.StatusNotFound, map[string]string{"error": "Asset not found"})
						return
					}
					respond(w, http.StatusOK, map[string]any{"asset": asset})
					return
				case http.MethodDelete:
					deleted, err := assetsDB.DeleteAsset(r.Context(), id)
					if err != nil {
						respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Asset storage unavailable"})
						return
					}
					if !deleted {
						respond(w, http.StatusNotFound, map[string]string{"error": "Asset not found"})
						return
					}
					respond(w, http.StatusOK, map[string]bool{"ok": true})
					return
				}
			}
		}
		if checker, ok := store.(AssetChecker); ok && r.Method == http.MethodPost {
			if match := assetCheckPath.FindStringSubmatch(r.URL.Path); match != nil {
				result, err := checker.CheckAsset(r.Context(), match[1], credentials.MasterKey)
				if err == storepkg.ErrAssetNotFound {
					respond(w, http.StatusNotFound, map[string]string{"error": "Asset not found"})
					return
				}
				if err == storepkg.ErrCollectionUnavailable {
					respond(w, http.StatusNotImplemented, map[string]string{"error": "Cross-layer Go collection is still in development"})
					return
				}
				if err != nil {
					respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Asset collection unavailable"})
					return
				}
				respond(w, http.StatusOK, map[string]any{"result": result})
				return
			}
		}
		if eventsDB, ok := store.(EventStore); ok {
			if r.Method == http.MethodGet {
				if match := eventReportPath.FindStringSubmatch(r.URL.Path); match != nil {
					format := r.URL.Query().Get("format")
					if format == "" {
						format = "md"
					}
					if format != "md" && format != "html" {
						respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid report format"})
						return
					}
					reports, ok := store.(EventReportReader)
					if !ok {
						respond(w, http.StatusNotImplemented, map[string]string{"error": "Event reports unavailable"})
						return
					}
					body, err := reports.EventReport(r.Context(), match[1], format)
					if errors.Is(err, storepkg.ErrEventNotFound) {
						respond(w, http.StatusNotFound, map[string]string{"error": "Event not found"})
						return
					}
					if err != nil {
						respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Event report unavailable"})
						return
					}
					contentType := "text/markdown; charset=utf-8"
					if format == "html" {
						contentType = "text/html; charset=utf-8"
					}
					w.Header().Set("Content-Type", contentType)
					w.Header().Set("Content-Disposition", "attachment; filename=\"dbsphere-event-"+match[1]+"."+format+"\"")
					w.Header().Set("Cache-Control", "no-store")
					_, _ = w.Write([]byte(body))
					return
				}
				if match := eventPath.FindStringSubmatch(r.URL.Path); match != nil {
					value, err := eventsDB.EventByID(r.Context(), match[1])
					if err != nil {
						respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Event data unavailable"})
						return
					}
					if value == nil {
						respond(w, http.StatusNotFound, map[string]string{"error": "Event not found"})
						return
					}
					respond(w, http.StatusOK, value)
					return
				}
				if match := rawSamplePath.FindStringSubmatch(r.URL.Path); match != nil {
					value, err := eventsDB.RawSample(r.Context(), match[1], match[2])
					if err != nil {
						respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Sample data unavailable"})
						return
					}
					if value == nil {
						respond(w, http.StatusNotFound, map[string]string{"error": "Sample not found"})
						return
					}
					respond(w, http.StatusOK, value)
					return
				}
				if match := rawTracePath.FindStringSubmatch(r.URL.Path); match != nil {
					value, err := eventsDB.RawTrace(r.Context(), match[1], match[2])
					if err != nil {
						respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Trace data unavailable"})
						return
					}
					if value == nil {
						respond(w, http.StatusNotFound, map[string]string{"error": "Trace not found"})
						return
					}
					respond(w, http.StatusOK, value)
					return
				}
			}
			if r.Method == http.MethodPost {
				if match := eventResponsePath.FindStringSubmatch(r.URL.Path); match != nil {
					writer, ok := store.(EventResponseStore)
					if !ok {
						respond(w, http.StatusNotImplemented, map[string]string{"error": "Event response unavailable"})
						return
					}
					var input struct {
						Phase    string `json:"phase"`
						Note     string `json:"note"`
						SampleID string `json:"sampleId"`
					}
					if readJSON(w, r, &input) != nil {
						respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid event response"})
						return
					}
					err := writer.RecordEventResponse(r.Context(), match[1], input.Phase, input.Note, input.SampleID)
					switch {
					case errors.Is(err, storepkg.ErrInvalidEventResponse):
						respond(w, http.StatusBadRequest, map[string]string{"error": "Valid phase and note are required"})
					case errors.Is(err, storepkg.ErrEventNotFound):
						respond(w, http.StatusNotFound, map[string]string{"error": "Event not found"})
					case errors.Is(err, storepkg.ErrRecoveryNotVerified):
						respond(w, http.StatusConflict, map[string]string{"error": "Recovery requires a successful sample after event resolution"})
					case err != nil:
						respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Event response storage unavailable"})
					default:
						respond(w, http.StatusOK, map[string]bool{"ok": true})
					}
					return
				}
				if match := eventDecisionPath.FindStringSubmatch(r.URL.Path); match != nil {
					var input struct {
						Code     string `json:"code"`
						Decision string `json:"decision"`
						Reason   string `json:"reason"`
					}
					if readJSON(w, r, &input) != nil {
						respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid RCA decision"})
						return
					}
					err := eventsDB.RecordRCADecision(r.Context(), match[1], input.Code, input.Decision, input.Reason)
					if errors.Is(err, storepkg.ErrInvalidRCADecision) {
						respond(w, http.StatusBadRequest, map[string]string{"error": "Valid candidate, decision and reason are required"})
						return
					}
					if errors.Is(err, storepkg.ErrEventNotFound) {
						respond(w, http.StatusNotFound, map[string]string{"error": "Event not found"})
						return
					}
					if err != nil {
						respond(w, http.StatusServiceUnavailable, map[string]string{"error": "RCA decision unavailable"})
						return
					}
					respond(w, http.StatusOK, map[string]bool{"ok": true})
					return
				}
				if match := eventAckPath.FindStringSubmatch(r.URL.Path); match != nil {
					value, err := eventsDB.AcknowledgeEvent(r.Context(), match[1])
					if err != nil {
						respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Event update unavailable"})
						return
					}
					if value == nil {
						respond(w, http.StatusNotFound, map[string]string{"error": "Event not found"})
						return
					}
					respond(w, http.StatusOK, value)
					return
				}
			}
		}
		if sections, ok := store.(AssetSectionReader); ok && r.Method == http.MethodGet {
			if match := rawSQLPath.FindStringSubmatch(r.URL.Path); match != nil {
				value, err := sections.RawSQLEvidence(r.Context(), match[1], match[2], match[3])
				if err != nil {
					respond(w, http.StatusServiceUnavailable, map[string]string{"error": "SQL evidence unavailable"})
					return
				}
				if value == nil {
					respond(w, http.StatusNotFound, map[string]string{"error": "SQL evidence not found"})
					return
				}
				respond(w, http.StatusOK, value)
				return
			}
			if match := assetSectionPath.FindStringSubmatch(r.URL.Path); match != nil {
				if assetsDB, ok := store.(AssetStore); ok {
					asset, err := assetsDB.AssetByID(r.Context(), match[1])
					if err != nil {
						respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Asset storage unavailable"})
						return
					}
					if asset == nil {
						respond(w, http.StatusNotFound, map[string]string{"error": "Asset not found"})
						return
					}
				}
				value, err := sections.ReadAssetSection(r.Context(), match[1], match[2], r.URL.Query())
				if err == storepkg.ErrInvalidRange {
					respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid monitoring time range"})
					return
				}
				if err != nil {
					respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Asset detail unavailable"})
					return
				}
				respond(w, http.StatusOK, value)
				return
			}
		}
		if r.Method == http.MethodGet {
			if r.URL.Path == "/api/monitoring/series" {
				readMonitoringSeries(w, r, store, viewerScope)
				return
			}
			if r.URL.Path == "/api/assets/page" {
				pager, ok := store.(AssetPager)
				if !ok {
					respond(w, http.StatusNotImplemented, map[string]string{"error": "Asset pagination unavailable"})
					return
				}
				options := storepkg.AssetPageQuery{Search: r.URL.Query().Get("q"),
					Runtime: r.URL.Query().Get("runtime"), Cursor: r.URL.Query().Get("cursor"), Limit: 50}
				if raw := r.URL.Query().Get("limit"); raw != "" {
					var err error
					options.Limit, err = strconv.Atoi(raw)
					if err != nil {
						respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid asset page query"})
						return
					}
				}
				var scopedIDs []string
				if viewerScope != nil {
					scopedIDs = credentials.ViewerAssetIDs
				}
				page, err := pager.AssetPage(r.Context(), options, scopedIDs)
				if errors.Is(err, storepkg.ErrInvalidAssetPage) {
					respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid asset page query"})
					return
				}
				if err != nil {
					respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Asset page unavailable"})
					return
				}
				respond(w, http.StatusOK, page)
				return
			}
			if r.URL.Path == "/api/events/overview" {
				reader, ok := store.(EventOverviewReader)
				if !ok {
					respond(w, http.StatusNotImplemented, map[string]string{"error": "Event overview unavailable"})
					return
				}
				var scopedIDs []string
				if viewerScope != nil {
					scopedIDs = credentials.ViewerAssetIDs
				}
				value, err := reader.EventOverview(r.Context(), scopedIDs)
				if err != nil {
					respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Event overview unavailable"})
					return
				}
				respond(w, http.StatusOK, value)
				return
			}
			if r.URL.Path == "/api/events/page" {
				pager, ok := store.(EventPager)
				if !ok {
					respond(w, http.StatusNotImplemented, map[string]string{"error": "Event pagination unavailable"})
					return
				}
				options := storepkg.EventPageQuery{Status: r.URL.Query().Get("status"),
					Search: r.URL.Query().Get("q"), Cursor: r.URL.Query().Get("cursor"), Limit: 50}
				if raw := r.URL.Query().Get("limit"); raw != "" {
					var err error
					options.Limit, err = strconv.Atoi(raw)
					if err != nil {
						respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid event page query"})
						return
					}
				}
				var scopedIDs []string
				if viewerScope != nil {
					scopedIDs = credentials.ViewerAssetIDs
				}
				page, err := pager.EventPage(r.Context(), options, scopedIDs)
				if errors.Is(err, storepkg.ErrInvalidEventPage) {
					respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid event page query"})
					return
				}
				if err != nil {
					respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Event page unavailable"})
					return
				}
				respond(w, http.StatusOK, page)
				return
			}
			if reader, ok := store.(CollectionReader); ok {
				var value any
				var found bool
				var err error
				if viewerScope != nil && scopedCollectionKeys[r.URL.Path] != "" {
					scopedReader, supported := store.(ScopedCollectionReader)
					if !supported {
						respond(w, http.StatusForbidden, map[string]string{"error": "Scoped collection unavailable"})
						return
					}
					value, found, err = scopedReader.ReadScopedCollection(r.Context(), r.URL.Path, credentials.ViewerAssetIDs)
				} else {
					value, found, err = reader.ReadCollection(r.Context(), r.URL.Path)
				}
				if found {
					if err != nil {
						respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Monitoring data unavailable"})
						return
					}
					if viewerScope != nil && scopedCollectionKeys[r.URL.Path] != "" {
						value, err = filterScopedCollection(r.URL.Path, value, viewerScope)
						if err != nil {
							respond(w, http.StatusForbidden, map[string]string{"error": "Scoped collection unavailable"})
							return
						}
					}
					respond(w, http.StatusOK, value)
					return
				}
			}
		}
		respond(w, http.StatusNotImplemented, map[string]string{"error": "Go API migration in progress"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/" && r.URL.Path != "/index.html" && r.URL.Path != "/app.js" &&
			r.URL.Path != "/observability.js" &&
			r.URL.Path != "/monitoring.js" && r.URL.Path != "/theme.js" &&
			r.URL.Path != "/styles.css" && r.URL.Path != "/freshness.css" && r.URL.Path != "/console.css" && r.URL.Path != "/favicon.svg" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		files.ServeHTTP(w, r)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'")
		mux.ServeHTTP(w, r)
	})
}

func readJSON(w http.ResponseWriter, r *http.Request, value any) error {
	return readJSONLimit(w, r, value, 65536)
}

func readJSONLimit(w http.ResponseWriter, r *http.Request, value any, limit int64) error {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return io.ErrUnexpectedEOF
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
