package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

const (
	KeySize   = 32
	NonceSize = 12
)

// KeyPair holds a symmetric key for E2EE (simplified)
type KeyPair struct {
	PublicKey  [KeySize]byte // Used as identifier
	PrivateKey [KeySize]byte // Shared secret
}

// GenerateKeyPair creates a new key pair
func GenerateKeyPair() (*KeyPair, error) {
	kp := &KeyPair{}
	if _, err := rand.Read(kp.PrivateKey[:]); err != nil {
		return nil, err
	}
	// Public key is hash of private key
	hash := sha256.Sum256(kp.PrivateKey[:])
	kp.PublicKey = hash
	return kp, nil
}

// Encrypt encrypts a message using AES-GCM
func (kp *KeyPair) Encrypt(message []byte, _ *[KeySize]byte) ([]byte, error) {
	block, err := aes.NewCipher(kp.PrivateKey[:])
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := make([]byte, NonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}

	ciphertext := gcm.Seal(nonce, nonce, message, nil)
	return ciphertext, nil
}

// Decrypt decrypts a message using AES-GCM
func (kp *KeyPair) Decrypt(encrypted []byte, _ *[KeySize]byte) ([]byte, error) {
	if len(encrypted) < NonceSize {
		return nil, errors.New("ciphertext too short")
	}

	block, err := aes.NewCipher(kp.PrivateKey[:])
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	nonce := encrypted[:NonceSize]
	ciphertext := encrypted[NonceSize:]

	return gcm.Open(nil, nonce, ciphertext, nil)
}

// PublicKeyBase64 returns the public key as base64 string
func (kp *KeyPair) PublicKeyBase64() string {
	return base64.StdEncoding.EncodeToString(kp.PublicKey[:])
}

// PublicKeyFromBase64 parses a base64 public key
func PublicKeyFromBase64(s string) (*[KeySize]byte, error) {
	data, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	if len(data) != KeySize {
		return nil, errors.New("invalid key size")
	}
	var key [KeySize]byte
	copy(key[:], data)
	return &key, nil
}
