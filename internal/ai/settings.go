package ai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/cuipengdba/dbsphere/internal/security"
)

var ErrSettingsChanged = errors.New("AI configuration changed")
var ErrProbeBusy = errors.New("AI connection test is already running or was just started")

type Settings struct {
	Enabled       bool   `json:"enabled"`
	Endpoint      string `json:"endpoint"`
	Model         string `json:"model"`
	APIKey        string `json:"apiKey,omitempty"`
	DailyLimit    int    `json:"dailyLimit"`
	ReviewSeconds int    `json:"reviewSeconds"`
	ReviewSchema  bool   `json:"reviewSchema"`
	AutoSummary   bool   `json:"autoSummary"`
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
		"overridden": s.Overridden, "invalid": s.Invalid}
}

type Runtime struct {
	store   SettingsStore
	key     []byte
	base    ActiveSettings
	origins map[string]bool

	mu        sync.Mutex
	saved     ActiveSettings
	probing   bool
	lastProbe time.Time
}

func NewRuntime(store SettingsStore, key []byte, base Settings, allowRemote bool, origins string) (*Runtime, error) {
	client, err := New(base.clientConfig(allowRemote))
	if err != nil || base.AutoSummary && client == nil {
		return nil, ErrInvalidConfiguration
	}
	base.Enabled = client != nil
	publicBase := base
	publicBase.APIKey = ""
	wire, _ := json.Marshal(publicBase)
	r := &Runtime{store: store, key: key, base: ActiveSettings{Settings: base, Client: client, Revision: revision(string(wire))}, origins: map[string]bool{}}
	if allowRemote && base.Endpoint != "" {
		endpoint, _ := url.Parse(base.Endpoint)
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
	return New(s.clientConfig(r.origins[modelOrigin(u)]))
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
	if keepKey {
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
	wire, err := json.Marshal(map[string]any{"model": input.Model, "stream": false, "max_tokens": 32,
		"messages": []map[string]string{{"role": "user", "content": "DbSphere connection test. Reply OK."}}})
	if err != nil {
		return err
	}
	answer, err := client.completion(ctx, client.http, wire)
	if err != nil || strings.TrimSpace(answer) == "" {
		return ErrUnavailable
	}
	return nil
}
