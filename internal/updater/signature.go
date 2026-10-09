package updater

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// SignatureAsset is the detached signature published next to checksums.txt:
// the base64 of the raw Ed25519 signature over the exact bytes of
// checksums.txt (bridge-abuse-hardening 5e.2).
const SignatureAsset = "checksums.txt.sig"

// trustedKeys is the list of release signing public keys (base64, raw 32
// bytes). It is a list so a key can be rotated: ship a release signed by an
// old key that carries the new list, then retire the old key. See
// docs/release-signing.md. npm/install.js embeds the same list; keep them in
// sync.
var trustedKeys = []string{
	"NTmWyMB+MN8QmQ+R0TXJ3t3cwuqki1Cxld9HzHzc4Vw=",
}

// requireSignature is the transitional policy switch. While true (the
// default) a release without a valid checksums.txt.sig is refused. It is a
// string only so it can be flipped with -X at build time.
var requireSignature = "true"

// RequireSignature reports whether unsigned releases are refused.
func RequireSignature() bool { return requireSignature != "false" }

// ErrNoSignature means the release shipped no checksums.txt.sig.
var ErrNoSignature = errors.New("release has no " + SignatureAsset)

func parseKeys(keys []string) []ed25519.PublicKey {
	var out []ed25519.PublicKey
	for _, k := range keys {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(k))
		if err == nil && len(raw) == ed25519.PublicKeySize {
			out = append(out, ed25519.PublicKey(raw))
		}
	}
	return out
}

// verifySignature checks sigB64 over checksums against any of keys.
func verifySignature(checksums, sigB64 []byte, keys []string) error {
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigB64)))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("malformed %s", SignatureAsset)
	}
	for _, pub := range parseKeys(keys) {
		if ed25519.Verify(pub, checksums, sig) {
			return nil
		}
	}
	return fmt.Errorf("%s does not verify against any trusted release key", SignatureAsset)
}
