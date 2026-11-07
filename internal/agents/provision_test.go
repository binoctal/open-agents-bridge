package agents

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/binoctal/open-agents-bridge/internal/config"
)

// reset points the provisioning root at a temp HOME. npmCommand (the package
// var in provision.go) is swapped per-test; the real-npm path gets its own
// integration test below.
func reset(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

// fakeNpm writes a plausible installed package into the install cwd
// (provision.go sets cmd.Dir to the staging dir).
func fakeNpm(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"mkdir -p \"$PWD/node_modules/@agentclientprotocol/claude-agent-acp/dist\"\n" +
		"printf '{\"name\":\"@agentclientprotocol/claude-agent-acp\",\"version\":\"" + version + "\",\"bin\":{\"claude-agent-acp\":\"dist/index.js\"}}' > \"$PWD/node_modules/@agentclientprotocol/claude-agent-acp/package.json\"\n" +
		"echo '{}' > \"$PWD/node_modules/@agentclientprotocol/claude-agent-acp/dist/index.js\"\n"
	path := filepath.Join(dir, "fake-npm")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEnsureInstalledInstallsWhenMissing(t *testing.T) {
	reset(t)
	old := npmCommand
	npmCommand = fakeNpm(t, ClaudeAgentVersion)
	defer func() { npmCommand = old }()

	if err := EnsureInstalled("claude"); err != nil {
		t.Fatalf("EnsureInstalled: %v", err)
	}
	if err := checkVersion(dirFor("claude"), spec{Pkg: ClaudeAgentPackage, Version: ClaudeAgentVersion}); err != nil {
		t.Fatalf("installed dir invalid: %v", err)
	}
	// No staging leftovers.
	entries, _ := os.ReadDir(Root())
	for _, e := range entries {
		if filepath.Ext(e.Name()) == "" && e.Name()[0] == '.' {
			t.Errorf("staging leftover: %s", e.Name())
		}
	}
}

func TestEnsureInstalledReinstallsOnVersionDrift(t *testing.T) {
	reset(t)
	// Simulate an old install sitting in the target dir.
	home := config.ConfigDir()
	target := filepath.Join(home, "agents", "claude", "node_modules", ClaudeAgentPackage)
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "package.json"),
		[]byte(`{"version":"0.91.0","bin":{"claude-agent-acp":"dist/index.js"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	old := npmCommand
	npmCommand = fakeNpm(t, ClaudeAgentVersion)
	defer func() { npmCommand = old }()

	if err := EnsureInstalled("claude"); err != nil {
		t.Fatalf("EnsureInstalled: %v", err)
	}
	if err := checkVersion(dirFor("claude"), spec{Pkg: ClaudeAgentPackage, Version: ClaudeAgentVersion}); err != nil {
		t.Fatalf("version drift not corrected: %v", err)
	}
}

func TestEnsureInstalledConcurrentSingleInstall(t *testing.T) {
	reset(t)
	calls := 0
	dir := t.TempDir()
	script := "#!/bin/sh\nsleep 0.2\n" +
		"mkdir -p \"$PWD/node_modules/@agentclientprotocol/claude-agent-acp/dist\"\n" +
		"printf '{\"version\":\"" + ClaudeAgentVersion + "\",\"bin\":{\"claude-agent-acp\":\"dist/index.js\"}}' > \"$PWD/node_modules/@agentclientprotocol/claude-agent-acp/package.json\"\n"
	path := filepath.Join(dir, "fake-npm")
	_ = os.WriteFile(path, []byte(script), 0o755)
	old := npmCommand
	npmCommand = path
	defer func() { npmCommand = old }()
	_ = calls

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = EnsureInstalled("claude")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent EnsureInstalled[%d]: %v", i, err)
		}
	}
	if err := checkVersion(dirFor("claude"), spec{Pkg: ClaudeAgentPackage, Version: ClaudeAgentVersion}); err != nil {
		t.Fatalf("final state invalid: %v", err)
	}
}

func TestResolveExecThreeStates(t *testing.T) {
	home := reset(t)

	// 1. Not provisioned → nil.
	if got := ResolveExec("claude"); got != nil {
		t.Fatalf("want nil when not provisioned, got %+v", got)
	}
	// 2. Not a provisionable type → nil.
	if got := ResolveExec("codex"); got != nil {
		t.Fatalf("want nil for unprovisionable type, got %+v", got)
	}

	// 3. Provisioned → node + absolute script entry.
	old := npmCommand
	npmCommand = fakeNpm(t, ClaudeAgentVersion)
	defer func() { npmCommand = old }()
	if err := EnsureInstalled("claude"); err != nil {
		t.Fatal(err)
	}
	exe := ResolveExec("claude")
	if exe == nil {
		t.Fatal("want resolved exec after provisioning")
	}
	wantScript := filepath.Join(home, ".open-agents-bridge", "agents", "claude", "node_modules", ClaudeAgentPackage, "dist", "index.js")
	if exe.Script != wantScript {
		t.Fatalf("script = %q, want %q", exe.Script, wantScript)
	}
	if exe.Node == "" {
		t.Fatal("node binary must be resolved")
	}
}

// Real npm integration: skipped in short mode (downloads the actual package).
func TestEnsureInstalledRealNpm(t *testing.T) {
	if testing.Short() {
		t.Skip("downloads a real npm package")
	}
	reset(t)
	if _, err := os.Stat("/usr/bin/env"); err != nil {
		t.Skip("no /usr/bin/env")
	}
	if err := EnsureInstalled("claude"); err != nil {
		t.Fatalf("real npm provisioning failed: %v", err)
	}
	exe := ResolveExec("claude")
	if exe == nil {
		t.Fatal("ResolveExec nil after real provisioning")
	}
	// The entry must exist on disk.
	if _, err := os.Stat(exe.Script); err != nil {
		t.Fatalf("entry %s missing: %v", exe.Script, err)
	}
}
