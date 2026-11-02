// Package register implements the `hosting-agent register --code` subcommand
// (byo-hosting-node D1/D2): redeem the one-time install code, prove the
// machine via a temporary nonce listener on 80/443, and persist the issued
// node credentials (0600) for the daemon to pick up.
//
// The handshake is one-shot, not a loop: a failed probe prints the platform's
// own readable reason (probe results, code expiry) and exits 1 -- the code
// stays usable until confirm succeeds or its TTL runs out, so the operator
// fixes the network and reruns the same command.
package register

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/binoctal/open-agents/hosting-agent/internal/api"
)

// Credentials is the daemon-side credentials file (JSON, 0600). The apiBaseUrl
// rides along so the daemon needs no separate API endpoint configuration --
// a user node serves exactly one platform.
type Credentials struct {
	NodeID     string `json:"nodeId"`
	NodeSecret string `json:"nodeSecret"`
	APIBaseURL string `json:"apiBaseUrl"`
}

// LoadCredentials reads and validates a credentials file for the daemon
// (user-node mode). A looser-than-0600 mode is a loud warning, not a refusal:
// the secret is the node's own credential (revocable from the web), but the
// operator should fix the permissions.
func LoadCredentials(path string) (*Credentials, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read node credentials %s: %w", path, err)
	}
	var creds Credentials
	if err := json.Unmarshal(raw, &creds); err != nil {
		return nil, fmt.Errorf("parse node credentials %s: %w", path, err)
	}
	switch {
	case creds.NodeID == "":
		return nil, fmt.Errorf("node credentials %s: nodeId is empty; rerun register", path)
	case creds.NodeSecret == "":
		return nil, fmt.Errorf("node credentials %s: nodeSecret is empty; rerun register", path)
	case creds.APIBaseURL == "":
		return nil, fmt.Errorf("node credentials %s: apiBaseUrl is empty; rerun register", path)
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "hosting-agent: warning: %s is mode %v; chmod 600 recommended\n", path, fi.Mode().Perm())
	}
	return &creds, nil
}

// Options for Run. The zero-valued seams fall back to production behavior.
type Options struct {
	Code           string        // install code (oan-...), required
	APIBaseURL     string        // platform API origin, required
	CredentialsOut string        // credentials file path, required
	Timeout        time.Duration // handshake budget; 0 = 90s
	Logf           func(format string, args ...any)
}

// Run performs the full handshake and writes the credentials file.
func Run(ctx context.Context, opts Options) error {
	switch {
	case opts.Code == "":
		return fmt.Errorf("--code is required (generate one on the web \"my nodes\" page)")
	case opts.APIBaseURL == "":
		return fmt.Errorf("--api is required (the platform API origin, e.g. https://api.openagents.top)")
	case opts.CredentialsOut == "":
		return fmt.Errorf("credentials output path is required")
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(format string, args ...any) { fmt.Printf(format+"\n", args...) }
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	client := api.New(opts.APIBaseURL, "", "") // register face: the code is the bearer
	if client.HTTP.Timeout < timeout {
		client.HTTP.Timeout = timeout
	}

	// 1. Redeem: nonce + probe path.
	logf("redeeming install code against %s ...", opts.APIBaseURL)
	redeem, err := client.RedeemInstallCode(ctx, opts.Code)
	if err != nil {
		return fmt.Errorf("install code rejected: %w", err)
	}
	logf("code accepted; platform will probe this machine on port 80/443 at %s", redeem.ProbePath)

	// 2. Temp nonce listener(s). At least one of 80/443 must bind; both are
	// probed platform-side. A bind failure is almost always an existing web
	// server (Caddy/nginx) holding the port -- name it, since "permission
	// denied" alone sent users digging in the wrong place. bindProbes is the
	// seam so tests can exercise the handshake without root's 80/443.
	listeners, err := bindProbes(redeem.ProbePath, redeem.Nonce)
	if err != nil {
		return err
	}
	defer func() {
		for _, srv := range listeners {
			srv.srv.Close()
		}
	}()
	for _, srv := range listeners {
		logf("answering probe on %s", srv.Addr)
	}

	// 3. Confirm: the platform probes NOW (synchronously, inside this call).
	ip := egressIPv4()
	if ip != "" {
		logf("self-detected egress IPv4: %s (platform verifies its own view)", ip)
	} else {
		logf("no self-detected public IPv4; relying on the platform's connection-IP view")
	}
	confirm, err := client.ConfirmRegistration(ctx, opts.Code, ip, runtime.GOARCH)
	if err != nil {
		return fmt.Errorf("registration not completed: %w\n(fix the reported problem and rerun the same command; the code stays valid until it succeeds or expires)", err)
	}

	// 4. Persist credentials 0600 (dir 0700), atomically.
	creds := Credentials{
		NodeID:     confirm.NodeID,
		NodeSecret: confirm.NodeSecret,
		APIBaseURL: opts.APIBaseURL,
	}
	if err := writeCredentials(opts.CredentialsOut, creds); err != nil {
		return fmt.Errorf("registration succeeded but persisting credentials failed: %w", err)
	}
	// The node-id marker (0644, next to the 0600 credentials) is how a
	// same-host bridge recognizes this machine's node for the local tier
	// (byo-hosting-node D3): root installs leave node.json unreadable to the
	// user's bridge, and the node id is public material anyway — it rides
	// every poll. Failure is non-fatal: local-tier detection degrades to the
	// R2 path, and register still succeeded.
	if err := writeNodeIDMarker(opts.CredentialsOut, creds.NodeID); err != nil {
		logf("warning: could not write the node-id marker (%v); same-machine local deploys will fall back to R2 uploads", err)
	}
	logf("node %s registered (verified public IP %s)", confirm.NodeID, confirm.PublicIP)
	logf("credentials written to %s (0600)", opts.CredentialsOut)
	logf("next: start the agent with HOSTING_NODE_CREDENTIALS=%s (then start Caddy)", opts.CredentialsOut)
	return nil
}

// NodeIDMarkerPath is where the world-readable node id lands (sibling of the
// credentials file). The bridge reads exactly this name.
const NodeIDMarkerPath = "node-id"

func writeNodeIDMarker(credsPath, nodeID string) error {
	return os.WriteFile(filepath.Join(filepath.Dir(credsPath), NodeIDMarkerPath), []byte(nodeID+"\n"), 0o644)
}

// probeServer is one bound probe listener.
type probeServer struct {
	Addr string
	srv  *http.Server
}

// bindProbes is the injectable listener seam (production: :80 + :443, which
// needs root; tests swap it for an ephemeral-port listener).
var bindProbes = listenProbes

// listenProbes binds :80 and :443 (plain HTTP on both -- the platform probes
// before Caddy/TLS exists) and serves the nonce at probePath. Binding at
// least one port is enough (the platform tries both); zero bound ports is a
// hard failure naming the likely culprits.
func listenProbes(probePath, nonce string) ([]*probeServer, error) {
	return listenProbesOn([]string{":80", ":443"}, probePath, nonce)
}

// listenProbesOn is listenProbes against explicit ports (tests run it on
// ephemeral loopback ports; production always passes :80/:443).
func listenProbesOn(ports []string, probePath, nonce string) ([]*probeServer, error) {
	mux := http.NewServeMux()
	// The platform checks body.Contains(nonce); the prefix makes the answer
	// self-describing for a human hitting the URL in a browser.
	mux.HandleFunc(probePath, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "openagents-node-register:%s", nonce)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	var out []*probeServer
	var bindErrs []string
	for _, port := range ports {
		ln, err := net.Listen("tcp", port)
		if err != nil {
			bindErrs = append(bindErrs, fmt.Sprintf("port %s: %v", port, err))
			continue
		}
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = srv.Serve(ln) }() // closed by the caller's defer
		// Addr is the BOUND address (":80" listens may land on [::]:80).
		out = append(out, &probeServer{Addr: ln.Addr().String(), srv: srv})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf(
			"cannot bind port 80 or 443 for the registration probe (%s) -- stop the web server holding them (systemctl stop caddy/nginx) and rerun; the probe must be answerable from the public internet",
			joinAnd(bindErrs))
	}
	return out, nil
}

// egressIPv4 best-effort detects this machine's egress IPv4 via a UDP dial
// (no packets leave for a connected-socket dial to a public resolver; the
// kernel picks the source address the default route would use). "" when the
// result is not a public IPv4 -- the platform's connection-IP view then
// carries the handshake alone.
func egressIPv4() string {
	conn, err := net.Dial("udp4", "9.9.9.9:53")
	if err != nil {
		return ""
	}
	defer conn.Close()
	ip := conn.LocalAddr().(*net.UDPAddr).IP
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return ""
	}
	return ip.String()
}

// writeCredentials persists the credentials file 0600 with a tmp+rename swap
// so a crash never leaves a half-written credential (or a world-readable one:
// rename of a 0600 tmp keeps the mode).
func writeCredentials(path string, creds Credentials) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".node-creds-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func joinAnd(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "; "
		}
		out += p
	}
	return out
}
