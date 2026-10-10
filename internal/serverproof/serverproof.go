// Package serverproof verifies the control plane's identity before the bridge
// sends it any credential (bridge-abuse-hardening 5e.1, bridge half).
//
// The bridge posts a fresh random challenge to POST {server}/api/server-proof
// and the server answers with an Ed25519 signature over
//
//	"oa-server-proof-v1|" + challenge + "|" + host
//
// where host is the lowercase hostname (no port) the bridge dialed. The
// signature is checked against the embedded trusted server keys, so an
// impostor that merely holds a look-alike domain cannot produce it, and a
// proof captured for one host or challenge cannot be replayed for another.
// See docs/server-identity.md for what this does and does not prove.
package serverproof

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/binoctal/open-agents-bridge/internal/config"
)

// ProofPath is the unauthenticated endpoint that signs a challenge.
const ProofPath = "/api/server-proof"

const messagePrefix = "oa-server-proof-v1|"

// TrustedKey is one trusted server signing public key (base64, raw 32 bytes).
type TrustedKey struct {
	ID  string
	Key string
}

// trustedKeys is the list of server proof public keys. It is a list so a key
// can be rotated (see docs/server-identity.md); a proof is accepted when it
// verifies against ANY entry. Tests replace it.
var trustedKeys = []TrustedKey{
	{ID: "staging", Key: "YQ37ox3O0kSPtYtIfXuEexn8IKsUDg9klyrk7AXxTr8="},
	{ID: "production", Key: "9gFMg60eboQSvG/VvStAUxRVq6ZH/9H2uwKqPDnreqU="},
}

// requireServerProof is the policy switch for official builds. While not
// "false" (the default) an official build refuses a server that cannot prove
// its identity. It is a string only so it can be flipped with -X at build
// time, like updater.requireSignature.
var requireServerProof = "true"

// RequireServerProof reports whether the build policy demands a valid proof.
func RequireServerProof() bool { return requireServerProof != "false" }

// httpTimeout keeps the check from holding up start-up or pairing for long.
const httpTimeout = 8 * time.Second

var httpClient = &http.Client{Timeout: httpTimeout}

// isOfficial is a seam for tests.
var isOfficial = config.IsOfficialBuild

var (
	modeMu sync.Mutex
	// unsafeServer is set by --unsafe-server: the operator knowingly targets
	// their own control plane, so a failed proof only warns.
	unsafeServer bool
	// lenient marks runs that are never held to the official-build policy
	// (cloud session containers, whose URL comes from the platform env).
	lenient  bool
	verified = map[string]bool{}
)

// SetUnsafe records the --unsafe-server flag.
func SetUnsafe(v bool) { modeMu.Lock(); unsafeServer = v; modeMu.Unlock() }

// SetLenient marks the process as exempt from the official-build requirement
// (verification is still attempted and failures are warned about).
func SetLenient(v bool) { modeMu.Lock(); lenient = v; modeMu.Unlock() }

// Required reports whether a failed proof must stop the caller.
func Required() bool {
	modeMu.Lock()
	defer modeMu.Unlock()
	return isOfficial() && RequireServerProof() && !unsafeServer && !lenient
}

// resetForTest clears the per-process cache and modes.
func resetForTest() {
	modeMu.Lock()
	unsafeServer, lenient = false, false
	verified = map[string]bool{}
	modeMu.Unlock()
}

// HostOf returns the lowercase hostname (no port, no trailing dot) of raw.
func HostOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
}

// apiBase maps a ws(s)/http(s) server URL to the http(s) origin.
func apiBase(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid server URL %q", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "wss", "https":
		u.Scheme = "https"
	case "ws", "http":
		u.Scheme = "http"
	default:
		return "", fmt.Errorf("unsupported server URL scheme %q", u.Scheme)
	}
	return u.Scheme + "://" + u.Host, nil
}

// Message builds the exact signed byte string.
func Message(challenge, host string) []byte {
	return []byte(messagePrefix + challenge + "|" + host)
}

// verifyProof checks proofB64 over (challenge, host) against any trusted key
// and returns the matching key id.
func verifyProof(proofB64, challenge, host string) (string, error) {
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(proofB64))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return "", errors.New("malformed server proof")
	}
	msg := Message(challenge, host)
	for _, k := range trustedKeys {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(k.Key))
		if err != nil || len(raw) != ed25519.PublicKeySize {
			continue
		}
		if ed25519.Verify(ed25519.PublicKey(raw), msg, sig) {
			return k.ID, nil
		}
	}
	return "", errors.New("server proof does not verify against any trusted server key")
}

// Verify performs the challenge round trip against serverURL. It sends NO
// credential: the request is unauthenticated and carries only the challenge.
func Verify(serverURL string) error {
	base, err := apiBase(serverURL)
	if err != nil {
		return err
	}
	host := HostOf(serverURL)
	if host == "" {
		return fmt.Errorf("invalid server URL %q", serverURL)
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("challenge: %v", err)
	}
	challenge := base64.StdEncoding.EncodeToString(raw)

	body, _ := json.Marshal(map[string]string{"challenge": challenge})
	req, err := http.NewRequest(http.MethodPost, base+ProofPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("server proof request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server proof endpoint answered HTTP %d (server does not prove its identity)", resp.StatusCode)
	}
	var out struct {
		Proof string `json:"proof"`
		KeyID string `json:"keyId"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return fmt.Errorf("server proof response is not valid JSON: %v", err)
	}
	if out.Proof == "" {
		return errors.New("server proof missing from response")
	}
	if _, err := verifyProof(out.Proof, challenge, host); err != nil {
		return err
	}
	return nil
}

// Ensure makes sure serverURL has proven its identity in this process,
// caching success per URL. It returns a non-nil error only when the proof
// failed AND the policy requires it (official build, no --unsafe-server);
// otherwise a failure is reported through warn (may be nil) and Ensure
// returns nil. Callers must not send any credential when it returns an error.
func Ensure(serverURL string, warn func(format string, args ...interface{})) error {
	modeMu.Lock()
	ok := verified[serverURL]
	modeMu.Unlock()
	if ok {
		return nil
	}
	err := Verify(serverURL)
	if err == nil {
		modeMu.Lock()
		verified[serverURL] = true
		modeMu.Unlock()
		return nil
	}
	if Required() {
		return fmt.Errorf("cannot verify the identity of %s: %v", HostOf(serverURL), err)
	}
	if warn != nil {
		warn("WARNING: could not verify the identity of %s (%v); continuing because this is not an official build, --unsafe-server was given, or this is a cloud session", HostOf(serverURL), err)
	}
	return nil
}

// ForceOfficialForTest makes the policy treat the build as official (or not)
// and clears the verification cache; the returned func restores everything.
// For tests of callers in other packages only.
func ForceOfficialForTest(official bool) (restore func()) {
	modeMu.Lock()
	old := isOfficial
	isOfficial = func() bool { return official }
	modeMu.Unlock()
	resetForTest()
	return func() {
		modeMu.Lock()
		isOfficial = old
		modeMu.Unlock()
		resetForTest()
	}
}

// TrustKeyForTest replaces the trusted key list with one key; the returned
// func restores it. For tests of callers in other packages only.
func TrustKeyForTest(pub ed25519.PublicKey) (restore func()) {
	old := trustedKeys
	trustedKeys = []TrustedKey{{ID: "test", Key: base64.StdEncoding.EncodeToString(pub)}}
	return func() { trustedKeys = old }
}
