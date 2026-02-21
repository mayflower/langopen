package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const deploymentVarsEncryptionKeyEnv = "DEPLOYMENT_VARS_ENCRYPTION_KEY"

type AESGCM struct {
	key []byte
}

func IsDevEnv() bool {
	env := strings.TrimSpace(strings.ToLower(os.Getenv("LANGOPEN_ENV")))
	switch env {
	case "", "dev", "development", "local", "test":
		return true
	default:
		return false
	}
}

func LoadDeploymentVarsKeyFromEnv() ([]byte, error) {
	raw := strings.TrimSpace(os.Getenv(deploymentVarsEncryptionKeyEnv))
	if raw == "" {
		if IsDevEnv() {
			sum := sha256.Sum256([]byte("langopen-dev-deployment-runtime-vars"))
			return sum[:], nil
		}
		return nil, fmt.Errorf("%s is required unless LANGOPEN_ENV=dev", deploymentVarsEncryptionKeyEnv)
	}
	decoded, err := decodeBase64(raw)
	if err != nil {
		return nil, fmt.Errorf("%s must be base64-encoded 32 bytes: %w", deploymentVarsEncryptionKeyEnv, err)
	}
	if len(decoded) != 32 {
		return nil, fmt.Errorf("%s must decode to 32 bytes, got %d", deploymentVarsEncryptionKeyEnv, len(decoded))
	}
	return decoded, nil
}

func NewAESGCM(key []byte) (*AESGCM, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("invalid key length %d: expected 32", len(key))
	}
	k := make([]byte, len(key))
	copy(k, key)
	return &AESGCM{key: k}, nil
}

func NewAESGCMFromEnv() (*AESGCM, error) {
	key, err := LoadDeploymentVarsKeyFromEnv()
	if err != nil {
		return nil, err
	}
	return NewAESGCM(key)
}

func (e *AESGCM) EncryptString(plaintext string) (string, error) {
	if e == nil {
		return "", errors.New("nil cipher")
	}
	block, err := aes.NewCipher(e.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ciphertext := gcm.Seal(nil, nonce, []byte(plaintext), nil)
	packed := append(nonce, ciphertext...)
	return base64.RawStdEncoding.EncodeToString(packed), nil
}

func (e *AESGCM) DecryptString(encoded string) (string, error) {
	if e == nil {
		return "", errors.New("nil cipher")
	}
	packed, err := decodeBase64(strings.TrimSpace(encoded))
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(e.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(packed) < gcm.NonceSize() {
		return "", errors.New("ciphertext too short")
	}
	nonce := packed[:gcm.NonceSize()]
	ciphertext := packed[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func decodeBase64(input string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(input)
	if err == nil {
		return decoded, nil
	}
	return base64.RawStdEncoding.DecodeString(input)
}
