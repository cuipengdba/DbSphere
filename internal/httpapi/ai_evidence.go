package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"time"

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
	reply := map[string]any{"eventId": eventID, "assetId": packet.AssetID, "sources": packet.Sources,
		"result": packet.Fallback, "inputBytes": len(wire), "maxInputBytes": ai.MaxReviewBytes, "dailyLimit": auth.AIDailyLimit}
	reply["canRequest"] = auth.AI != nil && len(packet.Sources) > 0 && len(wire) <= ai.MaxReviewBytes && (role == "admin" || role == "operator")
	switch {
	case r.Method == http.MethodGet && packet.Latest != nil:
		reply["status"] = "model_saved"
		reply["result"] = packet.Latest
	case len(packet.Sources) == 0 || len(wire) > ai.MaxReviewBytes:
		reply["status"] = "insufficient"
	case auth.AI == nil:
		reply["status"] = "no_model"
	case r.Method == http.MethodGet:
		reply["status"] = "ready"
	default:
		invocation, err := store.ReserveAIEvidenceReview(r.Context(), eventID, auth.AI.Model(), auth.AIDailyLimit)
		if errors.Is(err, storepkg.ErrAIBudgetExceeded) {
			reply["status"] = "budget_exhausted"
			respond(w, http.StatusOK, reply)
			return true
		}
		if err != nil {
			respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Evidence review audit unavailable"})
			return true
		}
		review, modelErr := auth.AI.ReviewEvidence(r.Context(), packet.Sources)
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
		if err := store.CompleteAIEvidenceReview(ctx, eventID, invocation, reason, len(packet.Sources), len(wire), accepted); err != nil {
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
