package httpapi

import (
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

func registerLogStreams(mux *http.ServeMux, db HealthStore) {
	store, ok := db.(*storepkg.Postgres)
	if !ok {
		return
	}
	mux.HandleFunc("/v1/log-sources/{source}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("source")
		if !opsUUID.MatchString(id) {
			http.NotFound(w, r)
			return
		}
		credential := r.Header.Get("Authorization")
		token, ok := strings.CutPrefix(credential, "Bearer ")
		_, decodeErr := hex.DecodeString(token)
		if !ok || len(token) != 64 || decodeErr != nil {
			respond(w, http.StatusUnauthorized, map[string]string{"error": "Invalid log source credential"})
			return
		}
		cursor, err := store.LogStreamCursor(r.Context(), id, token)
		if err != nil {
			writeLogStreamError(w, cursor, err)
			return
		}
		switch r.Method {
		case http.MethodGet:
			respond(w, http.StatusOK, cursor)
		case http.MethodPut:
			mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if mediaType != "application/octet-stream" {
				respond(w, http.StatusUnsupportedMediaType, map[string]string{"error": "Expected application/octet-stream"})
				return
			}
			generation, genErr := strconv.ParseInt(r.Header.Get("X-Log-Generation"), 10, 64)
			offset, offsetErr := strconv.ParseInt(r.Header.Get("X-Log-Offset"), 10, 64)
			senderSize, sizeErr := strconv.ParseInt(r.Header.Get("X-Log-Size"), 10, 64)
			final := r.Header.Get("X-Log-Final")
			if genErr != nil || offsetErr != nil || sizeErr != nil || (final != "true" && final != "false") {
				writeLogStreamError(w, cursor, storepkg.ErrLogInput)
				return
			}
			raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, storepkg.LogArchiveBlockBytes))
			if err != nil {
				writeLogStreamError(w, cursor, err)
				return
			}
			cursor, err = store.AppendLogStream(r.Context(), id, token, storepkg.LogStreamChunk{
				Generation: generation, Offset: offset, SenderSize: senderSize, Filename: r.Header.Get("X-Log-Filename"),
				Final: final == "true", Raw: raw,
			})
			if err != nil {
				writeLogStreamError(w, cursor, err)
				return
			}
			respond(w, http.StatusAccepted, cursor)
		default:
			w.Header().Set("Allow", "GET, PUT")
			respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
		}
	})
}

func writeLogStreamError(w http.ResponseWriter, cursor storepkg.LogStreamCursor, err error) {
	if errors.Is(err, storepkg.ErrLogStreamCredential) {
		respond(w, http.StatusUnauthorized, map[string]string{"error": "Invalid log source credential"})
		return
	}
	status := logAnalysisStatus(http.StatusOK, err)
	if status == http.StatusConflict {
		respond(w, status, map[string]any{"error": "Log generation or offset conflicts with accepted data", "cursor": cursor})
		return
	}
	respond(w, status, map[string]string{"error": http.StatusText(status)})
}
