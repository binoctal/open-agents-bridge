package serverproof

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type fakeServer struct {
	*httptest.Server
	proofCalls, otherCalls atomic.Int32
	authSeen               atomic.Bool
}

// newServer answers /api/server-proof with sign(challenge, host); sign may
// return "" to omit the proof.
func newServer(t *testing.T, sign func(challenge, host string) string) *fakeServer {
	t.Helper()
	f := &fakeServer{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.URL.Query().Get("token") != "" {
			f.authSeen.Store(true)
		}
		if r.URL.Path != ProofPath || r.Method != http.MethodPost {
			f.otherCalls.Add(1)
			http.NotFound(w, r)
			return
		}
		f.proofCalls.Add(1)
		var in struct{ Challenge string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		host := HostOf(f.URL)
		_ = json.NewEncoder(w).Encode(map[string]string{"proof": sign(in.Challenge, host), "keyId": "test"})
	}))
	t.Cleanup(f.Close)
	return f
}

func setup(t *testing.T, official bool) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	oldKeys, oldOff := trustedKeys, isOfficial
	trustedKeys = []TrustedKey{{ID: "test", Key: base64.StdEncoding.EncodeToString(pub)}}
	isOfficial = func() bool { return official }
	resetForTest()
	t.Cleanup(func() { trustedKeys, isOfficial = oldKeys, oldOff; resetForTest() })
	return pub, priv
}

func signer(priv ed25519.PrivateKey) func(c, h string) string {
	return func(c, h string) string {
		return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, Message(c, h)))
	}
}

func TestValidProofAccepted(t *testing.T) {
	_, priv := setup(t, true)
	s := newServer(t, signer(priv))
	if err := Verify(s.URL); err != nil {
		t.Fatalf("valid proof rejected: %v", err)
	}
	// ws:// form of the same URL is mapped to http.
	if err := Verify("ws" + strings.TrimPrefix(s.URL, "http")); err != nil {
		t.Fatalf("ws URL: %v", err)
	}
}

func TestWrongKeyRejected(t *testing.T) {
	setup(t, true)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	s := newServer(t, signer(other))
	if err := Verify(s.URL); err == nil {
		t.Fatal("proof from an untrusted key was accepted")
	}
}

func TestSecondTrustedKeyAccepted(t *testing.T) {
	pub, priv := setup(t, true)
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	trustedKeys = []TrustedKey{{ID: "old", Key: base64.StdEncoding.EncodeToString(other)}, {ID: "new", Key: base64.StdEncoding.EncodeToString(pub)}}
	s := newServer(t, signer(priv))
	if err := Verify(s.URL); err != nil {
		t.Fatalf("rotation list: %v", err)
	}
}

func TestProofForOtherHostRejected(t *testing.T) {
	_, priv := setup(t, true)
	s := newServer(t, func(c, h string) string { return signer(priv)(c, "evil.example") })
	if err := Verify(s.URL); err == nil {
		t.Fatal("proof signed for another host was accepted")
	}
}

func TestReplayedProofForOtherChallengeRejected(t *testing.T) {
	_, priv := setup(t, true)
	captured := signer(priv)("some-earlier-challenge", "127.0.0.1")
	s := newServer(t, func(c, h string) string { return captured })
	if err := Verify(s.URL); err == nil {
		t.Fatal("replayed proof for a different challenge was accepted")
	}
}

func TestMissingAndGarbageProofRejected(t *testing.T) {
	setup(t, true)
	for name, p := range map[string]string{"missing": "", "garbage": "!!!not-base64!!!", "short": base64.StdEncoding.EncodeToString([]byte("abc"))} {
		s := newServer(t, func(c, h string) string { return p })
		if err := Verify(s.URL); err == nil {
			t.Errorf("%s proof accepted", name)
		}
	}
}

func TestNon200AndNetworkErrorRejected(t *testing.T) {
	setup(t, true)
	s := httptest.NewServer(http.NotFoundHandler())
	if err := Verify(s.URL); err == nil {
		t.Fatal("404 accepted")
	}
	s.Close()
	if err := Verify(s.URL); err == nil {
		t.Fatal("network error accepted")
	}
}

func TestOfficialBuildRefusesAndNeverCaches(t *testing.T) {
	setup(t, true)
	s := newServer(t, func(c, h string) string { return "" })
	if err := Ensure(s.URL, nil); err == nil {
		t.Fatal("official build tolerated a missing proof")
	}
	if err := Ensure(s.URL, nil); err == nil || s.proofCalls.Load() != 2 {
		t.Fatalf("failure must not be cached (calls=%d)", s.proofCalls.Load())
	}
	// Network error on official build is also a refusal.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	if err := Ensure(dead.URL, nil); err == nil {
		t.Fatal("official build tolerated a network error")
	}
}

func TestNonOfficialUnsafeAndLenientWarnOnly(t *testing.T) {
	cases := map[string]func(){
		"dev build":  func() {},
		"unsafe":     func() { isOfficial = func() bool { return true }; SetUnsafe(true) },
		"cloud sess": func() { isOfficial = func() bool { return true }; SetLenient(true) },
	}
	for name, arm := range cases {
		setup(t, false)
		arm()
		s := newServer(t, func(c, h string) string { return "" })
		var warned string
		if err := Ensure(s.URL, func(f string, a ...interface{}) { warned = f }); err != nil {
			t.Errorf("%s: failure must be tolerated, got %v", name, err)
		}
		if !strings.Contains(warned, "WARNING") {
			t.Errorf("%s: no warning logged", name)
		}
	}
}

func TestSuccessCachedPerProcess(t *testing.T) {
	_, priv := setup(t, true)
	s := newServer(t, signer(priv))
	for i := 0; i < 3; i++ {
		if err := Ensure(s.URL, nil); err != nil {
			t.Fatal(err)
		}
	}
	if s.proofCalls.Load() != 1 {
		t.Fatalf("expected 1 proof request, got %d", s.proofCalls.Load())
	}
}

func TestVerifySendsNoCredentialAndNothingElse(t *testing.T) {
	_, priv := setup(t, true)
	s := newServer(t, signer(priv))
	if err := Verify(s.URL); err != nil {
		t.Fatal(err)
	}
	if s.authSeen.Load() || s.otherCalls.Load() != 0 {
		t.Fatal("verification leaked a credential or touched another route")
	}
}

func TestEmbeddedKeysWellFormed(t *testing.T) {
	if len(trustedKeys) != 2 {
		t.Fatalf("expected staging+production keys, got %d", len(trustedKeys))
	}
	for _, k := range trustedKeys {
		raw, err := base64.StdEncoding.DecodeString(k.Key)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			t.Errorf("key %s malformed", k.ID)
		}
	}
}

func TestNoCredentialSentWhenOfficialVerificationFails(t *testing.T) {
	setup(t, true)
	s := newServer(t, func(c, h string) string { return "" })
	if err := Ensure(s.URL, nil); err == nil {
		t.Fatal("expected refusal")
	}
	if s.authSeen.Load() || s.otherCalls.Load() != 0 || s.proofCalls.Load() != 1 {
		t.Fatalf("only the unauthenticated proof request may reach the server (auth=%v other=%d proof=%d)",
			s.authSeen.Load(), s.otherCalls.Load(), s.proofCalls.Load())
	}
}
