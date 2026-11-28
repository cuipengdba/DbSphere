package entitlement

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"regexp"
	"time"
)

const MaxFileSize = 16 << 10
const ClockTolerance = 5 * time.Minute
const domain = "DbSphere enterprise license v1\n"

var ErrInvalid = errors.New("invalid enterprise license or signature")
var deploymentID = regexp.MustCompile(`^[a-f0-9]{8}(?:-[a-f0-9]{4}){3}-[a-f0-9]{12}$`)

type License struct {
	Deployment string    `json:"deploymentId"`
	Sequence   int64     `json:"sequence"`
	IssuedAt   time.Time `json:"issuedAt"`
	StartsAt   time.Time `json:"startsAt"`
	ExpiresAt  time.Time `json:"expiresAt"`
	Assets     int       `json:"assetLimit"`
	Sites      int       `json:"siteLimit"`
}

type signedLicense struct {
	Version   int    `json:"version"`
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

func decode(data []byte, into any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(into) != nil || d.Decode(new(any)) != io.EOF {
		return ErrInvalid
	}
	return nil
}

func ParsePublicKey(data []byte) (ed25519.PublicKey, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PUBLIC KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, ErrInvalid
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	pub, ok := key.(ed25519.PublicKey)
	if err != nil || !ok || len(pub) != ed25519.PublicKeySize {
		return nil, ErrInvalid
	}
	return pub, nil
}

func ParsePrivateKey(data []byte) (ed25519.PrivateKey, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, ErrInvalid
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	priv, ok := key.(ed25519.PrivateKey)
	if err != nil || !ok || len(priv) != ed25519.PrivateKeySize {
		return nil, ErrInvalid
	}
	return priv, nil
}

func valid(l License) bool {
	return deploymentID.MatchString(l.Deployment) && l.Sequence > 0 && !l.IssuedAt.IsZero() &&
		!l.StartsAt.IsZero() && l.ExpiresAt.After(l.StartsAt) && l.ExpiresAt.After(l.IssuedAt) &&
		l.ExpiresAt.Sub(l.StartsAt) <= 5*366*24*time.Hour &&
		l.Assets >= 1 && l.Assets <= 1000000 && l.Sites >= 0 && l.Sites <= 10000
}

func Issue(data []byte, key ed25519.PrivateKey) ([]byte, error) {
	var license License
	if len(data) > MaxFileSize || len(key) != ed25519.PrivateKeySize || decode(data, &license) != nil || !valid(license) {
		return nil, ErrInvalid
	}
	canonical, err := json.Marshal(license)
	if err != nil {
		return nil, err
	}
	sig := ed25519.Sign(key, append([]byte(domain), canonical...))
	return json.Marshal(signedLicense{1, base64.StdEncoding.EncodeToString(canonical), base64.StdEncoding.EncodeToString(sig)})
}

func Verify(data []byte, key ed25519.PublicKey) (License, error) {
	var license License
	var file signedLicense
	if len(data) > MaxFileSize || len(key) != ed25519.PublicKeySize || decode(data, &file) != nil || file.Version != 1 {
		return license, ErrInvalid
	}
	payload, payloadErr := base64.StdEncoding.Strict().DecodeString(file.Payload)
	sig, sigErr := base64.StdEncoding.Strict().DecodeString(file.Signature)
	if payloadErr != nil || sigErr != nil || !ed25519.Verify(key, append([]byte(domain), payload...), sig) ||
		decode(payload, &license) != nil || !valid(license) {
		return License{}, ErrInvalid
	}
	return license, nil
}

func (l License) Status(now, latestUse time.Time) string {
	switch {
	case now.Add(ClockTolerance).Before(latestUse) || now.Add(ClockTolerance).Before(l.IssuedAt):
		return "clock_skew"
	case now.Before(l.StartsAt):
		return "pending"
	case !now.Before(l.ExpiresAt):
		return "expired"
	default:
		return "active"
	}
}
