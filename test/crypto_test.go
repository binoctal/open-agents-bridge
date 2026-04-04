package test

import (
	"testing"

	"github.com/open-agents/open-agents-bridge/internal/crypto"
)

func TestGenerateKeyPair(t *testing.T) {
	kp, err := crypto.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair failed: %v", err)
	}

	if len(kp.PublicKey) != crypto.KeySize {
		t.Errorf("PublicKey size = %d, want %d", len(kp.PublicKey), crypto.KeySize)
	}

	if len(kp.PrivateKey) != crypto.KeySize {
		t.Errorf("PrivateKey size = %d, want %d", len(kp.PrivateKey), crypto.KeySize)
	}
}

func TestEncryptDecrypt(t *testing.T) {
	kp, _ := crypto.GenerateKeyPair()
	
	plaintext := []byte("Hello, World!")
	
	encrypted, err := kp.Encrypt(plaintext, nil)
	if err != nil {
		t.Fatalf("Encrypt failed: %v", err)
	}

	if len(encrypted) <= len(plaintext) {
		t.Error("Encrypted data should be longer than plaintext")
	}

	decrypted, err := kp.Decrypt(encrypted, nil)
	if err != nil {
		t.Fatalf("Decrypt failed: %v", err)
	}

	if string(decrypted) != string(plaintext) {
		t.Errorf("Decrypted = %s, want %s", decrypted, plaintext)
	}
}

func TestPublicKeyBase64(t *testing.T) {
	kp, _ := crypto.GenerateKeyPair()
	
	b64 := kp.PublicKeyBase64()
	if len(b64) == 0 {
		t.Error("PublicKeyBase64 returned empty string")
	}

	parsed, err := crypto.PublicKeyFromBase64(b64)
	if err != nil {
		t.Fatalf("PublicKeyFromBase64 failed: %v", err)
	}

	if *parsed != kp.PublicKey {
		t.Error("Parsed key does not match original")
	}
}

func TestPublicKeyFromBase64Invalid(t *testing.T) {
	_, err := crypto.PublicKeyFromBase64("invalid")
	if err == nil {
		t.Error("Expected error for invalid base64")
	}

	_, err = crypto.PublicKeyFromBase64("dG9vc2hvcnQ=") // "tooshort"
	if err == nil {
		t.Error("Expected error for wrong size key")
	}
}
