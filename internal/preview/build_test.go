package preview

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func TestHasBuildScript_Present(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "package.json", `{"name":"x","scripts":{"build":"vite build","test":"vitest"}}`)

	has, err := HasBuildScript(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("expected build script to be detected")
	}
}

func TestHasBuildScript_Absent(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "package.json", `{"name":"x","scripts":{"test":"vitest"}}`)

	has, err := HasBuildScript(dir)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("expected no build script")
	}
}

func TestHasBuildScript_EmptyBuildScript(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "package.json", `{"scripts":{"build":""}}`)

	has, err := HasBuildScript(dir)
	if err != nil {
		t.Fatal(err)
	}
	if has {
		t.Error("empty build script should not count as present")
	}
}

func TestHasBuildScript_NoPackageJSON(t *testing.T) {
	dir := t.TempDir()

	has, err := HasBuildScript(dir)
	if err != nil {
		t.Fatalf("missing package.json should not error, got %v", err)
	}
	if has {
		t.Error("expected false when package.json is absent")
	}
}

func TestHasBuildScript_MalformedPackageJSON(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "package.json", `{not valid json`)

	has, err := HasBuildScript(dir)
	if err != nil {
		t.Fatalf("malformed package.json should degrade to false, not error, got %v", err)
	}
	if has {
		t.Error("expected false for malformed package.json")
	}
}

func TestPackageManagerCommand_PrefersLockfiles(t *testing.T) {
	cases := []struct {
		lockfile    string
		wantName    string
		wantArgsLen int
	}{
		{"pnpm-lock.yaml", "pnpm", 2},
		{"yarn.lock", "yarn", 1},
		{"bun.lockb", "bun", 2},
	}
	for _, c := range cases {
		t.Run(c.lockfile, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, c.lockfile, "")
			name, args := packageManagerCommand(dir)
			if name != c.wantName {
				t.Errorf("got %s, want %s", name, c.wantName)
			}
			if len(args) != c.wantArgsLen {
				t.Errorf("got args %v", args)
			}
		})
	}
}

func TestPackageManagerCommand_DefaultsToNpm(t *testing.T) {
	dir := t.TempDir()
	name, args := packageManagerCommand(dir)
	if name != "npm" {
		t.Errorf("expected npm default, got %s", name)
	}
	if len(args) != 2 || args[0] != "run" || args[1] != "build" {
		t.Errorf("unexpected args: %v", args)
	}
}

func TestResolveOutputDir_PrefersDistOverBuildOverOut(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "build/index.html", "<html></html>")
	writeFile(t, dir, "out/index.html", "<html></html>")

	// Only build/ and out/ exist: build/ must win since dist/ is checked
	// first but absent here, then build/ before out/.
	got, ok := ResolveOutputDir(dir)
	if !ok {
		t.Fatal("expected an output dir to resolve")
	}
	if got != filepath.Join(dir, "build") {
		t.Errorf("expected build/ to win over out/, got %s", got)
	}

	// Now add dist/ too — it must take priority over build/.
	writeFile(t, dir, "dist/index.html", "<html></html>")
	got, ok = ResolveOutputDir(dir)
	if !ok {
		t.Fatal("expected an output dir to resolve")
	}
	if got != filepath.Join(dir, "dist") {
		t.Errorf("expected dist/ to win, got %s", got)
	}
}

func TestResolveOutputDir_RequiresIndexAtRoot(t *testing.T) {
	dir := t.TempDir()
	// index.html nested, not at dist/ root — must not count.
	writeFile(t, dir, "dist/sub/index.html", "<html></html>")

	if _, ok := ResolveOutputDir(dir); ok {
		t.Error("expected no output dir when index.html is not at the candidate's root")
	}
}

func TestResolveOutputDir_NoneFound(t *testing.T) {
	dir := t.TempDir()
	if _, ok := ResolveOutputDir(dir); ok {
		t.Error("expected false when no candidate output dir exists")
	}
}

func TestRunBuild_NonZeroExit(t *testing.T) {
	dir := t.TempDir()
	// A package.json with a build script pointing at a command guaranteed to
	// fail; RunBuild must surface a plain error, not panic or hang.
	writeFile(t, dir, "package.json", `{"scripts":{"build":"false"}}`)

	// npm may not be installed in a minimal CI image; skip gracefully if so.
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not available in test environment")
	}

	err := RunBuild(dir)
	if err == nil {
		t.Error("expected error for a build script that exits non-zero")
	}
}
