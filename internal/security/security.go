package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"golang.org/x/crypto/scrypt"
)

// Passwords and secrets use the formats written by the historical Node backend.
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hash, err := scrypt.Key([]byte(password), salt, 16384, 8, 1, 32)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(salt) + ":" + hex.EncodeToString(hash), nil
}

func VerifyPassword(password, stored string) bool {
	parts := strings.Split(stored, ":")
	if len(parts) != 2 || len(parts[0]) != 32 || len(parts[1]) != 64 {
		return false
	}
	salt, saltErr := hex.DecodeString(parts[0])
	want, hashErr := hex.DecodeString(parts[1])
	if saltErr != nil || hashErr != nil {
		return false
	}
	got, err := scrypt.Key([]byte(password), salt, 16384, 8, 1, 32)
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

func Encrypt(key []byte, value string) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	iv := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, iv, []byte(value), nil)
	tagStart := len(sealed) - gcm.Overhead()
	encoded := make([]byte, 0, len(iv)+len(sealed))
	encoded = append(encoded, iv...)
	encoded = append(encoded, sealed[tagStart:]...)
	encoded = append(encoded, sealed[:tagStart]...)
	return base64.StdEncoding.EncodeToString(encoded), nil
}

func Decrypt(key []byte, encoded string) (string, error) {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(data) < gcm.NonceSize()+gcm.Overhead() {
		return "", errors.New("encrypted secret is too short")
	}
	iv := data[:gcm.NonceSize()]
	tag := data[gcm.NonceSize() : gcm.NonceSize()+gcm.Overhead()]
	ciphertext := data[gcm.NonceSize()+gcm.Overhead():]
	sealed := make([]byte, 0, len(ciphertext)+len(tag))
	sealed = append(sealed, ciphertext...)
	sealed = append(sealed, tag...)
	plain, err := gcm.Open(nil, iv, sealed, nil)
	return string(plain), err
}

func signature(key []byte, payload string) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func CreateSession(key []byte, now time.Time) (string, error) {
	return createSession(key, now, "")
}

func CreateViewerSession(key []byte, now time.Time) (string, error) {
	return createSession(key, now, "viewer")
}

func CreateApproverSession(key []byte, now time.Time) (string, error) {
	return createSession(key, now, "approver")
}

// AdminSessionKey invalidates administrator cookies when the password hash
// changes while leaving the master key for encrypted assets unchanged.
func AdminSessionKey(master []byte, passwordHash string) []byte {
	mac := hmac.New(sha256.New, master)
	mac.Write([]byte("dbsphere-admin-session-v1:"))
	mac.Write([]byte(passwordHash))
	return mac.Sum(nil)
}

// ViewerSessionKey binds viewer sessions to the configured password hash so
// rotating or disabling the account invalidates its existing sessions.
func ViewerSessionKey(master []byte, passwordHash string) []byte {
	mac := hmac.New(sha256.New, master)
	mac.Write([]byte("dbsphere-viewer-session-v1:"))
	mac.Write([]byte(passwordHash))
	return mac.Sum(nil)
}

func ApproverSessionKey(master []byte, passwordHash string) []byte {
	mac := hmac.New(sha256.New, master)
	mac.Write([]byte("dbsphere-approver-session-v1:"))
	mac.Write([]byte(passwordHash))
	return mac.Sum(nil)
}

func createSession(key []byte, now time.Time, role string) (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	payload, err := json.Marshal(struct {
		Expires int64  `json:"exp"`
		Nonce   string `json:"nonce"`
		Role    string `json:"role,omitempty"`
	}{Expires: now.Add(12 * time.Hour).UnixMilli(), Nonce: hex.EncodeToString(nonce), Role: role})
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return encoded + "." + signature(key, encoded), nil
}

func VerifySession(key []byte, token string, now time.Time) bool {
	_, ok := SessionRole(key, token, now)
	return ok
}

func SessionRole(key []byte, token string, now time.Time) (string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 || len(parts[0]) == 0 || len(parts[1]) != 43 || len(token) > 1024 {
		return "", false
	}
	want := signature(key, parts[0])
	if subtle.ConstantTimeCompare([]byte(want), []byte(parts[1])) != 1 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	var session struct {
		Expires int64  `json:"exp"`
		Role    string `json:"role"`
	}
	if json.Unmarshal(payload, &session) != nil || session.Expires <= now.UnixMilli() {
		return "", false
	}
	if session.Role == "" {
		session.Role = "admin"
	}
	if session.Role != "admin" && session.Role != "viewer" && session.Role != "approver" {
		return "", false
	}
	return session.Role, true
}

func CSRFToken(key []byte, session string) string {
	hash := sha256.Sum256([]byte(signature(key, "csrf:"+session)))
	return hex.EncodeToString(hash[:])
}
