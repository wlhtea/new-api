package common

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

const openCodeGoReferenceDomain = "new-api/opencode-go/cache-identity/v1"

func GenerateHMACWithKey(key []byte, data string) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}

func GenerateHMAC(data string) string {
	h := hmac.New(sha256.New, []byte(CryptoSecret))
	h.Write([]byte(data))
	return hex.EncodeToString(h.Sum(nil))
}

// OpenCodeGoDiagnosticRef returns a stable, non-reversible reference suitable
// for correlating sensitive OpenCode Go identifiers in process logs.
func OpenCodeGoDiagnosticRef(source string, value string) string {
	h := hmac.New(sha256.New, []byte(CryptoSecret))
	_, _ = fmt.Fprintf(h, "%s\x00%s\x00%s", openCodeGoReferenceDomain, source, value)
	return "ocg_" + base64.RawURLEncoding.EncodeToString(h.Sum(nil)[:16])
}

func Password2Hash(password string) (string, error) {
	passwordBytes := []byte(password)
	hashedPassword, err := bcrypt.GenerateFromPassword(passwordBytes, bcrypt.DefaultCost)
	return string(hashedPassword), err
}

func ValidatePasswordAndHash(password string, hash string) bool {
	err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
	return err == nil
}
