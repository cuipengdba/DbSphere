package security

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
)

type AuditEntry struct {
	Sequence     int64           `json:"sequence"`
	PreviousHash string          `json:"previousHash"`
	Hash         string          `json:"hash"`
	Signature    string          `json:"signature"`
	Record       json.RawMessage `json:"record"`
}

type AuditExport struct {
	SchemaVersion  int          `json:"schemaVersion"`
	OrganizationID string       `json:"organizationId"`
	PublicKey      string       `json:"publicKey"`
	KeyFingerprint string       `json:"keyFingerprint"`
	HeadSequence   int64        `json:"headSequence"`
	HeadHash       string       `json:"headHash"`
	HeadSignature  string       `json:"headSignature"`
	Entries        []AuditEntry `json:"entries"`
}

type auditMessage struct {
	Domain         string `json:"domain"`
	OrganizationID string `json:"organizationId"`
	Sequence       int64  `json:"sequence"`
	PreviousHash   string `json:"previousHash"`
	Record         []byte `json:"record"`
}

func auditPrivateKey(master []byte) ed25519.PrivateKey {
	seed := hmac.New(sha256.New, master)
	seed.Write([]byte("dbsphere:organization-audit:v1"))
	return ed25519.NewKeyFromSeed(seed.Sum(nil))
}

func auditFingerprint(pub []byte) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])
}

func auditEntryHash(org string, seq int64, prev string, record []byte) [32]byte {
	var canonical bytes.Buffer
	if json.Compact(&canonical, record) == nil {
		record = canonical.Bytes()
	}
	value, _ := json.Marshal(auditMessage{"dbsphere.audit.entry.v1", org, seq, prev, record})
	return sha256.Sum256(value)
}

func auditHeadHash(org string, seq int64, hash string) [32]byte {
	value, _ := json.Marshal(auditMessage{Domain: "dbsphere.audit.head.v1", OrganizationID: org, Sequence: seq, PreviousHash: hash})
	return sha256.Sum256(value)
}

func SignAuditEntry(master []byte, org string, sequence int64, previousHash string, record []byte) AuditEntry {
	sum := auditEntryHash(org, sequence, previousHash, record)
	return AuditEntry{Sequence: sequence, PreviousHash: previousHash, Hash: hex.EncodeToString(sum[:]),
		Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(auditPrivateKey(master), sum[:])), Record: record}
}

func BuildAuditExport(master []byte, org string, sequence int64, hash string, items []AuditEntry) AuditExport {
	key := auditPrivateKey(master)
	public := key.Public().(ed25519.PublicKey)
	head := auditHeadHash(org, sequence, hash)
	return AuditExport{SchemaVersion: 1, OrganizationID: org, PublicKey: base64.RawURLEncoding.EncodeToString(public),
		KeyFingerprint: auditFingerprint(public), HeadSequence: sequence, HeadHash: hash,
		HeadSignature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, head[:])), Entries: items}
}

func VerifyAuditExport(export AuditExport, fingerprint string) error {
	if export.SchemaVersion != 1 || export.OrganizationID == "" || export.HeadSequence != int64(len(export.Entries)) {
		return errors.New("audit manifest does not match the number of entries")
	}
	public, err := base64.RawURLEncoding.DecodeString(export.PublicKey)
	if err != nil || len(public) != ed25519.PublicKeySize ||
		export.KeyFingerprint != auditFingerprint(public) || fingerprint != "" && fingerprint != export.KeyFingerprint {
		return errors.New("audit key fingerprint mismatch")
	}
	previous := ""
	for index, entry := range export.Entries {
		sum := auditEntryHash(export.OrganizationID, int64(index+1), previous, entry.Record)
		sig, err := base64.RawURLEncoding.DecodeString(entry.Signature)
		if err != nil || !json.Valid(entry.Record) || entry.Sequence != int64(index+1) ||
			entry.PreviousHash != previous || entry.Hash != hex.EncodeToString(sum[:]) ||
			!ed25519.Verify(ed25519.PublicKey(public), sum[:], sig) {
			return errors.New("audit entry sequence, hash or signature mismatch")
		}
		previous = entry.Hash
	}
	head := auditHeadHash(export.OrganizationID, export.HeadSequence, export.HeadHash)
	signature, err := base64.RawURLEncoding.DecodeString(export.HeadSignature)
	if err != nil || previous != export.HeadHash || !ed25519.Verify(ed25519.PublicKey(public), head[:], signature) {
		return errors.New("audit head signature mismatch")
	}
	return nil
}
