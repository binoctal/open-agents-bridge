package agents

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/binoctal/open-agents-bridge/internal/config"
	"github.com/binoctal/open-agents-bridge/internal/logger"
)

// Exec is a resolved executable for a provisioned agent (design D3):
// node is invoked explicitly on the package's js entry so no shell shim,
// npx, or network is involved at spawn time.
type Exec struct {
	Node   string // absolute path to the node binary
	Script string // absolute path to the package's js entry point
}

// npmCommand is the installer binary; a package var so hermetic tests can
// substitute a fake. The real value is resolved via LookPath before use.
var npmCommand = "npm"

// Command returns the argv for spawning the provisioned agent.
func (e Exec) Command() (string, []string) {
	return e.Node, []string{e.Script}
}

// spec ties a CLI type to its expected npm package.
type spec struct {
	Pkg     string
	Version string
}

// expectedSpecs is the provisioning table. It deliberately lives next to the
// pinned version constants: bumping a version is a release-time decision.
func expectedSpecs() map[string]spec {
	return map[string]spec{
		"claude": {Pkg: ClaudeAgentPackage, Version: ClaudeAgentVersion},
	}
}

// Root returns the provisioning root (~/.open-agents-bridge/agents).
func Root() string {
	return filepath.Join(config.ConfigDir(), "agents")
}

// dirFor returns the install directory for one CLI type.
func dirFor(cliType string) string {
	return filepath.Join(Root(), cliType)
}

// EnsureInstalled makes sure the expected package for cliType is installed at
// the expected version. It is safe to call concurrently and from multiple
// bridge processes (flock), and it is best-effort: on failure it returns the
// error and the caller falls back to the pinned-npx path (spec
// agent-provisioning: fallback MUST NOT be worse than the status quo).
func EnsureInstalled(cliType string) error {
	sp, ok := expectedSpecs()[cliType]
	if !ok {
		return fmt.Errorf("agents: no provisioning spec for cliType %q", cliType)
	}

	// Fast path: correct version already installed.
	if err := checkVersion(dirFor(cliType), sp); err == nil {
		return nil
	}

	if _, err := exec.LookPath("npm"); err != nil {
		return fmt.Errorf("agents: npm not available for provisioning: %w", err)
	}

	if err := os.MkdirAll(Root(), 0o755); err != nil {
		return fmt.Errorf("agents: create root: %w", err)
	}

	// Cross-process / cross-goroutine mutual exclusion. The lock file itself
	// is never deleted (deleting a flock target is a classic race).
	lockPath := filepath.Join(Root(), ".provision.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("agents: open lock: %w", err)
	}
	defer lock.Close()
	if err := lockExclusive(int(lock.Fd())); err != nil {
		return fmt.Errorf("agents: flock: %w", err)
	}
	defer lockRelease(int(lock.Fd()))

	// Re-check under the lock: another worker may have finished installing.
	if err := checkVersion(dirFor(cliType), sp); err == nil {
		return nil
	}

	// Install into a staging dir, then swap in atomically. A half-finished
	// npm install must never be visible as a valid install dir.
	staging := filepath.Join(Root(), ".staging-"+cliType+"-"+fmt.Sprint(os.Getpid()))
	_ = os.RemoveAll(staging) // ours by construction (pid-suffixed)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return fmt.Errorf("agents: create staging: %w", err)
	}

	pin := sp.Pkg + "@" + sp.Version
	// npmCommand is a package var so tests can swap in a fake installer.
	install := exec.Command(npmCommand, "install", "--no-audit", "--no-fund", "--loglevel=error", pin)
	install.Dir = staging
	out, err := install.CombinedOutput()
	if err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("agents: npm install %s failed: %v: %s", pin, err, truncate(string(out), 500))
	}

	target := dirFor(cliType)
	if err := checkVersion(staging, sp); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("agents: staged install incomplete: %w", err)
	}

	// Swap: park any existing dir, move the staged one in, then clean the
	// parked copy (it contains no user data — it is a bridge-managed cache).
	if _, statErr := os.Stat(target); statErr == nil {
		parked := target + ".parked-" + fmt.Sprint(os.Getpid())
		if err := os.Rename(target, parked); err != nil {
			_ = os.RemoveAll(staging)
			return fmt.Errorf("agents: park old install: %w", err)
		}
		defer os.RemoveAll(parked) // bridge-managed cache dir, not user data
	}
	if err := os.Rename(staging, target); err != nil {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("agents: swap install into place: %w", err)
	}
	logger.Info("[agents] provisioned %s at %s", pin, target)
	return nil
}

// ResolveExec returns the provisioned executable for cliType, or nil when it
// is not usable (not provisioned / no node). Callers fall back to the pinned
// npx path; nil is not an error.
func ResolveExec(cliType string) *Exec {
	sp, ok := expectedSpecs()[cliType]
	if !ok {
		return nil
	}
	dir := dirFor(cliType)
	if err := checkVersion(dir, sp); err != nil {
		return nil
	}
	node, err := exec.LookPath("node")
	if err != nil {
		return nil
	}
	pkgDir := filepath.Join(dir, "node_modules", sp.Pkg)
	entry, err := binEntry(pkgDir)
	if err != nil {
		return nil
	}
	return &Exec{Node: node, Script: entry}
}

// checkVersion verifies the install dir contains the expected package at the
// expected version.
func checkVersion(dir string, sp spec) error {
	pkgJSON := filepath.Join(dir, "node_modules", sp.Pkg, "package.json")
	data, err := os.ReadFile(pkgJSON)
	if err != nil {
		return fmt.Errorf("not installed: %w", err)
	}
	var meta struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return fmt.Errorf("bad package.json: %w", err)
	}
	if meta.Version != sp.Version {
		return fmt.Errorf("version %s != expected %s", meta.Version, sp.Version)
	}
	return nil
}

// binEntry resolves the package's bin js entry (bin may be a string or a map;
// claude-agent-acp uses {"claude-agent-acp": "dist/index.js"}).
func binEntry(pkgDir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(pkgDir, "package.json"))
	if err != nil {
		return "", err
	}
	var meta struct {
		Bin json.RawMessage `json:"bin"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", err
	}
	rel := ""
	if len(meta.Bin) > 0 {
		var asString string
		if err := json.Unmarshal(meta.Bin, &asString); err == nil && asString != "" {
			rel = asString
		} else {
			var asMap map[string]string
			if err := json.Unmarshal(meta.Bin, &asMap); err == nil {
				// Prefer the bin matching the package's unscoped name.
				unscoped := asMap[filepath.Base(ClaudeAgentPackage)]
				if unscoped != "" {
					rel = unscoped
				} else {
					for _, v := range asMap {
						if v != "" {
							rel = v
							break
						}
					}
				}
			}
		}
	}
	if rel == "" {
		return "", fmt.Errorf("no bin entry in %s", pkgDir)
	}
	return filepath.Join(pkgDir, rel), nil
}

// ProvisionAll ensures every spec'd CLI type is installed. Meant to run as a
// background goroutine at bridge start: failures are logged (WARN) and never
// fatal — sessions fall back to the pinned-npx path.
func ProvisionAll() {
	for cliType := range expectedSpecs() {
		if err := EnsureInstalled(cliType); err != nil {
			logger.Warn("[agents] provisioning %s failed (sessions will use the npx fallback): %v", cliType, err)
		}
	}
}

// truncate keeps error surfaces readable.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
