package security

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var baoPathPart = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)
var baoAssetID = regexp.MustCompile(`^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)

// OpenBao holds deployment-managed bindings to a single KV v2 service. Asset
// data and browser requests cannot choose the service or path.
type OpenBao struct {
	origin   *url.URL
	bindings map[string]OpenBaoBinding
	client   *http.Client
	token    func() (string, error)
}

type OpenBaoBinding struct {
	Path     string `json:"path"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	Username string `json:"username"`
}

func NewOpenBao(address, rawBindings string, caPEM []byte, token func() (string, error)) (*OpenBao, error) {
	if address == "" && rawBindings == "" && len(caPEM) == 0 && token == nil {
		return nil, nil
	}
	origin, err := url.Parse(address)
	if err != nil || origin.Scheme != "https" || origin.Hostname() == "" || origin.User != nil ||
		origin.RawQuery != "" || origin.ForceQuery || origin.Fragment != "" || origin.RawPath != "" ||
		(origin.Path != "" && origin.Path != "/") || token == nil || len(rawBindings) > 64*1024 {
		return nil, errors.New("OpenBao requires a fixed HTTPS origin, token source and asset bindings")
	}
	var bindings map[string]OpenBaoBinding
	if json.Unmarshal([]byte(rawBindings), &bindings) != nil || len(bindings) == 0 || len(bindings) > 1000 {
		return nil, errors.New("OpenBao asset bindings must map asset UUIDs to KV v2 paths")
	}
	for asset, binding := range bindings {
		parts := strings.Split(binding.Path, "/")
		if !baoAssetID.MatchString(asset) || len(parts) < 2 || len(parts) > 12 || len(binding.Path) > 256 ||
			binding.Host == "" || binding.Database == "" || binding.Username == "" || binding.Port < 1 || binding.Port > 65535 {
			return nil, errors.New("invalid OpenBao asset binding")
		}
		for _, part := range parts {
			if !baoPathPart.MatchString(part) {
				return nil, errors.New("invalid OpenBao KV v2 path segment")
			}
		}
		binding.Path = "/v1/" + parts[0] + "/data/" + strings.Join(parts[1:], "/")
		bindings[asset] = binding
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if len(caPEM) > 0 {
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("invalid OpenBao CA bundle")
		}
		transport.TLSClientConfig.RootCAs = roots
	}
	return &OpenBao{origin: origin, bindings: bindings, token: token, client: &http.Client{Transport: transport, Timeout: 3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

type OpenBaoSecret struct {
	Password string
	Version  int
}

func (client *OpenBao) Bound(asset string) bool {
	if client == nil {
		return false
	}
	_, found := client.bindings[asset]
	return found
}

func (client *OpenBao) Read(ctx context.Context, asset, host string, port int, database, username string) (OpenBaoSecret, error) {
	if !client.Bound(asset) {
		return OpenBaoSecret{}, errors.New("OpenBao asset is unbound")
	}
	binding := client.bindings[asset]
	if binding.Host != host || binding.Port != port || binding.Database != database || binding.Username != username {
		return OpenBaoSecret{}, errors.New("OpenBao binding must match the current asset target")
	}
	token, err := client.token()
	token = strings.TrimSpace(token)
	if err != nil || len(token) == 0 || len(token) > 4096 || strings.ContainsAny(token, "\r\n\x00") {
		return OpenBaoSecret{}, errors.New("OpenBao token is unavailable")
	}
	endpoint := *client.origin
	endpoint.Path = binding.Path
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return OpenBaoSecret{}, errors.New("OpenBao request could not be created")
	}
	request.Header.Set("X-Vault-Token", token)
	response, err := client.client.Do(request)
	if err != nil {
		return OpenBaoSecret{}, errors.New("OpenBao connection or TLS check failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return OpenBaoSecret{}, errors.New("OpenBao returned HTTP " + strconv.Itoa(response.StatusCode))
	}
	const maxBody = 16 * 1024
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	var result struct {
		Data struct {
			Data     struct{ Password string }
			Metadata struct {
				Version      int
				Destroyed    bool
				DeletionTime string `json:"deletion_time"`
			}
		}
	}
	if err != nil || len(raw) > maxBody || json.Unmarshal(raw, &result) != nil ||
		result.Data.Metadata.Version < 1 || result.Data.Metadata.Destroyed || result.Data.Metadata.DeletionTime != "" ||
		len(result.Data.Data.Password) == 0 || len(result.Data.Data.Password) > 1024 || strings.ContainsRune(result.Data.Data.Password, 0) {
		return OpenBaoSecret{}, errors.New("OpenBao KV v2 response is invalid")
	}
	return OpenBaoSecret{Password: result.Data.Data.Password, Version: result.Data.Metadata.Version}, nil
}
