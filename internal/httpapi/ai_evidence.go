package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cuipengdba/dbsphere/internal/ai"
	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

var eventReviewPath = regexp.MustCompile("^/api/events/(" + accessUUID + ")/ai-evidence-review$")

type aiEvidenceReviewStore interface {
	PrepareAIEvidenceReview(context.Context, string) (storepkg.EvidenceReviewContext, error)
	ReserveAIEvidenceReview(context.Context, string, string, int) (string, error)
	CompleteAIEvidenceReview(context.Context, string, string, string, int, int, *ai.EvidenceReview) error
}

func handleAIEvidenceReview(w http.ResponseWriter, r *http.Request, db HealthStore, auth *AuthConfig, role string) bool {
	match := eventReviewPath.FindStringSubmatch(r.URL.Path)
	if match == nil {
		return false
	}
	if r.Method != http.MethodGet && (r.Method != http.MethodPost || (role != "admin" && role != "operator")) {
		respond(w, http.StatusForbidden, map[string]string{"error": "Evidence review requires an assigned operator"})
		return true
	}
	store, ok := db.(aiEvidenceReviewStore)
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "Evidence review unavailable"})
		return true
	}
	eventID := match[1]
	packet, err := store.PrepareAIEvidenceReview(r.Context(), eventID)
	switch {
	case errors.Is(err, storepkg.ErrEventNotFound):
		respond(w, http.StatusNotFound, map[string]string{"error": "Event not found"})
		return true
	case err != nil:
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Evidence storage unavailable"})
		return true
	}
	wire, err := json.Marshal(packet.Sources)
	if err != nil {
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Evidence packet unavailable"})
		return true
	}
	current, modelStateErr := auth.eventAI(r.Context(), db, eventID)
	if current.Settings.DailyLimit == 0 {
		current.Settings.DailyLimit = auth.AIDailyLimit
	}
	identity := sha256.Sum256([]byte(eventID + "\n" + current.Revision + "\n" + string(wire)))
	preview := hex.EncodeToString(identity[:])
	var input struct {
		Preview  string `json:"preview"`
		Question string `json:"question"`
	}
	if r.Method == http.MethodPost && r.ContentLength != 0 {
		if readJSONLimit(w, r, &input, 4096) != nil || utf8.RuneCountInString(input.Question) > 400 || strings.ContainsAny(input.Question, "\x00\r") {
			respond(w, http.StatusBadRequest, map[string]string{"error": "请检查证据预览与核查问题，问题最多 400 字"})
			return true
		}
		if (input.Preview != "" || input.Question != "") && input.Preview != preview {
			respond(w, http.StatusConflict, map[string]string{"error": "证据或模型配置已更新，请刷新送审预览"})
			return true
		}
	}
	reply := map[string]any{"eventId": eventID, "assetId": packet.AssetID, "sources": packet.Sources,
		"result": packet.Fallback, "inputBytes": len(wire), "maxInputBytes": ai.MaxReviewBytes, "dailyLimit": current.Settings.DailyLimit,
		"preview": preview, "endpointModel": current.Settings.Model}
	reply["canRequest"] = current.Client != nil && len(packet.Sources) > 0 && len(wire) <= ai.MaxReviewBytes && (role == "admin" || role == "operator")
	switch {
	case len(packet.Sources) == 0 || len(wire) > ai.MaxReviewBytes:
		reply["status"] = "insufficient"
	case modelStateErr != nil || current.Invalid:
		reply["status"] = "model_unavailable"
	case current.Client == nil:
		reply["status"] = "no_model"
	case r.Method == http.MethodGet && packet.Latest != nil:
		reply["status"] = "model_saved"
		reply["result"] = packet.Latest
	case r.Method == http.MethodGet:
		reply["status"] = "ready"
	default:
		invocation, err := store.ReserveAIEvidenceReview(r.Context(), eventID, current.Client.Model(), current.Settings.DailyLimit)
		if errors.Is(err, storepkg.ErrAIBudgetExceeded) {
			reply["status"] = "budget_exhausted"
			respond(w, http.StatusOK, reply)
			return true
		}
		if err != nil {
			respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Evidence review audit unavailable"})
			return true
		}
		question := strings.TrimSpace(input.Question)
		review, modelErr := current.Client.ReviewEvidence(r.Context(), packet.Sources, question)
		reason := ""
		var accepted *ai.EvidenceReview
		switch {
		case errors.Is(modelErr, ai.ErrInvalidEvidence):
			reason = "invalid_citations"
		case modelErr != nil:
			reason = "model_unavailable"
		default:
			accepted = &review
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		defer cancel()
		if err := store.CompleteAIEvidenceReview(ctx, eventID, invocation, reason, len(packet.Sources), len(wire)+len(question), accepted); err != nil {
			respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Evidence review audit unavailable"})
			return true
		}
		if accepted != nil {
			reply["status"] = "model"
			reply["result"] = review
		} else {
			reply["status"] = reason
		}
		reply["invocationId"] = invocation
	}
	respond(w, http.StatusOK, reply)
	return true
}
