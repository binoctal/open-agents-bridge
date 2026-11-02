package register

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakePlatform scripts the register handshake over a real HTTP server, with
// the same wire shapes the API worker serves (apps/api hosting-nodes.ts).
type fakePlatform struct {
	srv *httptest.Server
	// redeemStatus/redeemBody script the redeem answer; confirm likewise.
	redeemStatus int
	redeemBody   string
	confirmCode  int
	confirmBody  string
	// sawPublicIP records the confirm payload's publicIp presence flag.
	sawPublicIP bool
}

func newFakePlatform(t *testing.T) *fakePlatform {
	t.Helper()
	f := &fakePlatform{
		redeemStatus: 200,
		redeemBody:   `{"nonce":"n-abc123","probePath":"/oa-register-probe","deadlineSeconds":300}`,
		confirmCode:  201,
		confirmBody:  `{"nodeId":"nd_1","nodeSecret":"sec_1","publicIp":"203.0.113.10"}`,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/hosting-node-register/redeem", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(f.redeemStatus)
		_, _ = w.Write([]byte(f.redeemBody))
	})
	mux.HandleFunc("/internal/hosting-node-register/confirm", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, f.sawPublicIP = body["publicIp"]
		w.WriteHeader(f.confirmCode)
		_, _ = w.Write([]byte(f.confirmBody))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// stubBind replaces the :80/:443 listener seam with one ephemeral loopback
// listener, so Run's handshake runs unprivileged.
func stubBind(t *testing.T) {
	t.Helper()
	orig := bindProbes
	bindProbes = func(probePath, nonce string) ([]*probeServer, error) {
		return listenProbesOn([]string{"127.0.0.1:0"}, probePath, nonce)
	}
	t.Cleanup(func() { bindProbes = orig })
}

func opts(f *fakePlatform, out string) Options {
	return Options{
		Code:           "oan-test-code",
		APIBaseURL:     f.srv.URL,
		CredentialsOut: out,
		Timeout:        10 * time.Second,
	}
}

// A rejected code surfaces the platform's own envelope text: "install code
// rejected" plus CODE_EXPIRED's message, not a bare "400".
func TestRunRedeemRejectedIsReadable(t *testing.T) {
	f := newFakePlatform(t)
	f.redeemStatus = 400
	f.redeemBody = `{"error":{"code":"CODE_EXPIRED","message":"install code expired; generate a new one"}}`
	out := filepath.Join(t.TempDir(), "sub", "node.json")
	err := Run(context.Background(), opts(f, out))
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"install code rejected", "CODE_EXPIRED", "install code expired"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should contain %q", err, want)
		}
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("no credentials may be written on a rejected code")
	}
}

// A failed confirm (probe could not reach the machine) carries the
// rerun-same-code guidance -- the operator's next action.
func TestRunConfirmProbeFailedAdvisesRerun(t *testing.T) {
	f := newFakePlatform(t)
	stubBind(t)
	f.confirmCode = 422
	f.confirmBody = `{"error":{"code":"PROBE_FAILED","message":"neither 80 nor 443 answered the nonce","details":{"probes":[{"port":80,"ok":false},{"port":443,"ok":false}]}}}`
	out := filepath.Join(t.TempDir(), "node.json")
	err := Run(context.Background(), opts(f, out))
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"registration not completed", "PROBE_FAILED", "rerun the same command"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should contain %q", err, want)
		}
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("no credentials may be written on a failed confirm")
	}
}

// The happy handshake persists 0600 credentials (0700 dir) that
// LoadCredentials round-trips, and confirm went out WITHOUT a publicIp when
// egress detection found none (lab machine: the platform's connection-IP view
// carries the handshake alone).
func TestRunSuccessPersistsCredentials(t *testing.T) {
	f := newFakePlatform(t)
	stubBind(t)
	// Nested path: writeCredentials creates the credentials dir itself, so
	// the 0700 assertion is against a dir the production code made.
	out := filepath.Join(t.TempDir(), "oa", "node.json")
	if err := Run(context.Background(), opts(f, out)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	fi, err := os.Stat(out)
	if err != nil {
		t.Fatalf("credentials missing: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("credentials mode = %o, want 600", perm)
	}
	creds, err := LoadCredentials(out)
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	if creds.NodeID != "nd_1" || creds.NodeSecret != "sec_1" || creds.APIBaseURL != f.srv.URL {
		t.Errorf("round-trip mismatch: %+v", creds)
	}
	if f.sawPublicIP {
		t.Error("confirm must omit publicIp when no public egress IPv4 was self-detected")
	}
	// The credentials dir is 0700 (the secret's first perimeter).
	if di, err := os.Stat(filepath.Dir(out)); err == nil && di.Mode().Perm() != 0o700 {
		t.Errorf("credentials dir mode = %o, want 700", di.Mode().Perm())
	}
}

// Missing arguments fail before any network I/O, naming the flag to set.
func TestRunValidatesArguments(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts Options
		want string
	}{
		{"no code", Options{APIBaseURL: "https://api", CredentialsOut: "/tmp/x"}, "--code is required"},
		{"no api", Options{Code: "oan-x", CredentialsOut: "/tmp/x"}, "--api is required"},
		{"no out", Options{Code: "oan-x", APIBaseURL: "https://api"}, "credentials output path is required"},
	} {
		if err := Run(context.Background(), tc.opts); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want error containing %q, got %v", tc.name, tc.want, err)
		}
	}
}

// Zero bound probe ports is the operator-actionable failure: name both ports
// and the likely culprit (an existing web server), never a bare EACCES.
func TestListenProbesOnBothPortsHeld(t *testing.T) {
	var held []net.Listener
	for i := 0; i < 2; i++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, ln)
	}
	t.Cleanup(func() {
		for _, ln := range held {
			ln.Close()
		}
	})
	ports := make([]string, len(held))
	for i, ln := range held {
		ports[i] = ln.Addr().String()
	}
	_, err := listenProbesOn(ports, "/oa-register-probe", "n-abc")
	if err == nil {
		t.Fatal("want bind failure")
	}
	for _, want := range []string{"cannot bind port 80 or 443", "stop the web server", ports[0], ports[1]} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should contain %q", err, want)
		}
	}
}

// One bound port is enough (the platform probes both); the listener answers
// the nonce at the probe path and 404s everything else.
func TestListenProbesOnSinglePortServesNonce(t *testing.T) {
	servers, err := listenProbesOn([]string{"127.0.0.1:0", "127.0.0.1:0"}, "/oa-register-probe", "n-secret123")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, s := range servers {
			s.srv.Close()
		}
	})
	if len(servers) != 2 {
		t.Fatalf("loopback binds must succeed, got %d servers", len(servers))
	}
	resp, err := http.Get("http://" + servers[0].Addr + "/oa-register-probe")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	if got := string(buf[:n]); got != "openagents-node-register:n-secret123" {
		t.Errorf("probe answer = %q", got)
	}
	resp2, err := http.Get("http://" + servers[0].Addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("non-probe path = %d, want 404", resp2.StatusCode)
	}
}

// LoadCredentials refuses empty fields naming the field and the fix (rerun
// register), and warns -- without refusing -- on loose permissions.
func TestLoadCredentialsValidation(t *testing.T) {
	dir := t.TempDir()
	for name, raw := range map[string]string{
		"empty node id": `{"nodeId":"","nodeSecret":"s","apiBaseUrl":"https://api"}`,
		"empty secret":  `{"nodeId":"n","nodeSecret":"","apiBaseUrl":"https://api"}`,
		"empty api":     `{"nodeId":"n","nodeSecret":"s","apiBaseUrl":""}`,
	} {
		p := filepath.Join(dir, name+".json")
		if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadCredentials(p); err == nil {
			t.Errorf("%s: want refusal", name)
		} else if !strings.Contains(err.Error(), "rerun register") {
			t.Errorf("%s: error should advise rerunning register, got %q", name, err)
		}
	}
	// Bad JSON is a parse error, not a panic.
	p := filepath.Join(dir, "garbage.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredentials(p); err == nil {
		t.Error("garbage file must fail")
	}
	// Loose mode (0644) still LOADS -- a warning, not a refusal: locking the
	// operator out of their own node over a chmod would strand the machine.
	p = filepath.Join(dir, "loose.json")
	if err := os.WriteFile(p, []byte(`{"nodeId":"n","nodeSecret":"s","apiBaseUrl":"https://api"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCredentials(p); err != nil {
		t.Errorf("loose mode must warn, not refuse: %v", err)
	}
}
