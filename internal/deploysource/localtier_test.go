package deploysource

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/binoctal/open-agents-bridge/internal/api"
)

// --- same-machine detection ------------------------------------------------------

// The local tier's single switch. Every non-matching posture — no env, no
// files, foreign node id, empty work id — must answer false so the dispatch
// keeps the R2 path byte-for-byte (task 3.2: cross-machine behavior
// unchanged).
func TestSameMachineTier(t *testing.T) {
	dir := t.TempDir()
	credsPath := filepath.Join(dir, "node.json")
	markerPath := filepath.Join(dir, "node-id")

	// Nothing configured: this machine is not a node.
	t.Setenv("HOSTING_NODE_ID", "")
	t.Setenv(nodeCredsEnv, credsPath)
	if SameMachineTier("byn-abc12345") {
		t.Fatal("no registration on this machine must answer false")
	}

	// Credentials file readable (same-user install): nodeId only matters.
	if err := os.WriteFile(credsPath, []byte(`{"nodeId":"byn-abc12345","nodeSecret":"x","apiBaseUrl":"https://api"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !SameMachineTier("byn-abc12345") {
		t.Fatal("credentials-file node id must match its own rows")
	}
	if SameMachineTier("byn-other999") {
		t.Fatal("a foreign node's rows must keep the R2 path")
	}
	if SameMachineTier("") {
		t.Fatal("work items without a node id (old API) must never take the local tier")
	}
	if SameMachineTier("primary") {
		t.Fatal("platform rows must never take the local tier")
	}

	// Root install: the 0600 creds are unreadable to the bridge, the marker
	// file (public material) carries the id instead.
	if err := os.Remove(credsPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath, []byte("byn-marker99\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !SameMachineTier("byn-marker99") {
		t.Fatal("marker-file node id must match its own rows")
	}

	// Explicit override wins over every file.
	t.Setenv("HOSTING_NODE_ID", "byn-override")
	if !SameMachineTier("byn-override") {
		t.Fatal("HOSTING_NODE_ID override must win")
	}
}

// --- local-tier manifest shape ---------------------------------------------------

// stubBuildSave writes a real gzip stream at the requested scratch path and
// reports its true size — the manifest's sha/size must describe actual disk
// bytes, and the path must be exactly what the entry's LocalPath names.
func stubBuildSave(t *testing.T, payload string) func(repoRoot, composeRel, nodeArch string, svc composeService, tag, scratch string) (string, int64, error) {
	t.Helper()
	return func(repoRoot, composeRel, nodeArch string, svc composeService, tag, scratch string) (string, int64, error) {
		if tag != ImageTag("AbC123", svc.name) {
			t.Errorf("build tag = %q, want the folded three-end tag %q", tag, ImageTag("AbC123", svc.name))
		}
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		if _, err := gz.Write([]byte(payload)); err != nil {
			return "", 0, err
		}
		if err := gz.Close(); err != nil {
			return "", 0, err
		}
		tarPath := filepath.Join(scratch, svc.name+".tar")
		if err := os.WriteFile(tarPath, buf.Bytes(), 0o644); err != nil {
			return "", 0, err
		}
		return tarPath, int64(buf.Len()), nil
	}
}

func shaOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// The local-tier contract in one pass: tar entries carry LocalPath under the
// AGENT scratch root with true sha/size + the gzip label; the rewritten
// compose is the only uploaded member; complete rides nil verify. The export
// dir must live under the scratch root — the agent's store refuses anything
// else (its prefix check is the second security layer).
func TestRunLocalTierUploadManifestShape(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "docker-compose.yml"),
		"services:\n  web:\n    build: .\n    ports: [\"3000:3000\"]\n")
	scratch := t.TempDir()
	t.Setenv(scratchRootEnv, scratch)
	t.Setenv("HOSTING_NODE_ID", "byn-abc12345")

	origProbe, origBuild := dockerProbe, buildSave
	dockerProbe = func() error { return nil }
	buildSave = stubBuildSave(t, "docker-layer-bytes")
	defer func() { dockerProbe, buildSave = origProbe, origBuild }()

	fake := &fakeUploader{
		declareResp: &api.DeclareSourceResponse{
			DeploymentID: "AbC123",
			Uploads:      []api.PreviewUpload{{Path: "docker-compose.yml", URL: "https://r2/put/compose"}},
		},
	}
	if err := runLocalTierUpload(fake, "m1", "AbC123", root, "amd64", nil); err != nil {
		t.Fatalf("runLocalTierUpload: %v", err)
	}

	if fake.declared != 1 {
		t.Fatalf("declare called %d times, want 1", fake.declared)
	}
	if len(fake.declareFiles) != 2 {
		t.Fatalf("manifest has %d entries, want tar+compose", len(fake.declareFiles))
	}
	tarEntry, composeEntry := fake.declareFiles[0], fake.declareFiles[1]
	if tarEntry.Path != "web.tar" || tarEntry.Compression != CompressionGzip {
		t.Errorf("tar entry = %+v, want web.tar gzip", tarEntry)
	}
	wantDir := filepath.Join(scratch, "bridge", "AbC123")
	if !strings.HasPrefix(tarEntry.LocalPath, wantDir+string(os.PathSeparator)) {
		t.Errorf("tar LocalPath %q must sit under the agent scratch root %s", tarEntry.LocalPath, wantDir)
	}
	if raw, err := os.ReadFile(tarEntry.LocalPath); err != nil {
		t.Errorf("LocalPath must name a real file: %v", err)
	} else {
		if got := shaOf(raw); got != tarEntry.SHA256 {
			t.Errorf("tar sha = %s, want sha of the bytes on disk (%s)", tarEntry.SHA256, got)
		}
		if int64(len(raw)) != tarEntry.Size {
			t.Errorf("tar size = %d, want %d (compressed bytes)", tarEntry.Size, len(raw))
		}
	}
	if composeEntry.Path != "docker-compose.yml" {
		t.Errorf("second entry = %q, want the compose", composeEntry.Path)
	}
	if composeEntry.LocalPath != "" {
		t.Errorf("the compose must UPLOAD (complete reads it from R2), got localPath %q", composeEntry.LocalPath)
	}
	if raw, err := os.ReadFile(filepath.Join(wantDir, "docker-compose.yml")); err != nil || !strings.Contains(string(raw), ImageTag("AbC123", "web")) {
		t.Errorf("exported compose must be the rewritten one (image: %s): %v", ImageTag("AbC123", "web"), err)
	}

	// Exactly one PUT — the compose — and its bytes are the rewritten file.
	if len(fake.uploaded) != 1 || fake.uploaded["https://r2/put/compose"] == nil {
		t.Fatalf("uploads = %v, want only the compose PUT", fake.uploaded)
	}
	if !strings.Contains(string(fake.uploaded["https://r2/put/compose"]), ImageTag("AbC123", "web")) {
		t.Error("the uploaded compose must be the rewritten text")
	}
	if fake.completed != 1 || fake.completedBody != nil {
		t.Errorf("complete called %d times with %v, want once with nil verify (manifest round-trip)", fake.completed, fake.completedBody)
	}
}

// A build failure must fail the row loud and STAY failed: no declare, no
// upload, and above all no silent R2 fallback that ships the tars the user
// chose to keep on-device.
func TestRunLocalTierUploadNoFallbackOnBuildFailure(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "docker-compose.yml"), "services:\n  web:\n    build: .\n")
	t.Setenv(scratchRootEnv, t.TempDir())

	origProbe, origBuild := dockerProbe, buildSave
	dockerProbe = func() error { return nil }
	buildSave = func(repoRoot, composeRel, nodeArch string, svc composeService, tag, scratch string) (string, int64, error) {
		return "", 0, os.ErrDeadlineExceeded
	}
	defer func() { dockerProbe, buildSave = origProbe, origBuild }()

	var logs []string
	logf := func(format string, args ...interface{}) {
		if len(args) == 0 {
			logs = append(logs, format)
			return
		}
		logs = append(logs, fmt.Sprintf(format, args...))
	}

	fake := &fakeUploader{}
	RunLocalTierUpload(fake, "m1", "d1", root, "", logf)
	if fake.declared != 0 || fake.completed != 0 {
		t.Fatalf("a failed build must not declare or complete, got declare=%d complete=%d", fake.declared, fake.completed)
	}
	if len(logs) == 0 || !strings.Contains(strings.Join(logs, " "), "no R2 fallback") {
		t.Errorf("the failure log must name the no-fallback rule, got %v", logs)
	}
}

// The scratch-root gate: an unusable agent scratch root fails with a reason
// that names the env knob (root installs where /var/lib is not user-writable
// are the expected real-world hit). "Unusable" here is a FILE squatting on a
// parent path — MkdirAll creates missing directories, so absence alone never
// fails.
func TestRunLocalTierUploadScratchRootFailureNamesKnob(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "docker-compose.yml"), "services:\n  web:\n    build: .\n")
	scratch := t.TempDir()
	writeFile(t, filepath.Join(scratch, "blocker"), "a file, not a directory")
	t.Setenv(scratchRootEnv, filepath.Join(scratch, "blocker", "bridge", "d1"))

	origProbe := dockerProbe
	dockerProbe = func() error { return nil }
	defer func() { dockerProbe = origProbe }()

	err := runLocalTierUpload(nil, "m1", "d1", root, "", nil)
	if err == nil || !strings.Contains(err.Error(), scratchRootEnv) {
		t.Fatalf("want scratch-root error naming %s, got %v", scratchRootEnv, err)
	}
}
