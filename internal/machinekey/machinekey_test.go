package machinekey

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"
)

func TestGenerateAndSignRoundTrip(t *testing.T) {
	privB64, pubB64, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	priv, err := ParsePrivate(privB64)
	if err != nil {
		t.Fatalf("ParsePrivate: %v", err)
	}

	pub, err := base64Decode(pubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		t.Fatalf("public key shape: %d bytes, err=%v", len(pub), err)
	}

	// The signature must verify against the message format the server
	// (apps/api/src/lib/machine-key.ts) checks: nonce|machineId|instanceId.
	sig := Sign(priv, "nonce1", "machine_abc", "inst_9")
	sigBytes, err := base64Decode(sig)
	if err != nil || len(sigBytes) != 64 {
		t.Fatalf("signature shape: %d bytes, err=%v", len(sigBytes), err)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), ChallengeMessage("nonce1", "machine_abc", "inst_9"), sigBytes) {
		t.Fatal("signature does not verify over nonce|machineId|instanceId")
	}

	// A different instance id is a different message — D7's replay scoping.
	if ed25519.Verify(ed25519.PublicKey(pub), ChallengeMessage("nonce1", "machine_abc", "inst_8"), sigBytes) {
		t.Fatal("signature verified for a different instanceId")
	}
}

func TestParsePrivateRejectsGarbage(t *testing.T) {
	if _, err := ParsePrivate("not base64!!"); err == nil {
		t.Fatal("expected error for non-base64 input")
	}
	// 32 bytes (a public key) is the wrong length for a private key.
	if _, err := ParsePrivate("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="); err == nil {
		t.Fatal("expected error for 32-byte input")
	}
}

func base64Decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}
