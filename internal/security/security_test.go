package security

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestNodePasswordAndSecretCompatibility(t *testing.T) {
	stored := "000102030405060708090a0b0c0d0e0f:82cd5015b8d8987f370dae5b0df403b3cf1b4f3671c6adc27b3351d0bb53dc59"
	if !VerifyPassword("migration-password", stored) || VerifyPassword("wrong", stored) {
		t.Fatal("Node scrypt password format is incompatible")
	}
	if VerifyPassword("migration-password", "malformed") {
		t.Fatal("malformed password hash was accepted")
	}
	key := bytes.Repeat([]byte{7}, 32)
	plain, err := Decrypt(key, "CQkJCQkJCQkJCQkJd1W2U51eMLFHSwU5rfQtbFfiqefbk7ME1A==")
	if err != nil || plain != "pg-secret" {
		t.Fatalf("Node AES-GCM secret is incompatible: %q %v", plain, err)
	}
	encoded, err := Encrypt(key, "new-secret")
	if err != nil {
		t.Fatal(err)
	}
	plain, err = Decrypt(key, encoded)
	if err != nil || plain != "new-secret" {
		t.Fatal("secret round trip failed")
	}
}

func TestSessionExpirySignatureAndCSRF(t *testing.T) {
	key := bytes.Repeat([]byte{3}, 32)
	now := time.Now().UTC()
	token, err := CreateSession(key, now)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifySession(key, token, now) || VerifySession(key, token, now.Add(13*time.Hour)) ||
		VerifySession(key, token+"x", now) || VerifySession(bytes.Repeat([]byte{4}, 32), token, now) {
		t.Fatal("session authentication or expiration failed")
	}
	if len(CSRFToken(key, token)) != 64 || strings.Contains(CSRFToken(key, token), "+") {
		t.Fatal("CSRF format is invalid")
	}
}

func TestViewerSessionCannotBeInterpretedAsAdmin(t *testing.T) {
	key := bytes.Repeat([]byte{5}, 32)
	now := time.Now().UTC()
	viewerKey := ViewerSessionKey(key, "viewer-hash-one")
	viewer, err := CreateViewerSession(viewerKey, now)
	if err != nil {
		t.Fatal(err)
	}
	role, valid := SessionRole(viewerKey, viewer, now)
	if !valid || role != "viewer" || VerifySession(key, viewer, now) {
		t.Fatal("viewer session has wrong role or accepted administrator key")
	}
	if _, valid := SessionRole(ViewerSessionKey(key, "viewer-hash-two"), viewer, now); valid {
		t.Fatal("old viewer session survived password rotation")
	}
	legacy, err := CreateSession(key, now)
	if err != nil {
		t.Fatal(err)
	}
	role, valid = SessionRole(key, legacy, now)
	if !valid || role != "admin" {
		t.Fatal("administrator session lost compatibility")
	}
	if role, valid := SessionRole(viewerKey, viewer+"x", now); valid || role != "" {
		t.Fatal("tampered viewer session accepted")
	}
}

func TestAdminSessionInvalidatedByPasswordRotation(t *testing.T) {
	master := bytes.Repeat([]byte{9}, 32)
	now := time.Now().UTC()
	oldKey := AdminSessionKey(master, "old-password-hash")
	token, err := CreateSession(oldKey, now)
	if err != nil {
		t.Fatal(err)
	}
	if !VerifySession(oldKey, token, now) || VerifySession(AdminSessionKey(master, "new-password-hash"), token, now) ||
		VerifySession(master, token, now) {
		t.Fatal("administrator session survived password rotation or used the asset master key directly")
	}
}

func TestApproverSessionIsRoleBoundAndRevokedOnRotation(t *testing.T) {
	master := bytes.Repeat([]byte{11}, 32)
	now := time.Now().UTC()
	key := ApproverSessionKey(master, "first-hash")
	token, err := CreateApproverSession(key, now)
	if err != nil {
		t.Fatal(err)
	}
	if role, ok := SessionRole(key, token, now); !ok || role != "approver" {
		t.Fatal("approver role was not verified")
	}
	if _, ok := SessionRole(AdminSessionKey(master, "first-hash"), token, now); ok {
		t.Fatal("approver session was accepted as administrator")
	}
	if _, ok := SessionRole(ApproverSessionKey(master, "second-hash"), token, now); ok {
		t.Fatal("approver session survived credential rotation")
	}
}

func TestNodeSessionCompatibility(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	token := "eyJleHAiOjk5OTk5OTk5OTk5OTksIm5vbmNlIjoiMDAwMTAyMDMwNDA1MDYwNzA4MDkwYTBiMGMwZDBlMGYifQ.Y2xRmFlsu57CkZsdF4CpJrytLZT-KR3qDiPFGN4hzBA"
	if !VerifySession(key, token, time.Now()) ||
		CSRFToken(key, token) != "a8d30d578dbceaa7ece3167d1392f1f3b0306e589c7ebdd1034040de8622495a" {
		t.Fatal("historical Node session and CSRF token are incompatible")
	}
}
