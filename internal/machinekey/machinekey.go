// Package machinekey implements the machine-bound signing key
// (bridge-abuse-hardening 4b, design D7).
//
// The machine token is a bearer secret: copying config.json copies the
// identity. The Ed25519 pair generated at pairing time does not make a
// compromised host honest, but it removes the token's standalone value —
// every WS upgrade must sign a server-issued one-time nonce with a key the
// server has never seen in private form.
//
// The private key is stored in the same 0600 config file as the token (the
// static build cannot link an OS keychain); a full-file copy therefore
// clones both — the dashboard's "reset machine key" is the recovery cut-off
// for that scenario.
package machinekey

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
)

// Generate returns a fresh key pair. The public half is the base64 raw form
// the server binds at pairing; the private half is the base64 64-byte form
// persisted in config.json.
func Generate() (privB64, pubB64 string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("generate machine key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(priv), base64.StdEncoding.EncodeToString(pub), nil
}

// ParsePrivate decodes a stored private key.
func ParsePrivate(privB64 string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(privB64))
	if err != nil {
		return nil, fmt.Errorf("decode machine key: %w", err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("machine key is %d bytes, want %d", len(raw), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(raw), nil
}

// ChallengeMessage is the signed payload. It must match the server's
// machine-key.ts byte for byte: nonce|machineId|instanceId.
func ChallengeMessage(nonce, machineID, instanceID string) []byte {
	return []byte(nonce + "|" + machineID + "|" + instanceID)
}

// Sign produces the base64 signature sent on the WS upgrade headers.
func Sign(priv ed25519.PrivateKey, nonce, machineID, instanceID string) string {
	sig := ed25519.Sign(priv, ChallengeMessage(nonce, machineID, instanceID))
	return base64.StdEncoding.EncodeToString(sig)
}
