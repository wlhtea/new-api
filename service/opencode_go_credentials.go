package service

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"golang.org/x/crypto/hkdf"
)

type OpenCodeGoCredentialKind string

const (
	OpenCodeGoCredentialAuthCookie OpenCodeGoCredentialKind = "auth_cookie"
	OpenCodeGoCredentialAPIKey     OpenCodeGoCredentialKind = "api_key"

	openCodeGoCredentialEnvelopePrefix  = "ocg:v1:"
	openCodeGoCredentialHKDFSalt        = "new-api/opencode-go/credentials/v1"
	openCodeGoCredentialEncryptionInfo  = "aes-256-gcm"
	openCodeGoCredentialFingerprintInfo = "hmac-sha256"
)

var ErrOpenCodeGoCryptoSecretRequired = errors.New("OpenCode Go account credentials require an explicitly configured CRYPTO_SECRET")

type OpenCodeGoCredentialCodec struct {
	aead           cipher.AEAD
	fingerprintKey [sha256.Size]byte
}

func NewConfiguredOpenCodeGoCredentialCodec() (*OpenCodeGoCredentialCodec, error) {
	if !common.CryptoSecretExplicitlyConfigured {
		return nil, ErrOpenCodeGoCryptoSecretRequired
	}
	return NewOpenCodeGoCredentialCodec(common.CryptoSecret)
}

func NewOpenCodeGoCredentialCodec(secret string) (*OpenCodeGoCredentialCodec, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, ErrOpenCodeGoCryptoSecretRequired
	}

	encryptionKey, err := deriveOpenCodeGoCredentialKey(secret, openCodeGoCredentialEncryptionInfo)
	if err != nil {
		return nil, err
	}
	fingerprintKey, err := deriveOpenCodeGoCredentialKey(secret, openCodeGoCredentialFingerprintInfo)
	if err != nil {
		return nil, err
	}

	block, err := aes.NewCipher(encryptionKey[:])
	if err != nil {
		return nil, fmt.Errorf("initialize OpenCode Go credential cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initialize OpenCode Go credential AEAD: %w", err)
	}

	return &OpenCodeGoCredentialCodec{
		aead:           aead,
		fingerprintKey: fingerprintKey,
	}, nil
}

func deriveOpenCodeGoCredentialKey(secret string, info string) ([sha256.Size]byte, error) {
	var key [sha256.Size]byte
	reader := hkdf.New(sha256.New, []byte(secret), []byte(openCodeGoCredentialHKDFSalt), []byte(info))
	if _, err := io.ReadFull(reader, key[:]); err != nil {
		return key, fmt.Errorf("derive OpenCode Go credential key: %w", err)
	}
	return key, nil
}

func (codec *OpenCodeGoCredentialCodec) Encrypt(
	kind OpenCodeGoCredentialKind,
	channelID int,
	rowUID string,
	plaintext string,
) (string, error) {
	aad, err := openCodeGoCredentialAAD(kind, channelID, rowUID)
	if err != nil {
		return "", err
	}
	if plaintext == "" {
		return "", errors.New("OpenCode Go credential is empty")
	}

	nonce := make([]byte, codec.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate OpenCode Go credential nonce: %w", err)
	}
	sealed := codec.aead.Seal(nil, nonce, []byte(plaintext), aad)
	payload := make([]byte, 0, len(nonce)+len(sealed))
	payload = append(payload, nonce...)
	payload = append(payload, sealed...)
	return openCodeGoCredentialEnvelopePrefix + base64.RawURLEncoding.EncodeToString(payload), nil
}

func (codec *OpenCodeGoCredentialCodec) Decrypt(
	kind OpenCodeGoCredentialKind,
	channelID int,
	rowUID string,
	envelope string,
) (string, error) {
	aad, err := openCodeGoCredentialAAD(kind, channelID, rowUID)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(envelope, openCodeGoCredentialEnvelopePrefix) {
		return "", errors.New("unsupported OpenCode Go credential envelope")
	}

	encodedPayload := strings.TrimPrefix(envelope, openCodeGoCredentialEnvelopePrefix)
	payload, err := base64.RawURLEncoding.DecodeString(encodedPayload)
	if err != nil {
		return "", errors.New("invalid OpenCode Go credential envelope")
	}
	if base64.RawURLEncoding.EncodeToString(payload) != encodedPayload {
		return "", errors.New("invalid OpenCode Go credential envelope")
	}
	nonceSize := codec.aead.NonceSize()
	if len(payload) < nonceSize+codec.aead.Overhead() {
		return "", errors.New("invalid OpenCode Go credential envelope")
	}

	plaintext, err := codec.aead.Open(nil, payload[:nonceSize], payload[nonceSize:], aad)
	if err != nil {
		return "", errors.New("OpenCode Go credential authentication failed")
	}
	return string(plaintext), nil
}

func (codec *OpenCodeGoCredentialCodec) Fingerprint(
	kind OpenCodeGoCredentialKind,
	credential string,
) (string, error) {
	if !isOpenCodeGoCredentialKind(kind) {
		return "", fmt.Errorf("unsupported OpenCode Go credential kind %q", kind)
	}
	if credential == "" {
		return "", errors.New("OpenCode Go credential is empty")
	}

	hash := hmac.New(sha256.New, codec.fingerprintKey[:])
	hash.Write([]byte(openCodeGoCredentialEnvelopePrefix))
	hash.Write([]byte{0})
	hash.Write([]byte(kind))
	hash.Write([]byte{0})
	hash.Write([]byte(credential))
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func openCodeGoCredentialAAD(kind OpenCodeGoCredentialKind, channelID int, rowUID string) ([]byte, error) {
	if !isOpenCodeGoCredentialKind(kind) {
		return nil, fmt.Errorf("unsupported OpenCode Go credential kind %q", kind)
	}
	if channelID <= 0 {
		return nil, errors.New("OpenCode Go credential channel ID is invalid")
	}
	if strings.TrimSpace(rowUID) == "" {
		return nil, errors.New("OpenCode Go credential row UID is empty")
	}

	aad := strings.Join([]string{
		openCodeGoCredentialEnvelopePrefix,
		string(kind),
		strconv.Itoa(channelID),
		rowUID,
	}, "\x00")
	return []byte(aad), nil
}

func isOpenCodeGoCredentialKind(kind OpenCodeGoCredentialKind) bool {
	return kind == OpenCodeGoCredentialAuthCookie || kind == OpenCodeGoCredentialAPIKey
}
