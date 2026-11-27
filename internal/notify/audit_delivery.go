package notify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"time"

	"github.com/cuipengdba/dbsphere/internal/security"
	"github.com/cuipengdba/dbsphere/internal/store"
)

type AuditDestination struct {
	org, id string
	url     string
	client  *http.Client
}

var auditOrgID = regexp.MustCompile(`^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)

func LoadAuditDestination() (*AuditDestination, error) {
	raw, org, ca64 := os.Getenv("DBSPHERE_AUDIT_EXPORT_URL"), os.Getenv("DBSPHERE_AUDIT_EXPORT_ORGANIZATION"), os.Getenv("DBSPHERE_AUDIT_EXPORT_CA_PEM_BASE64")
	if raw == "" && org == "" && ca64 == "" {
		return nil, nil
	}
	endpoint, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil ||
		endpoint.RawQuery != "" || endpoint.Fragment != "" || !auditOrgID.MatchString(org) || len(ca64) > 256*1024 {
		return nil, errors.New("audit delivery requires an HTTPS endpoint and explicit organization ID")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if ca64 != "" {
		pem, err := base64.StdEncoding.DecodeString(ca64)
		roots := x509.NewCertPool()
		if err != nil || !roots.AppendCertsFromPEM(pem) {
			return nil, errors.New("invalid audit delivery CA bundle")
		}
		transport.TLSClientConfig.RootCAs = roots
	}
	digest := sha256.Sum256([]byte(endpoint.String()))
	return &AuditDestination{org: org, id: hex.EncodeToString(digest[:]), url: endpoint.String(), client: &http.Client{
		Transport: transport, Timeout: 6 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

type auditDeliveryStore interface {
	ClaimAuditDelivery(context.Context, string, string, time.Time) (*store.AuditDelivery, error)
	FinishAuditDelivery(context.Context, string, string, string, bool) error
}

type auditEnvelope struct {
	SchemaVersion  int                 `json:"schemaVersion"`
	OrganizationID string              `json:"organizationId"`
	PublicKey      string              `json:"publicKey"`
	KeyFingerprint string              `json:"keyFingerprint"`
	Entry          security.AuditEntry `json:"entry"`
}

func (d *AuditDestination) Run(ctx context.Context, db auditDeliveryStore, master []byte, log *slog.Logger) {
	publicKey, fingerprint := security.AuditPublicIdentity(master)
	timer := time.NewTicker(time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		claimCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
		item, err := db.ClaimAuditDelivery(claimCtx, d.org, d.id, time.Now().UTC())
		cancel()
		if err != nil {
			log.Warn("audit delivery cursor unavailable", "organizationId", d.org)
			continue
		}
		if item == nil {
			continue
		}
		delivered := d.send(ctx, auditEnvelope{1, d.org, publicKey, fingerprint, item.Entry})
		if !delivered && ctx.Err() == nil {
			log.Warn("signed audit record will be retried", "organizationId", d.org, "sequence", item.Entry.Sequence)
		}
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 4*time.Second)
		if err := db.FinishAuditDelivery(finishCtx, d.org, d.id, item.Lease, delivered); err != nil {
			log.Warn("audit delivery acknowledgement was not persisted", "organizationId", d.org, "sequence", item.Entry.Sequence)
		}
		cancel()
	}
}

func (d *AuditDestination) send(ctx context.Context, envelope auditEnvelope) bool {
	if security.VerifyDeliveredAudit(envelope.OrganizationID, envelope.Entry, envelope.PublicKey, envelope.KeyFingerprint) != nil {
		return false
	}
	data, err := json.Marshal(envelope)
	if err != nil {
		return false
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, d.url, bytes.NewReader(data))
	if err != nil {
		return false
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", envelope.Entry.Hash)
	response, err := d.client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1024))
	return response.StatusCode >= 200 && response.StatusCode < 300
}
