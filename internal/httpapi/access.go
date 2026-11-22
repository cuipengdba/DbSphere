package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/cuipengdba/dbsphere/internal/security"
	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

type accessStore interface {
	AccessCredential(context.Context, string) (storepkg.AccessAccount, error)
	AccessSession(context.Context, string, time.Time) (storepkg.AccessAccount, error)
	CreateAccessSession(context.Context, storepkg.AccessAccount, string, time.Time, time.Time) error
	RevokeAccessSession(context.Context, string) error
	AccessDirectory(context.Context) (any, error)
	WriteAccessAccount(context.Context, string, storepkg.AccessAccountInput, string) (storepkg.AccessAccount, error)
	WriteAccessTeam(context.Context, string, storepkg.AccessTeamInput) (any, error)
	RotateAccessCredential(context.Context, string, string) error
	AccessHandoff(context.Context, string) (any, error)
	SetAccessHandoff(context.Context, string, string, string) error
	HasAccessApprover(context.Context, string) bool
}

const accessTokenPrefix = "na_"
const accessUUID = "[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}"

var accessAccountPath = regexp.MustCompile("^/api/admin/accounts/(" + accessUUID + ")(/rotate)?$")
var accessTeamPath = regexp.MustCompile("^/api/admin/teams/(" + accessUUID + ")$")
var eventHandoffPath = regexp.MustCompile("^/api/events/(" + accessUUID + ")/handoff$")

func accessDigest(master []byte, token string) string {
	return security.CSRFToken(master, "named-session:"+token)
}

func validAccessToken(token string) bool {
	if len(token) != len(accessTokenPrefix)+43 || !strings.HasPrefix(token, accessTokenPrefix) {
		return false
	}
	_, err := base64.RawURLEncoding.Strict().DecodeString(token[len(accessTokenPrefix):])
	return err == nil
}

func issueAccessSession(ctx context.Context, db accessStore, master []byte, account storepkg.AccessAccount, now time.Time) (string, error) {
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	token := accessTokenPrefix + base64.RawURLEncoding.EncodeToString(nonce[:])
	return token, db.CreateAccessSession(ctx, account, accessDigest(master, token), now, now.Add(12*time.Hour))
}

func accessPassword(password string) (string, error) {
	if len(password) < 12 || len(password) > 256 || strings.TrimSpace(password) == "" {
		return "", storepkg.ErrAccessInput
	}
	return security.HashPassword(password)
}

func legacyAccessHash(config *AuthConfig, role string) string {
	switch role {
	case "admin":
		return config.PasswordHash
	case "viewer":
		return config.ViewerPasswordHash
	case "approver":
		return config.ApproverPasswordHash
	}
	return ""
}

func authenticateAccessLogin(ctx context.Context, db accessStore, config *AuthConfig, name, password string) (string, storepkg.AccessAccount, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		for _, role := range []string{"admin", "viewer", "approver"} {
			if hash := legacyAccessHash(config, role); hash != "" && security.VerifyPassword(password, hash) {
				return role, storepkg.AccessAccount{}, nil
			}
		}
		return "", storepkg.AccessAccount{}, nil
	}
	if hash := legacyAccessHash(config, name); hash != "" {
		if security.VerifyPassword(password, hash) {
			return name, storepkg.AccessAccount{}, nil
		}
		return "", storepkg.AccessAccount{}, nil
	}
	if db == nil || len(name) > 48 || len(password) > 256 {
		return "", storepkg.AccessAccount{}, nil
	}
	account, err := db.AccessCredential(ctx, name)
	if err != nil && !errors.Is(err, storepkg.ErrAccessMissing) {
		return "", storepkg.AccessAccount{}, err
	}
	hash := account.Hash
	if hash == "" {
		hash = strings.Repeat("0", 32) + ":" + strings.Repeat("0", 64)
	}
	if security.VerifyPassword(password, hash) && account.Enabled {
		return account.Role, account, nil
	}
	return "", storepkg.AccessAccount{}, nil
}

func writeAccessResult(w http.ResponseWriter, status int, value any, err error) {
	switch {
	case errors.Is(err, storepkg.ErrAccessMissing), errors.Is(err, storepkg.ErrEventNotFound):
		respond(w, http.StatusNotFound, map[string]string{"error": "账号、团队或事件不可用"})
	case errors.Is(err, storepkg.ErrAccessConflict):
		respond(w, http.StatusConflict, map[string]string{"error": "账号或同一项目的团队已存在"})
	case errors.Is(err, storepkg.ErrWorkOrderConflict):
		respond(w, http.StatusConflict, map[string]string{"error": "事件已关闭"})
	case errors.Is(err, storepkg.ErrAccessInput):
		respond(w, http.StatusBadRequest, map[string]string{"error": "账号、密码、团队或资产授权无效"})
	case err != nil:
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "账号与团队服务暂不可用"})
	default:
		respond(w, status, value)
	}
}

func handleAccessDirectory(w http.ResponseWriter, r *http.Request, db accessStore, role string, config *AuthConfig) bool {
	path := r.URL.Path
	account := accessAccountPath.FindStringSubmatch(path)
	team := accessTeamPath.FindStringSubmatch(path)
	if path != "/api/admin/access" && path != "/api/admin/accounts" && path != "/api/admin/teams" && account == nil && team == nil {
		return false
	}
	if role != "admin" || db == nil {
		respond(w, http.StatusForbidden, map[string]string{"error": "Administrator access required"})
		return true
	}
	switch {
	case r.Method == http.MethodGet && path == "/api/admin/access":
		data, err := db.AccessDirectory(r.Context())
		writeAccessResult(w, http.StatusOK, data, err)
	case r.Method == http.MethodPost && path == "/api/admin/accounts":
		var input struct {
			storepkg.AccessAccountInput
			Password string `json:"password"`
			Legacy   string `json:"legacyRole"`
		}
		if readJSONLimit(w, r, &input, 8192) != nil || !prepareOIDCBinding(&input.AccessAccountInput, config) ||
			input.Legacy != "" && (input.OrganizationID != "" || storepkg.OrganizationFromContext(r.Context()) != "") {
			writeAccessResult(w, 0, nil, storepkg.ErrAccessInput)
			break
		}
		var hash string
		var err error
		if input.Legacy == "" {
			hash, err = accessPassword(input.Password)
		} else {
			hash = legacyAccessHash(config, input.Legacy)
			if hash == "" || input.Role != input.Legacy || input.Password != "" {
				err = storepkg.ErrAccessInput
			}
		}
		if err != nil {
			writeAccessResult(w, 0, nil, err)
			break
		}
		data, err := db.WriteAccessAccount(r.Context(), "", input.AccessAccountInput, hash)
		writeAccessResult(w, http.StatusCreated, map[string]any{"account": data}, err)
	case r.Method == http.MethodPatch && account != nil && account[2] == "":
		var input storepkg.AccessAccountInput
		if readJSONLimit(w, r, &input, 8192) != nil || !prepareOIDCBinding(&input, config) {
			writeAccessResult(w, 0, nil, storepkg.ErrAccessInput)
			break
		}
		data, err := db.WriteAccessAccount(r.Context(), account[1], input, "")
		writeAccessResult(w, http.StatusOK, map[string]any{"account": data}, err)
	case r.Method == http.MethodPost && account != nil && account[2] == "/rotate":
		var input struct{ Password string }
		if readJSONLimit(w, r, &input, 1024) != nil {
			writeAccessResult(w, 0, nil, storepkg.ErrAccessInput)
			break
		}
		hash, err := accessPassword(input.Password)
		if err == nil {
			err = db.RotateAccessCredential(r.Context(), account[1], hash)
		}
		writeAccessResult(w, http.StatusOK, map[string]bool{"ok": true}, err)
	case r.Method == http.MethodPost && path == "/api/admin/teams" || r.Method == http.MethodPatch && team != nil:
		var input storepkg.AccessTeamInput
		if readJSONLimit(w, r, &input, 20*1024) != nil {
			writeAccessResult(w, 0, nil, storepkg.ErrAccessInput)
			break
		}
		id, status := "", http.StatusCreated
		if team != nil {
			id, status = team[1], http.StatusOK
		}
		data, err := db.WriteAccessTeam(r.Context(), id, input)
		writeAccessResult(w, status, map[string]any{"team": data}, err)
	default:
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
	}
	return true
}

func prepareOIDCBinding(input *storepkg.AccessAccountInput, config *AuthConfig) bool {
	if input.OIDCSubject == nil || *input.OIDCSubject == "" {
		return true
	}
	if config.OIDC == nil {
		return false
	}
	input.OIDCIssuer = config.OIDC.Issuer
	return true
}

func handleEventHandoff(w http.ResponseWriter, r *http.Request, db accessStore, role, eventID string) {
	if db == nil || (role != "admin" && role != "operator") {
		respond(w, http.StatusForbidden, map[string]string{"error": "Operator access required"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		data, err := db.AccessHandoff(r.Context(), eventID)
		writeAccessResult(w, http.StatusOK, data, err)
	case http.MethodPost:
		var input struct {
			AccountID string `json:"accountId"`
			Note      string `json:"note"`
		}
		if readJSONLimit(w, r, &input, 4096) != nil {
			writeAccessResult(w, 0, nil, storepkg.ErrAccessInput)
			return
		}
		err := db.SetAccessHandoff(r.Context(), eventID, input.AccountID, input.Note)
		writeAccessResult(w, http.StatusOK, map[string]bool{"ok": true}, err)
	default:
		respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
	}
}

func authorizeNamedScope(w http.ResponseWriter, r *http.Request, db HealthStore, role string, allowed map[string]bool) bool {
	path := r.URL.Path
	if r.Method == http.MethodGet && eventShareReaderPath.MatchString(path) {
		return true
	}
	if match := eventShareMakerPath.FindStringSubmatch(path); match != nil && role == "operator" {
		return authorizeScopedResource(w, r, db, allowed, "event", match[1])
	}
	if r.Method == http.MethodGet {
		if path == "/api/access/assets/export" {
			return true
		}
		if match := eventWorkOrdersPath.FindStringSubmatch(path); match != nil && (role == "operator" || role == "approver") {
			return authorizeScopedResource(w, r, db, allowed, "event", match[1])
		}
		if match := eventHandoffPath.FindStringSubmatch(path); match != nil && role == "operator" {
			return authorizeScopedResource(w, r, db, allowed, "event", match[1])
		}
		return authorizeScopedViewerRead(w, r, db, allowed)
	}
	if r.Method == http.MethodPost && role == "approver" {
		if match := workOrderDecisionPath.FindStringSubmatch(path); match != nil {
			return authorizeScopedResource(w, r, db, allowed, "work_order", match[1])
		}
	}
	if r.Method == http.MethodPost && role == "operator" {
		if match := assetRiskPath.FindStringSubmatch(path); match != nil && match[2] == "review" {
			if allowed[match[1]] {
				return true
			}
			respond(w, http.StatusNotFound, map[string]string{"error": "Asset not found"})
			return false
		}
		if match := assetCheckPath.FindStringSubmatch(path); match != nil {
			if allowed[match[1]] {
				return true
			}
			respond(w, http.StatusNotFound, map[string]string{"error": "Asset not found"})
			return false
		}
		for _, route := range []*regexp.Regexp{eventAckPath, eventResponsePath, eventWorkOrdersPath, workOrderPreviewPath, eventHandoffPath} {
			if match := route.FindStringSubmatch(path); match != nil {
				return authorizeScopedResource(w, r, db, allowed, "event", match[1])
			}
		}
	}
	respond(w, http.StatusForbidden, map[string]string{"error": "Action unavailable to this role"})
	return false
}
