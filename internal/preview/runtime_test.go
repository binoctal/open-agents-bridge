package preview

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/binoctal/open-agents-bridge/internal/api"
)

func writeFileT(t *testing.T, dir, rel, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A fake Next build output: standalone server bundle + static assets + public.
func seedNextBuild(t *testing.T, repoRoot string) {
	t.Helper()
	writeFileT(t, repoRoot, ".next/standalone/server.js", "require('http')")
	writeFileT(t, repoRoot, ".next/standalone/.next/server/pages/index.js", "page")
	writeFileT(t, repoRoot, ".next/static/chunks/app.js", "chunk")
	writeFileT(t, repoRoot, "public/logo.svg", "<svg/>")
}

func TestPackNextStandalone(t *testing.T) {
	repo := t.TempDir()
	seedNextBuild(t, repo)

	packed, err := PackNextStandalone(repo)
	if err != nil {
		t.Fatal(err)
	}

	// The canonical Next recipe: static assets and public files land INSIDE
	// the standalone tree, runtime.json anchors it.
	for _, rel := range []string{
		"server.js",
		".next/server/pages/index.js",
		".next/static/chunks/app.js",
		"public/logo.svg",
		"runtime.json",
	} {
		if _, err := os.Stat(filepath.Join(packed, filepath.FromSlash(rel))); err != nil {
			t.Errorf("packed tree missing %s: %v", rel, err)
		}
	}

	// runtime.json carries the entry contract the node executor consumes.
	data, err := os.ReadFile(filepath.Join(packed, "runtime.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rt RuntimeManifest
	if err := json.Unmarshal(data, &rt); err != nil {
		t.Fatalf("runtime.json not parseable: %v", err)
	}
	if rt.Version != 1 || rt.Port != 3000 || rt.HealthCheckPath != "/" {
		t.Errorf("runtime.json = %+v, want v1 Next standalone convention", rt)
	}
	if len(rt.StartCommand) != 2 || rt.StartCommand[0] != "node" || rt.StartCommand[1] != "server.js" {
		t.Errorf("startCommand = %v, want [node server.js]", rt.StartCommand)
	}

	// The manifest over the packed tree must carry runtime.json at the root.
	files, err := BuildManifest(packed)
	if err != nil {
		t.Fatal(err)
	}
	if !HasRuntimeJSON(files) {
		t.Error("manifest over packed tree lacks runtime.json")
	}
	if HasRootIndexHTML(files) {
		t.Error("packed tree unexpectedly contains index.html")
	}
}

func TestPackNextStandalone_NoStandaloneOutput(t *testing.T) {
	repo := t.TempDir()
	if _, err := PackNextStandalone(repo); err == nil {
		t.Fatal("expected error when .next/standalone/server.js is absent")
	}
}

func TestPackNextStandalone_NoPublicIsFine(t *testing.T) {
	repo := t.TempDir()
	writeFileT(t, repo, ".next/standalone/server.js", "x")
	writeFileT(t, repo, ".next/static/a.js", "a")

	if _, err := PackNextStandalone(repo); err != nil {
		t.Fatalf("missing public/ must not fail packing: %v", err)
	}
}

// The shared upload pipeline accepts the runtime anchor: a tree with
// runtime.json but no index.html declares, uploads, and completes normally.
func TestUpload_RuntimeAnchorAccepted(t *testing.T) {
	fake := &fakeUploader{
		createResp: &api.DeclarePreviewResponse{
			PreviewID: "p1",
			Uploads: []api.PreviewUpload{
				{Path: "runtime.json", URL: "https://r2.example.com/put-rt"},
				{Path: "server.js", URL: "https://r2.example.com/put-srv"},
			},
		},
	}
	files := []ManifestFile{
		{Path: "runtime.json", SHA256: "r1", Size: 4},
		{Path: "server.js", SHA256: "s1", Size: 4},
	}
	blob := func(path string) ([]byte, error) {
		if path == "runtime.json" {
			return []byte("{}"), nil
		}
		return []byte("srv!"), nil
	}

	if err := Upload(fake, "mission-1", files, blob, api.CompletePreviewBody{TaskID: "t1", Kind: KindRuntime}, nil); err != nil {
		t.Fatal(err)
	}
	if fake.completedBody.Kind != KindRuntime {
		t.Errorf("complete kind = %q, want runtime", fake.completedBody.Kind)
	}
}

// A manifest with NEITHER anchor is still rejected before any declare.
func TestUpload_NoAnchorRejected(t *testing.T) {
	fake := &fakeUploader{createResp: &api.DeclarePreviewResponse{PreviewID: "p1"}}
	files := []ManifestFile{{Path: "server.js", SHA256: "s1", Size: 4}}
	if err := Upload(fake, "mission-1", files, testBlob, api.CompletePreviewBody{}, nil); err == nil {
		t.Fatal("expected anchor rejection")
	}
	if fake.createCalls != 0 {
		t.Error("must not declare an anchor-less manifest")
	}
}

// seedBuildableRepo writes a package.json whose build script is a no-op, so
// RunAndUpload's real RunBuild invocation succeeds instantly while the test
// controls what the "build" left behind.
func seedBuildableRepo(t *testing.T, repoRoot string) {
	t.Helper()
	writeFileT(t, repoRoot, "package.json", `{"name":"x","scripts":{"build":"true"}}`)
}

// Full runtime flow (task 6.2 coexistence): a Next-shaped repo (config
// without output:'export', standalone output present) reports kind=runtime,
// packs the standalone tree, and uploads a runtime.json-anchored manifest —
// while never looking for a static output dir. Since add-dynamic-preview-
// compute the terminal snapshot rides the merge sentinel task id; the
// per-task path is gated off (test below).
func TestRunAndUpload_RuntimeFlow(t *testing.T) {
	repo := t.TempDir()
	seedBuildableRepo(t, repo)
	writeFileT(t, repo, "next.config.js", "module.exports = {}")
	seedNextBuild(t, repo)

	fake := &fakeUploader{
		createResp: &api.DeclarePreviewResponse{
			PreviewID: "p1",
			Uploads:   []api.PreviewUpload{{Path: "runtime.json", URL: "https://r2.example.com/put-rt"}},
		},
	}
	RunAndUpload(fake, nil, "mission-1", repo, TaskIDMerge, nil)

	if len(fake.reportedKinds) != 1 || fake.reportedKinds[0] != KindRuntime {
		t.Fatalf("reported kinds = %v, want [runtime]", fake.reportedKinds)
	}
	if fake.createCalls != 1 {
		t.Fatalf("create calls = %d, want 1", fake.createCalls)
	}
	if fake.completedBody != (api.CompletePreviewBody{TaskID: TaskIDMerge, Kind: KindRuntime}) {
		t.Errorf("complete body = %+v, want merge/runtime", fake.completedBody)
	}
	if fake.createMeta == nil || fake.createMeta.FileCount == 0 {
		t.Errorf("declare meta = %+v, want a file count", fake.createMeta)
	}
	// The uploaded anchor bytes are the packed runtime.json, not something
	// from a static dir.
	if s := string(fake.uploadedBytes["https://r2.example.com/put-rt"]); s == "" || s[0] != '{' {
		t.Errorf("runtime.json upload = %q, want the packed JSON", s)
	}
}

// v1 granularity gate: a runtime tree on the per-task path (real task id)
// reports its kind but never packs/uploads — runtime snapshots are
// terminal-only, the merge-final build registers them.
func TestRunAndUpload_RuntimeTaskPathSkipped(t *testing.T) {
	repo := t.TempDir()
	seedBuildableRepo(t, repo)
	writeFileT(t, repo, "next.config.js", "module.exports = {}")
	seedNextBuild(t, repo)

	fake := &fakeUploader{}
	RunAndUpload(fake, nil, "mission-1", repo, "task-1", nil)

	if len(fake.reportedKinds) != 1 || fake.reportedKinds[0] != KindRuntime {
		t.Fatalf("reported kinds = %v, want [runtime] (mission-level kind still reported)", fake.reportedKinds)
	}
	if fake.createCalls != 0 {
		t.Errorf("create calls = %d, want 0 on the task path", fake.createCalls)
	}
}

// setHostPlatform simulates a BYOD host platform for the native-addon gate.
func setHostPlatform(t *testing.T, goos, goarch string) {
	t.Helper()
	oldOS, oldArch := hostGOOS, hostGOARCH
	hostGOOS, hostGOARCH = goos, goarch
	t.Cleanup(func() { hostGOOS, hostGOARCH = oldOS, oldArch })
}

// Task 2.2: native addons packed on a host that differs from the node target
// (linux/amd64) degrade to not-runnable — no declare, no crash-loop shipping.
func TestRunAndUpload_RuntimeNativeMismatchDegrades(t *testing.T) {
	setHostPlatform(t, "darwin", "arm64")
	repo := t.TempDir()
	seedBuildableRepo(t, repo)
	writeFileT(t, repo, "next.config.js", "module.exports = {}")
	seedNextBuild(t, repo)
	writeFileT(t, repo, ".next/standalone/node_modules/sharp/build/Release/sharp-linux-x64.node", "elf")

	fake := &fakeUploader{}
	RunAndUpload(fake, nil, "mission-1", repo, TaskIDMerge, nil)

	if fake.createCalls != 0 {
		t.Errorf("create calls = %d, want 0 (not-runnable terminal must not upload)", fake.createCalls)
	}
	if len(fake.reportedKinds) != 1 || fake.reportedKinds[0] != KindRuntime {
		t.Fatalf("reported kinds = %v, want [runtime] (detection is honest, only the upload degrades)", fake.reportedKinds)
	}
}

// Same tree on a node-target host passes the gate: cloud bridges (gVisor
// linux) are the native-compatible case.
func TestRunAndUpload_RuntimeNativeOnNodeTargetHost(t *testing.T) {
	setHostPlatform(t, "linux", "amd64")
	repo := t.TempDir()
	seedBuildableRepo(t, repo)
	writeFileT(t, repo, "next.config.js", "module.exports = {}")
	seedNextBuild(t, repo)
	writeFileT(t, repo, ".next/standalone/node_modules/sharp/build/Release/sharp-linux-x64.node", "elf")

	fake := &fakeUploader{
		createResp: &api.DeclarePreviewResponse{
			PreviewID: "p1",
			Uploads:   []api.PreviewUpload{{Path: "runtime.json", URL: "https://r2.example.com/put-rt"}},
		},
	}
	RunAndUpload(fake, nil, "mission-1", repo, TaskIDMerge, nil)

	if fake.createCalls != 1 {
		t.Errorf("create calls = %d, want 1 (native addons on the node target host are fine)", fake.createCalls)
	}
}

// Task 2.3: a runtime repo whose build produced no standalone output (e.g.
// output:'standalone' missing from next.config) degrades softly — kind is
// still reported, nothing is declared, and the mission flow is unaffected.
func TestRunAndUpload_RuntimePackFailureSoftDegrades(t *testing.T) {
	repo := t.TempDir()
	seedBuildableRepo(t, repo)
	writeFileT(t, repo, "next.config.js", "module.exports = {}")
	// Build script is a no-op, so .next/standalone never appears.

	fake := &fakeUploader{}
	RunAndUpload(fake, nil, "mission-1", repo, TaskIDMerge, nil)

	if len(fake.reportedKinds) != 1 || fake.reportedKinds[0] != KindRuntime {
		t.Fatalf("reported kinds = %v, want [runtime]", fake.reportedKinds)
	}
	if fake.createCalls != 0 {
		t.Errorf("create calls = %d, want 0 (pack failure must not declare)", fake.createCalls)
	}
}

// The static flow is unchanged by the runtime branch (task 6.2): an
// index.html-anchored dist/ completes with kind=static and the same
// rewrite-telemetry meta shape as before.
func TestRunAndUpload_StaticFlowUnchanged(t *testing.T) {
	repo := t.TempDir()
	seedBuildableRepo(t, repo)
	// /x.js exists in the output, so the href is an in-manifest root-absolute
	// reference and gets rewritten (that is what HTMLRewrites counts).
	writeFileT(t, repo, "dist/x.js", "x")
	writeFileT(t, repo, "dist/index.html", `<html><a href="/x.js">l</a></html>`)

	fake := &fakeUploader{
		createResp: &api.DeclarePreviewResponse{
			PreviewID: "p1",
			Uploads:   []api.PreviewUpload{{Path: "index.html", URL: "https://r2.example.com/put-idx"}},
		},
	}
	RunAndUpload(fake, nil, "mission-1", repo, "task-1", nil)

	if len(fake.reportedKinds) != 1 || fake.reportedKinds[0] != KindStatic {
		t.Fatalf("reported kinds = %v, want [static]", fake.reportedKinds)
	}
	if fake.completedBody != (api.CompletePreviewBody{TaskID: "task-1", Kind: KindStatic}) {
		t.Errorf("complete body = %+v, want task-1/static", fake.completedBody)
	}
	if fake.createMeta == nil || fake.createMeta.FileCount != 2 || fake.createMeta.HTMLRewrites != 1 {
		t.Errorf("declare meta = %+v, want 2 files / 1 rewrite", fake.createMeta)
	}
}
