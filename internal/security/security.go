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
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	payload, err := json.Marshal(struct {
		Expires int64  `json:"exp"`
		Nonce   string `json:"nonce"`
	}{Expires: now.Add(12 * time.Hour).UnixMilli(), Nonce: hex.EncodeToString(nonce)})
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return encoded + "." + signature(key, encoded), nil
}

func VerifySession(key []byte, token string, now time.Time) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 2 || len(parts[0]) == 0 || len(parts[1]) != 43 || len(token) > 1024 {
		return false
	}
	want := signature(key, parts[0])
	if subtle.ConstantTimeCompare([]byte(want), []byte(parts[1])) != 1 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	var session struct {
		Expires int64 `json:"exp"`
	}
	return json.Unmarshal(payload, &session) == nil && session.Expires > now.UnixMilli()
}

func CSRFToken(key []byte, session string) string {
	hash := sha256.Sum256([]byte(signature(key, "csrf:"+session)))
	return hex.EncodeToString(hash[:])
}
