package crypto

import (
	"encoding/base64"
	"testing"
)

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	c, err := NewAESGCM(key)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := c.EncryptString("hello")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := c.DecryptString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != "hello" {
		t.Fatalf("expected hello, got %q", decoded)
	}
}

func TestLoadDeploymentVarsKeyFromEnv(t *testing.T) {
	t.Setenv("LANGOPEN_ENV", "prod")
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = byte(200 + i)
	}
	t.Setenv("DEPLOYMENT_VARS_ENCRYPTION_KEY", base64.RawStdEncoding.EncodeToString(raw))
	key, err := LoadDeploymentVarsKeyFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != 32 {
		t.Fatalf("expected 32-byte key, got %d", len(key))
	}
}
