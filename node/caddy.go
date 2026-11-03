package node

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/binoctal/open-agents-bridge/node/agent"
)

// caddyVersion is pinned (hosting-agent-into-bridge D4): a floating "latest"
// could break the on-demand-TLS scaffold with an upstream release; upgrades
// ride a bridge release instead.
const caddyVersion = "2.11.7"

// caddyDownloadBase is overridable in tests (local httptest server).
var caddyDownloadBase = fmt.Sprintf("https://github.com/caddyserver/caddy/releases/download/v%s", caddyVersion)

// ensureCaddy resolves a caddy binary for the user-node daemon and reports
// whether the daemon should own the caddy process (hosting-agent-into-bridge
// D4):
//
//   - caddy on PATH: the operator already manages caddy (systemd unit or
//     manual); we use their binary and DO NOT manage a process -- the
//     scaffold is written, running it is the operator's existing setup.
//   - no caddy anywhere: we download the pinned release, verify it against
//     the official checksums.txt, install under {cacheDir}/bin, and the
//     daemon owns the process lifecycle.
//
// Failure is loud: a missing caddy would otherwise surface as probe/route
// failures much later.
func ensureCaddy(cacheDir string, logf agent.Logf) (caddyPath string, managed bool, err error) {
	if p, lookErr := exec.LookPath("caddy"); lookErr == nil {
		logf("caddy found on PATH at %s (operator-managed); the daemon will not manage its process", p)
		return p, false, nil
	}
	bin := filepath.Join(cacheDir, "bin", "caddy")
	if _, statErr := os.Stat(bin); statErr == nil {
		// Previously installed by us; version check keeps it honest.
		out, runErr := exec.Command(bin, "version").Output()
		if runErr == nil && strings.Contains(string(out), "/"+caddyVersion+" ") {
			logf("caddy %s already installed at %s", caddyVersion, bin)
			return bin, true, nil
		}
	}
	if err := downloadCaddy(bin); err != nil {
		return "", false, fmt.Errorf(`caddy is required for this node's public HTTPS but not installed, and the automatic download failed: %v

Install caddy manually and make sure it is on PATH, then re-run this command, for example:
  sudo apt-get install -y caddy
or download it from https://caddyserver.com/download and place it in PATH`, err)
	}
	logf("caddy %s downloaded and verified at %s", caddyVersion, bin)
	return bin, true, nil
}

// downloadCaddy fetches the pinned caddy tarball + checksums for this
// machine's arch, verifies sha256, and installs the binary to dest (0755).
func downloadCaddy(dest string) error {
	arch := map[string]string{"amd64": "amd64", "arm64": "arm64"}[runtime.GOARCH]
	if arch == "" {
		return fmt.Errorf("unsupported architecture %s (install caddy manually)", runtime.GOARCH)
	}
	name := fmt.Sprintf("caddy_%s_linux_%s.tar.gz", caddyVersion, arch)
	client := &http.Client{Timeout: 10 * time.Minute}
	tarball, err := httpGetAll(client, caddyDownloadBase+"/"+name)
	if err != nil {
		return fmt.Errorf("download %s: %w", name, err)
	}
	sums, err := httpGetAll(client, caddyDownloadBase+"/checksums.txt")
	if err != nil {
		return fmt.Errorf("download checksums.txt: %w", err)
	}
	want, ok := checksumFor(string(sums), name)
	if !ok {
		return fmt.Errorf("checksums.txt has no entry for %s", name)
	}
	got := sha256.Sum256(tarball)
	if !strings.EqualFold(hex.EncodeToString(got[:]), want) {
		return errors.New("sha256 mismatch on the downloaded tarball")
	}
	binary, err := extractSingleFile(tarball)
	if err != nil {
		return fmt.Errorf("extract caddy binary: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dest, binary, 0o755); err != nil {
		return err
	}
	return os.Chmod(dest, 0o755)
}

// httpGetAll reads the whole body (bounded by the tarball size; caddy's
// linux build is ~40MB) with a hard cap so a broken mirror cannot OOM us.
func httpGetAll(client *http.Client, url string) ([]byte, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 128<<20))
}

// checksumFor parses a checksums.txt line ("<sha256>  <filename>").
func checksumFor(sums, name string) (string, bool) {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == name {
			return fields[0], true
		}
	}
	return "", false
}

// extractSingleFile pulls the single "caddy" member out of the gzipped tar.
func extractSingleFile(tarball []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, errors.New("no caddy member in tarball")
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name == "caddy" && hdr.Typeflag == tar.TypeReg {
			return io.ReadAll(tr)
		}
	}
}

// startManagedCaddy launches caddy as a child of the node daemon
// (signal-forwarding semantics: when the daemon exits, caddy goes with it).
// The caller defers kill; caddy itself exits gracefully on SIGTERM.
func startManagedCaddy(caddyPath, caddyfile string, logf agent.Logf) (*exec.Cmd, error) {
	cmd := exec.Command(caddyPath, "run", "--config", caddyfile, "--adapter", "caddyfile")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start caddy with %s: %w", caddyfile, err)
	}
	logf("caddy running as a managed child process (pid %d, config %s)", cmd.Process.Pid, caddyfile)
	return cmd, nil
}
