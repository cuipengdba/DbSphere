package httpapi

import (
	"context"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/cuipengdba/dbsphere/internal/security"
)

const grafanaPrefix, grafanaReturnPath = "/grafana", "/grafana/dbsphere-return"

var storageDeviceID = regexp.MustCompile(`^\d{1,10}:\d{1,10}$`)
var storageClaimName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?$`)

func grafanaPrincipal(r *http.Request, db HealthStore, auth *AuthConfig) (string, int) {
	cookie, err := r.Cookie("dbsphere_session")
	if err != nil {
		return "", http.StatusUnauthorized
	}
	hash := auth.PasswordHash
	if stored, ok := r.Context().Value(administratorHashKey{}).(string); ok {
		hash = stored
	}
	now := time.Now()
	if role, ok := security.SessionRole(security.AdminSessionKey(auth.MasterKey, hash), cookie.Value, now); ok {
		if role != "admin" {
			return "", http.StatusForbidden
		}
		return "dbsphere-local-admin", http.StatusOK
	}
	if auth.ViewerPasswordHash != "" {
		if role, ok := security.SessionRole(security.ViewerSessionKey(auth.MasterKey, auth.ViewerPasswordHash), cookie.Value, now); ok && role == "viewer" {
			return "", http.StatusForbidden
		}
	}
	if auth.ApproverPasswordHash != "" {
		if role, ok := security.SessionRole(security.ApproverSessionKey(auth.MasterKey, auth.ApproverPasswordHash), cookie.Value, now); ok && role == "approver" {
			return "", http.StatusForbidden
		}
	}
	if db, ok := db.(accessStore); ok && validAccessToken(cookie.Value) {
		principal, err := db.AccessSession(r.Context(), accessDigest(auth.MasterKey, cookie.Value), now)
		if err == nil && principal.Enabled {
			if principal.Role != "admin" || principal.OrganizationID != "" {
				return "", http.StatusForbidden
			}
			return "dbsphere-admin-" + principal.ID, http.StatusOK
		}
	}
	return "", http.StatusUnauthorized
}

func registerGrafana(mux *http.ServeMux, db HealthStore, auth *AuthConfig) {
	upstream := auth.Observability.GrafanaURL
	if upstream == nil {
		return
	}
	type accountKey struct{}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(p *httputil.ProxyRequest) {
			p.SetURL(upstream)
			p.SetXForwarded()
			p.Out.Header.Del("Authorization")
			p.Out.Header.Del("Cookie")
			p.Out.Header.Set("X-WEBAUTH-USER", p.In.Context().Value(accountKey{}).(string))
		},
		ModifyResponse: func(response *http.Response) error {
			// Viewer identity is derived from the current DbSphere session on each request.
			response.Header.Del("Set-Cookie")
			if location, err := url.Parse(response.Header.Get("Location")); err == nil &&
				(location.Path == grafanaPrefix || strings.HasPrefix(location.Path, grafanaPrefix+"/")) {
				response.Header.Set("Location", location.RequestURI())
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, _ error) {
			respond(w, http.StatusBadGateway, map[string]string{"error": "Grafana 专题面板暂不可用"})
		},
	}
	handler := func(w http.ResponseWriter, r *http.Request) {
		username, status := grafanaPrincipal(r, db, auth)
		if status != http.StatusOK {
			respond(w, status, map[string]string{"error": "需要 DbSphere 平台管理员会话"})
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get("Origin") != "" {
			origin, err := url.Parse(r.Header.Get("Origin"))
			if err != nil || origin.Host != r.Host || (origin.Scheme != "http" && origin.Scheme != "https") {
				respond(w, http.StatusForbidden, map[string]string{"error": "Origin mismatch"})
				return
			}
		}
		if r.URL.Path == grafanaPrefix {
			http.Redirect(w, r, grafanaPrefix+"/", http.StatusTemporaryRedirect)
			return
		}
		if r.URL.Path == grafanaReturnPath && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			returnFromGrafana(w, r)
			return
		}
		proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), accountKey{}, username)))
	}
	mux.HandleFunc(grafanaPrefix, handler)
	mux.HandleFunc(grafanaPrefix+"/", handler)
}

func returnFromGrafana(w http.ResponseWriter, r *http.Request) {
	input := r.URL.Query()
	output := url.Values{}
	view := "live"
	if input.Get("view") == "storage" {
		view = "storage"
		output.Set("mode", "history")
		if id := input.Get("event"); opsUUID.MatchString(id) {
			output.Set("event", id)
		}
	}
	if input.Get("view") == "event" {
		view = "incidents"
		if id := input.Get("event"); opsUUID.MatchString(id) {
			output.Set("event", id)
		}
	}
	if view == "storage" || view == "incidents" && input.Get("source") == "storage" {
		if view == "incidents" {
			output.Set("origin", "storage")
		}
		if container := input.Get("container"); len(container) > 0 && len(container) <= 63 {
			output.Set("container", container)
		}
		if device := input.Get("device"); storageDeviceID.MatchString(device) {
			output.Set("device", device)
		}
		if pvc := strings.ReplaceAll(input.Get("pvc"), `\.`, "."); storageClaimName.MatchString(pvc) {
			output.Set("pvc", pvc)
		}
	}
	if asset := input.Get("asset"); opsUUID.MatchString(asset) {
		output.Set("asset", asset)
	}
	start, startErr := time.Parse(time.RFC3339Nano, input.Get("from"))
	end, endErr := time.Parse(time.RFC3339Nano, input.Get("to"))
	if startErr == nil && endErr == nil && start.Before(end) {
		output.Set("from", start.UTC().Format(time.RFC3339Nano))
		endKey := "end"
		if view == "incidents" || view == "storage" {
			endKey = "to"
		}
		output.Set(endKey, end.UTC().Format(time.RFC3339Nano))
	}
	location := "/#" + view
	if encoded := output.Encode(); encoded != "" {
		location += "?" + encoded
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, location, http.StatusSeeOther)
}
