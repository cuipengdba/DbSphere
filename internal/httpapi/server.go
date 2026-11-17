package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/cuipengdba/dbsphere"
	"github.com/cuipengdba/dbsphere/internal/security"
)

type HealthStore interface{ Health(context.Context) error }

type AuthConfig struct {
	MasterKey    []byte
	PasswordHash string
	SecureCookie bool
}

type loginAttempt struct {
	count int
	until time.Time
}

func New(store HealthStore, auth ...AuthConfig) http.Handler {
	var credentials *AuthConfig
	if len(auth) > 0 {
		credentials = &auth[0]
	}
	var attemptsMu sync.Mutex
	attempts := map[string]loginAttempt{}
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
		respond(w, http.StatusOK, map[string]string{"backend": "go", "stage": "migration-foundation"})
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
		loggedIn := security.VerifySession(credentials.MasterKey, token, time.Now())
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
			if !security.VerifyPassword(input.Password, credentials.PasswordHash) {
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
			session, err := security.CreateSession(credentials.MasterKey, time.Now())
			if err != nil {
				respond(w, http.StatusInternalServerError, map[string]string{"error": "Session unavailable"})
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "dbsphere_session", Value: session, Path: "/", MaxAge: 43200,
				HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: credentials.SecureCookie})
			respond(w, http.StatusOK, map[string]any{"authenticated": true, "csrf": security.CSRFToken(credentials.MasterKey, session)})
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/session" {
			var csrf any
			if loggedIn {
				csrf = security.CSRFToken(credentials.MasterKey, token)
			}
			respond(w, http.StatusOK, map[string]any{"authenticated": loggedIn, "csrf": csrf})
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
		respond(w, http.StatusNotImplemented, map[string]string{"error": "Go API migration in progress"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != "/" && r.URL.Path != "/index.html" && r.URL.Path != "/app.js" &&
			r.URL.Path != "/styles.css" && r.URL.Path != "/favicon.svg" {
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
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return io.ErrUnexpectedEOF
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
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
