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
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cuipengdba/dbsphere"
	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/cuipengdba/dbsphere/internal/security"
	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

type HealthStore interface{ Health(context.Context) error }
type CollectionReader interface {
	ReadCollection(context.Context, string) (any, bool, error)
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
var eventPath = regexp.MustCompile(`^/api/events/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)
var eventAckPath = regexp.MustCompile(`^/api/events/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/ack$`)
var assetSectionPath = regexp.MustCompile(`^/api/assets/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/(samples|sql|slow-logs|sql-plans|series|capacity)$`)

type AssetChecker interface {
	CheckAsset(context.Context, string, []byte) (storepkg.CheckResult, error)
}
type EventStore interface {
	EventByID(context.Context, string) (any, error)
	AcknowledgeEvent(context.Context, string) (any, error)
	RawSample(context.Context, string, string) (any, error)
}
type AssetSectionReader interface {
	ReadAssetSection(context.Context, string, string, url.Values) (any, error)
}

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
			}
			if r.Method == http.MethodPost {
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
			if reader, ok := store.(CollectionReader); ok {
				value, found, err := reader.ReadCollection(r.Context(), r.URL.Path)
				if found {
					if err != nil {
						respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Monitoring data unavailable"})
						return
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
