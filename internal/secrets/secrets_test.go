package secrets

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func TestPanelTokenCiphertextIsAuthenticatedAndDoesNotExposePlaintext(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	token := []byte("test-panel-secret-do-not-log")
	ciphertext, err := Seal(key, token)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, token) {
		t.Fatal("ciphertext contains plaintext token")
	}
	opened, err := Open(key, ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(opened, token) {
		t.Fatal("decrypted token differs")
	}
	ciphertext[len(ciphertext)-1] ^= 1
	if _, err = Open(key, ciphertext); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
	if _, err = ParseKey(base64.StdEncoding.EncodeToString(key)); err != nil {
		t.Fatal(err)
	}
	if _, err = ParseKey(strings.Repeat("x", 8)); err == nil {
		t.Fatal("invalid encryption key accepted")
	}
}
