package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The scaffold pins the whole on-demand-TLS contract in one file: admin
// loopback, ask callback at the agent's forwarder, per-label on-demand certs,
// and a 404 wildcard catch-all. Content test = the operator's Caddy behaves
// even if they never read a doc.
func TestEnsureNodeCaddyfileScaffold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "etc", "hosting-agent", "Caddyfile")
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, strings.TrimSpace(sprintf(format, args...))) }

	if err := ensureNodeCaddyfile(path, "deploy.openagents.top", "127.0.0.1:2019", "127.0.0.1:20190", logf); err != nil {
		t.Fatalf("first write: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	for _, want := range []string{
		"admin 127.0.0.1:2019",
		"on_demand_tls",
		"ask http://127.0.0.1:20190/ask",
		"*.deploy.openagents.top",
		"on_demand",
		`respond "no route" 404`,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("scaffold missing %q:\n%s", want, content)
		}
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "wrote Caddyfile scaffold") {
		t.Errorf("first write must announce itself (reload hint), got %v", logs)
	}

	// Idempotent: a matching file is left untouched and NOT re-announced.
	logs = nil
	before, _ := os.Stat(path)
	if err := ensureNodeCaddyfile(path, "deploy.openagents.top", "127.0.0.1:2019", "127.0.0.1:20190", logf); err != nil {
		t.Fatalf("second write: %v", err)
	}
	after, _ := os.Stat(path)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("unchanged scaffold must not be rewritten")
	}
	if len(logs) != 0 {
		t.Errorf("unchanged scaffold must not re-announce, got %v", logs)
	}

	// A differing file (operator's old manual config) IS rewritten -- the
	// agent owns this file on a user node -- and says so.
	if err := os.WriteFile(path, []byte("old manual config"), 0o644); err != nil {
		t.Fatal(err)
	}
	logs = nil
	if err := ensureNodeCaddyfile(path, "deploy.openagents.top", "127.0.0.1:2019", "127.0.0.1:20190", logf); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if raw, _ = os.ReadFile(path); !strings.Contains(string(raw), "on_demand_tls") {
		t.Error("diverged scaffold must be replaced with the current one")
	}
	if len(logs) != 1 {
		t.Errorf("rewrite must re-announce, got %v", logs)
	}
}

func sprintf(format string, args ...any) string {
	if len(args) == 0 {
		return format
	}
	return fmt.Sprintf(format, args...)
}

// The shared staging leaf must end up world-writable however it started: the
// bridge (desktop user) exports local-tier tars under it while this daemon
// usually runs as root — a stricter umask or a pre-existing 0755 dir must not
// silently keep the bridge out.
func TestEnsureBridgeExportDir(t *testing.T) {
	root := t.TempDir()
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, sprintf(format, args...)) }

	ensureBridgeExportDir(root, logf)
	dir := filepath.Join(root, "bridge")
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("export dir missing: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o777 {
		t.Errorf("fresh export dir mode = %o, want 777", got)
	}

	// A pre-existing 0755 dir (root-owned boot, manual mkdir) is re-opened.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ensureBridgeExportDir(root, logf)
	info, err = os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o777 {
		t.Errorf("re-opened export dir mode = %o, want 777", got)
	}
	if len(logs) != 0 {
		t.Errorf("a healthy export dir must not warn, got %v", logs)
	}
}
