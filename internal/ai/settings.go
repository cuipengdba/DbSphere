package ai

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cuipengdba/dbsphere/internal/security"
)

var ErrSettingsChanged = errors.New("AI configuration changed")
var ErrProbeBusy = errors.New("AI connection test is already running or was just started")

var modelOrganizationID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var modelReference = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,79}$`)

type Settings struct {
	Enabled        bool   `json:"enabled"`
	Endpoint       string `json:"endpoint"`
	Model          string `json:"model"`
	APIKey         string `json:"apiKey,omitempty"`
	DailyLimit     int    `json:"dailyLimit"`
	ReviewSeconds  int    `json:"reviewSeconds"`
	ReviewSchema   bool   `json:"reviewSchema"`
	AutoSummary    bool   `json:"autoSummary"`
	OrganizationID string `json:"organizationId"`
	Mode           string `json:"mode"`
	CARef          string `json:"caRef"`
	KeyRef         string `json:"keyRef"`
}

type SettingsStore interface {
	ReadAISettings(context.Context) (string, error)
	CompareAISettings(context.Context, string, string) (bool, error)
}

type ActiveSettings struct {
	Settings   Settings
	Client     *Client
	Revision   string
	Overridden bool
	Invalid    bool
}

func (s ActiveSettings) Public() map[string]any {
	return map[string]any{"enabled": s.Client != nil, "endpoint": s.Settings.Endpoint, "model": s.Settings.Model,
		"hasKey": s.Settings.APIKey != "", "dailyLimit": s.Settings.DailyLimit, "reviewSeconds": s.Settings.ReviewSeconds,
		"reviewSchema": s.Settings.ReviewSchema, "autoSummary": s.Settings.AutoSummary, "revision": s.Revision,
		"organizationId": s.Settings.OrganizationID, "mode": s.Settings.Mode, "caRef": s.Settings.CARef, "keyRef": s.Settings.KeyRef,
		"overridden": s.Overridden, "invalid": s.Invalid}
}

type Runtime struct {
	store   SettingsStore
	key     []byte
	base    ActiveSettings
	origins map[string]bool
	trust   map[string]*x509.CertPool
	secrets map[string]string

	mu        sync.Mutex
	saved     ActiveSettings
	probing   bool
	lastProbe time.Time
}

func NewRuntime(store SettingsStore, key []byte, base Settings, allowRemote bool, origins string) (*Runtime, error) {
	return NewRuntimeWithReferences(store, key, base, allowRemote, origins, nil, nil)
}

func NewRuntimeWithReferences(store SettingsStore, key []byte, base Settings, allowRemote bool, origins string, trust, secrets map[string]string) (*Runtime, error) {
	r := &Runtime{store: store, key: key, origins: map[string]bool{}, trust: map[string]*x509.CertPool{}, secrets: secrets}
	for name, encoded := range trust {
		data, err := base64.StdEncoding.DecodeString(encoded)
		pool := x509.NewCertPool()
		if !modelReference.MatchString(name) || len(data) > 256<<10 || err != nil || !pool.AppendCertsFromPEM(data) {
			return nil, ErrInvalidConfiguration
		}
		r.trust[name] = pool
	}
	if allowRemote && base.Endpoint != "" {
		endpoint, err := url.Parse(base.Endpoint)
		if err != nil {
			return nil, ErrInvalidConfiguration
		}
		r.origins[modelOrigin(endpoint)] = true
	}
	for _, raw := range strings.Split(origins, ",") {
		if raw = strings.TrimSpace(raw); raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || !allowRemote {
			return nil, ErrInvalidConfiguration
		}
		r.origins[modelOrigin(u)] = true
	}
	var client *Client
	if base.Endpoint != "" || base.Model != "" {
		var err error
		client, err = r.newClient(base)
		if err != nil {
			return nil, err
		}
	}
	if base.AutoSummary && client == nil {
		return nil, ErrInvalidConfiguration
	}
	base.Enabled = client != nil
	publicBase := base
	publicBase.APIKey = ""
	wire, _ := json.Marshal(publicBase)
	r.base = ActiveSettings{Settings: base, Client: client, Revision: revision(string(wire))}
	return r, nil
}

func (s Settings) clientConfig(remote bool) Config {
	return Config{URL: s.Endpoint, Model: s.Model, APIKey: s.APIKey, AllowRemote: remote,
		ReviewSchema: s.ReviewSchema, ReviewTimeout: time.Duration(s.ReviewSeconds) * time.Second}
}

func modelOrigin(u *url.URL) string {
	port := u.Port()
	if port == "" && u.Scheme == "https" {
		port = "443"
	}
	return u.Scheme + "://" + strings.ToLower(u.Hostname()) + ":" + port
}

func revision(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (r *Runtime) newClient(s Settings) (*Client, error) {
	if s.DailyLimit < 1 || s.DailyLimit > 1000 || s.ReviewSeconds < 1 || s.ReviewSeconds > 120 ||
		len(s.Endpoint) > 2048 || len(s.APIKey) > 4096 || strings.ContainsAny(s.Endpoint+s.Model+s.APIKey, "\r\n\x00") ||
		(s.Endpoint == "") != (s.Model == "") || s.Endpoint == "" && (s.Enabled || s.AutoSummary || s.APIKey != "") {
		return nil, ErrInvalidConfiguration
	}
	u, err := url.Parse(s.Endpoint)
	if err != nil {
		return nil, ErrInvalidConfiguration
	}
	if s.Mode != "" && s.Mode != "local" && s.Mode != "gateway" || (s.Mode == "gateway" || s.CARef != "") && u.Scheme != "https" ||
		s.Mode == "local" && !isLocalEndpoint(u) || s.OrganizationID != "" && !modelOrganizationID.MatchString(s.OrganizationID) {
		return nil, ErrInvalidConfiguration
	}
	cfg := s.clientConfig(r.origins[modelOrigin(u)])
	if s.CARef != "" {
		cfg.RootCAs = r.trust[s.CARef]
		if cfg.RootCAs == nil {
			return nil, ErrInvalidConfiguration
		}
	}
	if s.KeyRef != "" {
		cfg.APIKey = r.secrets[s.KeyRef]
		if !modelReference.MatchString(s.KeyRef) || s.APIKey != "" || cfg.APIKey == "" || len(cfg.APIKey) > 4096 || strings.ContainsAny(cfg.APIKey, "\x00\r\n") {
			return nil, ErrInvalidConfiguration
		}
	}
	return New(cfg)
}

func (s ActiveSettings) ForOrganization(organization string) ActiveSettings {
	if s.Settings.OrganizationID != "" && s.Settings.OrganizationID != organization {
		s.Client = nil
	}
	return s
}

func (r *Runtime) Current(ctx context.Context) (ActiveSettings, error) {
	_, current, err := r.read(ctx)
	return current, err
}

func (r *Runtime) read(ctx context.Context) (string, ActiveSettings, error) {
	raw, err := r.store.ReadAISettings(ctx)
	if err != nil {
		return "", ActiveSettings{}, err
	}
	version := revision(raw)
	r.mu.Lock()
	defer r.mu.Unlock()
	if raw == "" {
		if r.saved.Client != nil {
			r.saved.Client.http.CloseIdleConnections()
		}
		r.saved = ActiveSettings{}
		return raw, r.base, nil
	}
	if r.saved.Revision == version {
		return raw, r.saved, nil
	}
	plain, err := security.Decrypt(r.key, raw)
	if err != nil {
		return "", ActiveSettings{}, err
	}
	current := ActiveSettings{Revision: version, Overridden: true}
	if err := json.Unmarshal([]byte(plain), &current.Settings); err != nil {
		return "", ActiveSettings{}, err
	}
	if current.Settings.Enabled {
		current.Client, err = r.newClient(current.Settings)
		current.Invalid = err != nil
	}
	if r.saved.Client != nil {
		r.saved.Client.http.CloseIdleConnections()
	}
	r.saved = current
	return raw, current, nil
}

func proposedSettings(current ActiveSettings, input Settings, keepKey bool) (Settings, error) {
	input.Endpoint = strings.TrimSpace(input.Endpoint)
	input.Model = strings.TrimSpace(input.Model)
	if keepKey && input.KeyRef == "" {
		if input.APIKey != "" || (current.Settings.APIKey != "" && input.Endpoint != current.Settings.Endpoint) {
			return Settings{}, ErrInvalidConfiguration
		}
		input.APIKey = current.Settings.APIKey
	}
	return input, nil
}

func (r *Runtime) Save(ctx context.Context, expected string, input *Settings, keepKey bool) error {
	raw, previous, err := r.read(ctx)
	if err != nil {
		return err
	}
	if expected != previous.Revision {
		return ErrSettingsChanged
	}
	replacement := ""
	if input != nil {
		settings, err := proposedSettings(previous, *input, keepKey)
		if err != nil {
			return err
		}
		if _, err := r.newClient(settings); err != nil {
			return err
		}
		wire, err := json.Marshal(settings)
		if err != nil {
			return err
		}
		replacement, err = security.Encrypt(r.key, string(wire))
		if err != nil {
			return err
		}
	}
	changed, err := r.store.CompareAISettings(ctx, raw, replacement)
	if err == nil && !changed {
		return ErrSettingsChanged
	}
	return err
}

func (r *Runtime) Probe(ctx context.Context, expected string, input Settings, keepKey bool) error {
	current, err := r.Current(ctx)
	if err != nil {
		return err
	}
	if current.Revision != expected {
		return ErrSettingsChanged
	}
	input, err = proposedSettings(current, input, keepKey)
	if err != nil {
		return err
	}
	client, err := r.newClient(input)
	if err != nil || client == nil {
		return ErrInvalidConfiguration
	}
	r.mu.Lock()
	if r.probing || time.Since(r.lastProbe) < 3*time.Second {
		r.mu.Unlock()
		return ErrProbeBusy
	}
	r.probing = true
	r.lastProbe = time.Now()
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.probing = false
		r.mu.Unlock()
	}()
	defer client.http.CloseIdleConnections()
	wire, err := json.Marshal(map[string]any{"model": input.Model, "stream": false, "temperature": 0, "max_tokens": 32,
		"messages": []map[string]string{{"role": "user", "content": "DbSphere connection test. Reply OK."}}})
	if err != nil {
		return err
	}
	// A nonempty, token-limited reply still proves that the connection works.
	answer, err := client.completion(ctx, client.http, wire, true)
	if err != nil || strings.TrimSpace(answer) == "" {
		return ErrUnavailable
	}
	return nil
}
