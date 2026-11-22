package httpapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/cuipengdba/dbsphere/internal/security"
	storepkg "github.com/cuipengdba/dbsphere/internal/store"
	"golang.org/x/oauth2"
)

const oidcCallbackPath = "/api/oidc/callback"
const oidcFlowCookie = "dbsphere_oidc_flow"

type OIDCProvider struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	client       *http.Client
}

type oidcFlow struct {
	Issuer    string
	ClientID  string
	State     string
	Nonce     string
	Verifier  string
	ExpiresAt time.Time
}

func oidcURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("invalid OIDC endpoint")
	}
	if parsed.Scheme != "https" {
		ip := net.ParseIP(parsed.Hostname())
		if parsed.Scheme != "http" || !(parsed.Hostname() == "localhost" || ip != nil && ip.IsLoopback()) {
			return nil, errors.New("OIDC endpoints require HTTPS")
		}
	}
	return parsed, nil
}

func LoadOIDCProvider() (*OIDCProvider, error) {
	settings := &OIDCProvider{Issuer: strings.TrimSpace(os.Getenv("DBSPHERE_OIDC_ISSUER")),
		ClientID: os.Getenv("DBSPHERE_OIDC_CLIENT_ID"), ClientSecret: os.Getenv("DBSPHERE_OIDC_CLIENT_SECRET"),
		RedirectURL: os.Getenv("DBSPHERE_OIDC_REDIRECT_URL")}
	if settings.Issuer == "" && settings.ClientID == "" && settings.ClientSecret == "" && settings.RedirectURL == "" {
		return nil, nil
	}
	if _, err := oidcURL(settings.Issuer); err != nil {
		return nil, err
	}
	redirect, err := oidcURL(settings.RedirectURL)
	if err != nil || redirect.Path != oidcCallbackPath || settings.ClientID == "" || settings.ClientSecret == "" {
		return nil, errors.New("OIDC requires a client ID, secret and an explicit callback URL")
	}
	settings.client = &http.Client{Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return settings, nil
}

func (o *OIDCProvider) discover(ctx context.Context) (*oidc.Provider, oauth2.Config, error) {
	provider, err := oidc.NewProvider(ctx, o.Issuer)
	if err != nil {
		return nil, oauth2.Config{}, err
	}
	endpoints := provider.Endpoint()
	if _, err := oidcURL(endpoints.AuthURL); err != nil {
		return nil, oauth2.Config{}, err
	}
	if _, err := oidcURL(endpoints.TokenURL); err != nil {
		return nil, oauth2.Config{}, err
	}
	var metadata struct {
		Keys string `json:"jwks_uri"`
	}
	if err := provider.Claims(&metadata); err != nil {
		return nil, oauth2.Config{}, err
	}
	if _, err := oidcURL(metadata.Keys); err != nil {
		return nil, oauth2.Config{}, err
	}
	return provider, oauth2.Config{ClientID: o.ClientID, ClientSecret: o.ClientSecret, RedirectURL: o.RedirectURL,
		Endpoint: endpoints, Scopes: []string{oidc.ScopeOpenID}}, nil
}

type oidcAccessStore interface {
	accessStore
	AccessOIDC(context.Context, string, string) (storepkg.AccessAccount, error)
	RecordOIDCFlow(context.Context, string, time.Time) error
	ConsumeOIDCFlow(context.Context, string) (bool, error)
}

func registerOIDC(mux *http.ServeMux, db HealthStore, config *AuthConfig) {
	settings := config.OIDC
	store, ok := db.(oidcAccessStore)
	if !ok {
		return
	}
	secure := strings.HasPrefix(settings.RedirectURL, "https://")
	cookie := func(value string, maxAge int) *http.Cookie {
		return &http.Cookie{Name: oidcFlowCookie, Value: value, Path: oidcCallbackPath, MaxAge: maxAge,
			HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure}
	}
	failed := func(w http.ResponseWriter, r *http.Request, reason string) {
		http.Redirect(w, r, "/?sso="+reason, http.StatusSeeOther)
	}
	mux.HandleFunc("GET /api/oidc/start", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		ctx, cancel := context.WithTimeout(oidc.ClientContext(r.Context(), settings.client), 8*time.Second)
		defer cancel()
		_, oauthConfig, err := settings.discover(ctx)
		if err != nil {
			failed(w, r, "unavailable")
			return
		}
		flow := oidcFlow{Issuer: settings.Issuer, ClientID: settings.ClientID, State: oauth2.GenerateVerifier(),
			Nonce: oauth2.GenerateVerifier(), Verifier: oauth2.GenerateVerifier(), ExpiresAt: time.Now().Add(5 * time.Minute)}
		raw, _ := json.Marshal(flow)
		sealed, err := security.Encrypt(config.MasterKey, string(raw))
		if err != nil {
			failed(w, r, "unavailable")
			return
		}
		if err := store.RecordOIDCFlow(ctx, security.CSRFToken(config.MasterKey, "oidc-flow:"+flow.State), flow.ExpiresAt); err != nil {
			failed(w, r, "unavailable")
			return
		}
		http.SetCookie(w, cookie(sealed, 300))
		http.Redirect(w, r, oauthConfig.AuthCodeURL(flow.State, oauth2.S256ChallengeOption(flow.Verifier), oidc.Nonce(flow.Nonce)), http.StatusSeeOther)
	})
	mux.HandleFunc("GET "+oidcCallbackPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		http.SetCookie(w, cookie("", -1))
		saved, err := r.Cookie(oidcFlowCookie)
		if err != nil || r.URL.Query().Get("error") != "" {
			failed(w, r, "rejected")
			return
		}
		clear, err := security.Decrypt(config.MasterKey, saved.Value)
		var flow oidcFlow
		if err != nil || json.Unmarshal([]byte(clear), &flow) != nil || time.Now().After(flow.ExpiresAt) ||
			flow.Issuer != settings.Issuer || flow.ClientID != settings.ClientID ||
			subtle.ConstantTimeCompare([]byte(flow.State), []byte(r.URL.Query().Get("state"))) != 1 {
			failed(w, r, "rejected")
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" || len(code) > 4096 {
			failed(w, r, "rejected")
			return
		}
		ctx, cancel := context.WithTimeout(oidc.ClientContext(r.Context(), settings.client), 15*time.Second)
		defer cancel()
		valid, err := store.ConsumeOIDCFlow(ctx, security.CSRFToken(config.MasterKey, "oidc-flow:"+flow.State))
		if err != nil {
			failed(w, r, "unavailable")
			return
		}
		if !valid {
			failed(w, r, "rejected")
			return
		}
		provider, oauthConfig, err := settings.discover(ctx)
		if err != nil {
			failed(w, r, "unavailable")
			return
		}
		token, err := oauthConfig.Exchange(ctx, code, oauth2.VerifierOption(flow.Verifier))
		if err != nil {
			failed(w, r, "rejected")
			return
		}
		raw, _ := token.Extra("id_token").(string)
		if raw == "" || len(raw) > 16000 {
			failed(w, r, "rejected")
			return
		}
		identity, err := provider.VerifierContext(ctx, &oidc.Config{ClientID: settings.ClientID}).Verify(ctx, raw)
		if err != nil || identity.Subject == "" || len(identity.Subject) > 255 ||
			subtle.ConstantTimeCompare([]byte(identity.Nonce), []byte(flow.Nonce)) != 1 {
			failed(w, r, "rejected")
			return
		}
		var claims struct {
			AuthorizedParty string `json:"azp"`
		}
		if identity.Claims(&claims) != nil || (len(identity.Audience) > 1 || claims.AuthorizedParty != "") && claims.AuthorizedParty != settings.ClientID {
			failed(w, r, "rejected")
			return
		}
		account, err := store.AccessOIDC(ctx, identity.Issuer, identity.Subject)
		if err != nil || !account.Enabled {
			failed(w, r, "unlinked")
			return
		}
		signed, signedOK := db.(signedOrganizationAudit)
		var auditID string
		if signedOK {
			scope := account.OrganizationID
			if scope == "" && account.Role != "admin" {
				scope = storepkg.DefaultOrganizationID
			}
			auditContext := storepkg.WithAccessActor(storepkg.WithOrganization(ctx, scope), account.Username)
			auditID, err = signed.StartSignedRoleAudit(auditContext, account.Username, http.MethodPost, oidcCallbackPath, config.MasterKey)
			if err != nil {
				failed(w, r, "unavailable")
				return
			}
		}
		session, err := issueAccessSession(ctx, store, config.MasterKey, account, time.Now())
		if signedOK {
			status := http.StatusOK
			if err != nil {
				status = http.StatusServiceUnavailable
			}
			if auditErr := signed.FinishSignedAdminAudit(ctx, auditID, status, config.MasterKey); auditErr != nil {
				failed(w, r, "unavailable")
				return
			}
		}
		if err != nil {
			failed(w, r, "unavailable")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "dbsphere_session", Value: session, Path: "/", MaxAge: 43200, HttpOnly: true,
			SameSite: http.SameSiteStrictMode, Secure: secure || config.SecureCookie})
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})
}
