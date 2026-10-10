package bridge

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/binoctal/open-agents-bridge/internal/config"
	"github.com/binoctal/open-agents-bridge/internal/serverproof"
)

// An official build pointed at a server that cannot prove itself must send
// nothing but the unauthenticated proof request, and stop when shut down.
func TestWaitServerProofSendsNoCredentialBeforeVerification(t *testing.T) {
	defer serverproof.ForceOfficialForTest(true)()
	var proof, other, auth atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.URL.Query().Get("token") != "" {
			auth.Add(1)
		}
		if r.URL.Path == serverproof.ProofPath {
			proof.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]string{"proof": "", "keyId": "x"})
			return
		}
		other.Add(1)
	}))
	defer srv.Close()

	b := &Bridge{config: &config.Config{ServerURL: srv.URL, MachineToken: "secret"}, done: make(chan struct{})}
	res := make(chan error, 1)
	go func() { res <- b.waitServerProof() }()
	time.Sleep(300 * time.Millisecond)
	close(b.done)
	select {
	case <-res:
	case <-time.After(3 * time.Second):
		t.Fatal("waitServerProof did not stop on shutdown")
	}
	if auth.Load() != 0 || other.Load() != 0 || proof.Load() < 1 {
		t.Fatalf("auth=%d other=%d proof=%d", auth.Load(), other.Load(), proof.Load())
	}
	if err := b.connect(); err == nil {
		t.Fatal("connect must refuse an unverified server on an official build")
	}
	if auth.Load() != 0 || other.Load() != 0 {
		t.Fatal("connect leaked a request before verification")
	}
}

func TestWaitServerProofPassesWithValidProof(t *testing.T) {
	defer serverproof.ForceOfficialForTest(true)()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	defer serverproof.TrustKeyForTest(pub)()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Challenge string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		sig := ed25519.Sign(priv, serverproof.Message(in.Challenge, serverproof.HostOf("http://"+r.Host)))
		_ = json.NewEncoder(w).Encode(map[string]string{"proof": base64.StdEncoding.EncodeToString(sig), "keyId": "t"})
	}))
	defer srv.Close()
	b := &Bridge{config: &config.Config{ServerURL: srv.URL}, done: make(chan struct{})}
	if err := b.waitServerProof(); err != nil {
		t.Fatal(err)
	}
}
