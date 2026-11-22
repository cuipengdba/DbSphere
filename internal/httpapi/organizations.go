package httpapi

import (
	"context"
	"net/http"
	"regexp"

	"github.com/cuipengdba/dbsphere/internal/security"
	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

type organizationStore interface {
	Organizations(context.Context) ([]storepkg.Organization, error)
	CreateOrganization(context.Context, string) (storepkg.Organization, error)
	LegacyOrganizationScope(context.Context, []string) ([]string, bool, error)
	CreateEventShare(context.Context, string, int) (any, error)
	ResolveEventShare(context.Context, string) (string, error)
	RevokeEventShare(context.Context, string, string) error
	EventShares(context.Context, string) (any, error)
}

var eventShareMakerPath = regexp.MustCompile("^/api/events/(" + accessUUID + ")/shares(?:/(" + accessUUID + "))?$")
var eventShareReaderPath = regexp.MustCompile("^/api/event-shares/(" + accessUUID + ")$")
var organizationAssetPath = regexp.MustCompile("^/api/assets/(" + accessUUID + ")$")

func scopedPrincipalIDs(ctx context.Context, db HealthStore, config *AuthConfig, account storepkg.AccessAccount, role string) ([]string, error) {
	if account.ID != "" {
		if role != "admin" || account.OrganizationID != "" {
			return append([]string{}, account.AssetIDs...), nil
		}
		return nil, nil
	}
	if role != "viewer" && role != "approver" {
		return nil, nil
	}
	var explicit []string
	if role == "viewer" && len(config.ViewerAssetIDs) > 0 {
		explicit = config.ViewerAssetIDs
	}
	if org, ok := db.(organizationStore); ok {
		ids, restricted, err := org.LegacyOrganizationScope(ctx, explicit)
		if err != nil || restricted {
			return ids, err
		}
	}
	return explicit, nil
}

func authorizeOrganizationAdmin(w http.ResponseWriter, r *http.Request, db HealthStore, allowed map[string]bool) bool {
	path := r.URL.Path
	if r.Method == http.MethodGet && (path == "/api/admin/access" || path == "/api/admin/organizations" || path == "/api/admin/organization-audit") ||
		r.Method != http.MethodGet && (path == "/api/admin/accounts" || path == "/api/admin/teams" ||
			accessAccountPath.MatchString(path) || accessTeamPath.MatchString(path)) {
		return true
	}
	if r.Method == http.MethodPost && path == "/api/assets" {
		return true
	}
	if r.Method == http.MethodPatch || r.Method == http.MethodDelete {
		if match := organizationAssetPath.FindStringSubmatch(path); match != nil {
			if allowed[match[1]] {
				return true
			}
			respond(w, http.StatusNotFound, map[string]string{"error": "Asset not found"})
			return false
		}
	}
	if r.Method == http.MethodGet {
		if match := eventAuditPath.FindStringSubmatch(path); match != nil {
			return authorizeScopedResource(w, r, db, allowed, "event", match[1])
		}
	}
	if r.Method == http.MethodPost {
		for _, route := range []*regexp.Regexp{eventAIPath, eventAIQuestionPath, eventDecisionPath} {
			if match := route.FindStringSubmatch(path); match != nil {
				return authorizeScopedResource(w, r, db, allowed, "event", match[1])
			}
		}
		if match := workOrderExecutePath.FindStringSubmatch(path); match != nil {
			return authorizeScopedResource(w, r, db, allowed, "work_order", match[1])
		}
	}
	return authorizeNamedScope(w, r, db, "operator", allowed)
}

func handleOrganizationDirectory(w http.ResponseWriter, r *http.Request, db HealthStore, role string) {
	org, ok := db.(organizationStore)
	if !ok || role != "admin" {
		respond(w, http.StatusForbidden, map[string]string{"error": "Administrator access required"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		organizations, err := org.Organizations(r.Context())
		writeAccessResult(w, http.StatusOK, map[string]any{"organizations": organizations}, err)
	case http.MethodPost:
		if storepkg.OrganizationFromContext(r.Context()) != "" {
			respond(w, http.StatusForbidden, map[string]string{"error": "Platform administrator access required"})
			return
		}
		var input struct{ Name string }
		if readJSONLimit(w, r, &input, 2048) != nil {
			writeAccessResult(w, 0, nil, storepkg.ErrAccessInput)
			return
		}
		organization, err := org.CreateOrganization(r.Context(), input.Name)
		writeAccessResult(w, http.StatusCreated, map[string]any{"organization": organization}, err)
	default:
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
	}
}

func handleOrganizationShares(w http.ResponseWriter, r *http.Request, db HealthStore, role string, allowed map[string]bool) bool {
	path := r.URL.Path
	reader := eventShareReaderPath.FindStringSubmatch(path)
	writer := eventShareMakerPath.FindStringSubmatch(path)
	if reader == nil && writer == nil {
		return false
	}
	org, ok := db.(organizationStore)
	if !ok {
		respond(w, http.StatusNotImplemented, map[string]string{"error": "Event sharing unavailable"})
		return true
	}
	if reader != nil {
		if r.Method != http.MethodGet {
			respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
			return true
		}
		eventID, err := org.ResolveEventShare(r.Context(), reader[1])
		if err != nil {
			writeAccessResult(w, 0, nil, err)
			return true
		}
		if allowed == nil || authorizeScopedResource(w, r, db, allowed, "event", eventID) {
			respond(w, http.StatusOK, map[string]string{"eventId": eventID})
		}
		return true
	}
	if role != "admin" && role != "operator" {
		respond(w, http.StatusForbidden, map[string]string{"error": "Operator access required"})
		return true
	}
	if allowed != nil && !authorizeScopedResource(w, r, db, allowed, "event", writer[1]) {
		return true
	}
	switch {
	case r.Method == http.MethodGet && writer[2] == "":
		shares, err := org.EventShares(r.Context(), writer[1])
		writeAccessResult(w, http.StatusOK, map[string]any{"shares": shares}, err)
	case r.Method == http.MethodPost && writer[2] == "":
		var input struct{ Hours int }
		if readJSONLimit(w, r, &input, 1024) != nil {
			writeAccessResult(w, 0, nil, storepkg.ErrAccessInput)
			break
		}
		share, err := org.CreateEventShare(r.Context(), writer[1], input.Hours)
		writeAccessResult(w, http.StatusCreated, map[string]any{"share": share}, err)
	case r.Method == http.MethodDelete && writer[2] != "":
		err := org.RevokeEventShare(r.Context(), writer[1], writer[2])
		writeAccessResult(w, http.StatusOK, map[string]bool{"ok": true}, err)
	default:
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
	}
	return true
}

func hasOrganizationApprover(ctx context.Context, db accessStore, config *AuthConfig, org string) bool {
	if (org == "" || org == storepkg.DefaultOrganizationID) && config.ApproverPasswordHash != "" {
		return true
	}
	if org != "" {
		ctx = storepkg.WithOrganization(ctx, org)
	}
	return db != nil && db.HasAccessApprover(ctx, "")
}

func workOrderApproverAvailable(ctx context.Context, db HealthStore, access accessStore, config *AuthConfig, eventID string) bool {
	lookup, ok := db.(scopedResourceLookup)
	if !ok {
		return config.ApproverPasswordHash != ""
	}
	asset, err := lookup.AssetIDForResource(ctx, "event", eventID)
	if err != nil || asset == "" {
		return false
	}
	owner, ok := db.(interface {
		AssetOrganization(context.Context, string) (string, error)
	})
	if ok {
		organization, err := owner.AssetOrganization(ctx, asset)
		if err != nil || organization == "" {
			return false
		}
		if organization == storepkg.DefaultOrganizationID && config.ApproverPasswordHash != "" {
			return true
		}
		ctx = storepkg.WithOrganization(ctx, organization)
	} else if config.ApproverPasswordHash != "" {
		return true
	}
	return access != nil && access.HasAccessApprover(ctx, asset)
}

type signedOrganizationAudit interface {
	StartSignedRoleAudit(context.Context, string, string, string, []byte) (string, error)
	FinishSignedAdminAudit(context.Context, string, int, []byte) error
	OrganizationAudit(context.Context, string, []byte) (security.AuditExport, error)
}

func handleOrganizationAudit(w http.ResponseWriter, r *http.Request, db HealthStore, role string, key []byte) {
	signed, ok := db.(signedOrganizationAudit)
	if role != "admin" || !ok {
		respond(w, http.StatusForbidden, map[string]string{"error": "Administrator access required"})
		return
	}
	requested := r.URL.Query().Get("organizationId")
	if scope := storepkg.OrganizationFromContext(r.Context()); scope != "" && requested != "" && requested != scope {
		respond(w, http.StatusNotFound, map[string]string{"error": "Organization not found"})
		return
	}
	export, err := signed.OrganizationAudit(r.Context(), requested, key)
	if err != nil {
		writeAccessResult(w, 0, nil, err)
		return
	}
	w.Header().Set("Content-Disposition", `attachment; filename="dbsphere-organization-audit.json"`)
	respond(w, http.StatusOK, export)
}
