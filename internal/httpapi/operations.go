package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

type OperationsConfig struct {
	Token        string
	Organization string
	MetricsToken string
}

var opsUUID = regexp.MustCompile("^" + accessUUID + "$")

func LoadOperationsConfig() (OperationsConfig, error) {
	cfg := OperationsConfig{
		Token:        os.Getenv("DBSPHERE_OPS_TOKEN"),
		Organization: os.Getenv("DBSPHERE_OPS_ORGANIZATION"),
		MetricsToken: os.Getenv("DBSPHERE_METRICS_TOKEN"),
	}
	if cfg.Organization == "" {
		cfg.Organization = storepkg.DefaultOrganizationID
	}
	if !opsUUID.MatchString(cfg.Organization) {
		return cfg, errors.New("invalid DBSPHERE_OPS_ORGANIZATION")
	}
	for _, item := range []struct{ name, value string }{
		{"DBSPHERE_OPS_TOKEN", cfg.Token}, {"DBSPHERE_METRICS_TOKEN", cfg.MetricsToken},
	} {
		if item.value != "" && (len(item.value) < 24 || len(item.value) > 256 || strings.ContainsAny(item.value, " \t\r\n")) {
			return cfg, fmt.Errorf("%s must be 24 to 256 characters without whitespace", item.name)
		}
	}
	if cfg.Token != "" && cfg.MetricsToken == cfg.Token {
		return cfg, errors.New("operations and Prometheus credentials must be different")
	}
	return cfg, nil
}

type operationsStore interface {
	AssetPager
	EventPager
	OperationsAsset(context.Context, string, string) (json.RawMessage, error)
	OperationsEvent(context.Context, string, string) (json.RawMessage, error)
	RecordExternalIncident(context.Context, string, storepkg.ExternalIncidentInput) (storepkg.ExternalIncidentResult, error)
}

type ticketReceiptStore interface {
	RecordTicketReceipt(context.Context, string, storepkg.TicketReceipt) (storepkg.ExternalIncidentResult, error)
}

func requireOperationsToken(w http.ResponseWriter, r *http.Request, secret string) bool {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if ok && secret != "" && subtle.ConstantTimeCompare([]byte(token), []byte(secret)) == 1 {
		return true
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="DbSphere"`)
	respond(w, http.StatusUnauthorized, map[string]string{"error": "Invalid integration token"})
	return false
}

func registerOperations(mux *http.ServeMux, db HealthStore, auth *AuthConfig) {
	cfg := auth.Operations
	ops, _ := db.(operationsStore)
	if cfg.Token != "" {
		mux.HandleFunc("POST /api/v1/change-receipts", func(w http.ResponseWriter, r *http.Request) {
			if requireOperationsToken(w, r, cfg.Token) {
				handleChangeReceipt(w, r, db, cfg.Organization)
			}
		})
		for _, route := range []string{"GET /api/v1/assets", "GET /api/v1/assets/{asset}", "GET /api/v1/events",
			"GET /api/v1/events/{event}", "POST /api/v1/external-incidents", "POST /api/v1/ticket-receipts"} {
			mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) {
				if !requireOperationsToken(w, r, cfg.Token) {
					return
				}
				if ops == nil {
					respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Operations storage unavailable"})
					return
				}
				serveOperations(w, r, ops, cfg.Organization)
			})
		}
	}
	if cfg.MetricsToken != "" {
		mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
			if requireOperationsToken(w, r, cfg.MetricsToken) {
				servePlatformMetrics(w, r, db, auth)
			}
		})
	}
}

func serveOperations(w http.ResponseWriter, r *http.Request, db operationsStore, organization string) {
	var value any
	var err error
	status := http.StatusOK
	ctx := r.Context()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/ticket-receipts":
		receipts, ok := db.(ticketReceiptStore)
		if !ok {
			respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Ticket receipt storage unavailable"})
			return
		}
		var input storepkg.TicketReceipt
		if readJSONLimit(w, r, &input, 8192) != nil {
			err = storepkg.ErrExternalInput
			break
		}
		result, writeErr := receipts.RecordTicketReceipt(ctx, organization, input)
		value, err = result, writeErr
		if err == nil && !result.Duplicate {
			status = http.StatusCreated
		}
	case r.Method == http.MethodPost:
		var input storepkg.ExternalIncidentInput
		if err := readJSONLimit(w, r, &input, 8192); err != nil {
			respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid external incident JSON"})
			return
		}
		result, writeErr := db.RecordExternalIncident(ctx, organization, input)
		value, err = result, writeErr
		if err == nil && !result.Duplicate {
			status = http.StatusCreated
		}
	case r.PathValue("asset") != "":
		id := r.PathValue("asset")
		if !opsUUID.MatchString(id) {
			err = storepkg.ErrExternalMissing
			break
		}
		var asset json.RawMessage
		asset, err = db.OperationsAsset(ctx, organization, id)
		if err == nil && asset == nil {
			err = storepkg.ErrExternalMissing
		}
		value = asset
	case r.PathValue("event") != "":
		id := r.PathValue("event")
		if !opsUUID.MatchString(id) {
			err = storepkg.ErrExternalMissing
			break
		}
		var event json.RawMessage
		event, err = db.OperationsEvent(ctx, organization, id)
		if err == nil && event == nil {
			err = storepkg.ErrExternalMissing
		}
		value = event
	default:
		query := r.URL.Query()
		limit := 50
		if raw := query.Get("limit"); raw != "" {
			limit, err = strconv.Atoi(raw)
			if err != nil || limit < 1 || limit > 100 {
				err = storepkg.ErrExternalInput
				break
			}
		}
		if r.URL.Path == "/api/v1/assets" {
			value, err = db.AssetPage(ctx, storepkg.AssetPageQuery{Search: query.Get("q"), Runtime: query.Get("runtime"),
				Cursor: query.Get("cursor"), Limit: limit, OrganizationID: organization}, nil)
		} else {
			value, err = db.EventPage(ctx, storepkg.EventPageQuery{Search: query.Get("q"), Status: query.Get("status"),
				Cursor: query.Get("cursor"), Limit: limit, OrganizationID: organization}, nil)
		}
	}
	switch {
	case err == nil:
		respond(w, status, map[string]any{"schemaVersion": 1, "data": value})
	case errors.Is(err, storepkg.ErrExternalInput) || errors.Is(err, storepkg.ErrInvalidAssetPage) || errors.Is(err, storepkg.ErrInvalidEventPage):
		respond(w, http.StatusBadRequest, map[string]string{"error": "Invalid operations request"})
	case errors.Is(err, storepkg.ErrExternalMissing):
		respond(w, http.StatusNotFound, map[string]string{"error": "Asset or event not found"})
	case errors.Is(err, storepkg.ErrExternalConflict):
		respond(w, http.StatusConflict, map[string]string{"error": "External identity already linked"})
	default:
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Operations storage unavailable"})
	}
}

func servePlatformMetrics(w http.ResponseWriter, r *http.Request, db HealthStore, auth *AuthConfig) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	metric := func(name, help, kind string, value float64) {
		fmt.Fprintf(w, "# HELP dbsphere_%s %s\n# TYPE dbsphere_%s %s\ndbsphere_%s %g\n", name, help, name, kind, name, value)
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	storeUp := db.Health(ctx) == nil
	if storeUp {
		metric("store_up", "Whether PostgreSQL storage is reachable.", "gauge", 1)
	} else {
		metric("store_up", "Whether PostgreSQL storage is reachable.", "gauge", 0)
	}
	if auth.CollectorState != nil {
		state := auth.CollectorState.Snapshot(time.Now())
		metric("collector_cycles_total", "Completed collection cycles on this replica.", "counter", float64(state.CompletedCycles))
		metric("collector_last_cycle_seconds", "Duration of the latest completed collection cycle.", "gauge", float64(state.CycleDurationMs)/1000)
		metric("collector_last_target_failures", "Target failures in the latest collection cycle.", "gauge", float64(state.Cycle.TargetFailures))
		metric("collector_last_errors", "Collector errors in the latest cycle.", "gauge", float64(state.Cycle.CollectorErrors))
	}
	queueDB, ok := db.(interface {
		OperationsQueue(context.Context) (storepkg.OperationsQueue, error)
	})
	if ok && storeUp {
		if queue, err := queueDB.OperationsQueue(ctx); err == nil {
			metric("notification_pending", "Pending notification deliveries in shared storage.", "gauge", float64(queue.Pending))
			metric("notification_failed", "Failed notification deliveries in shared storage.", "gauge", float64(queue.Failed))
		}
	}
	if db, ok := db.(prometheusAssetReader); ok && storeUp && auth.Observability.PrometheusURL != nil {
		staleAfter := 0
		if auth.AutoCollect {
			staleAfter = max(10, 2*auth.CollectEvery)
		}
		assets, err := db.PrometheusAssets(ctx, staleAfter)
		if err == nil {
			metric("asset_projection_up", "Whether asset metrics were read from PostgreSQL.", "gauge", 1)
			writeAssetMetrics(w, assets)
		} else {
			metric("asset_projection_up", "Whether asset metrics were read from PostgreSQL.", "gauge", 0)
		}
	}
}
