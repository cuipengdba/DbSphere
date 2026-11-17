package httpapi

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"

	"github.com/cuipengdba/dbsphere"
)

type HealthStore interface{ Health(context.Context) error }

func New(store HealthStore) http.Handler {
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
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
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

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
