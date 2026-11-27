package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"time"

	"github.com/cuipengdba/dbsphere/internal/security"
)

var errEmergencyAudit = errors.New("emergency login requires signed audit storage")

type emergencyAdminWindowStore interface {
	EmergencyAdminWindow(context.Context) (string, error)
}

func requestAdminKey(r *http.Request, db HealthStore, cfg *AuthConfig, bridge netip.Addr) ([]byte, time.Time, error) {
	if !cfg.EmergencyAdmin {
		return security.AdminSessionKey(cfg.MasterKey, cfg.PasswordHash), time.Time{}, nil
	}
	store, ok := db.(emergencyAdminWindowStore)
	if !ok || !localSetupAllowed(r, bridge) {
		return nil, time.Time{}, nil
	}
	window, err := store.EmergencyAdminWindow(r.Context())
	if err != nil || window == "" {
		return nil, time.Time{}, err
	}
	until, err := time.Parse(time.RFC3339Nano, window)
	if err != nil || !until.After(time.Now()) {
		return nil, time.Time{}, err
	}
	return security.AdminSessionKey(cfg.MasterKey, cfg.PasswordHash+":emergency:"+window), until, nil
}

func recordEmergencyLogin(ctx context.Context, db HealthStore, key []byte) error {
	audit, ok := db.(signedOrganizationAudit)
	if !ok {
		return errEmergencyAudit
	}
	id, err := audit.StartSignedRoleAudit(ctx, "admin", http.MethodPost, "/api/admin/emergency/login", key)
	if err != nil {
		return err
	}
	return audit.FinishSignedAdminAudit(ctx, id, http.StatusOK, key)
}
