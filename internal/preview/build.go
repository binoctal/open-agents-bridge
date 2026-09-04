package preview

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// BuildTimeout is the hard ceiling on the build command (spec: 10 minutes).
// A build that runs past it is killed and treated exactly like a failed
// build: log and skip, no retry.
const BuildTimeout = 10 * time.Minute

// outputDirCandidates is the search order for the build's static output.
// The first candidate with an index.html at its root wins (spec: dist/ >
// build/ > out/).
var outputDirCandidates = []string{"dist", "build", "out"}

type packageJSON struct {
	Scripts map[string]string `json:"scripts"`
}

// HasBuildScript reports whether repoRoot has a package.json declaring a
// non-empty "build" script. A missing package.json, or one without a build
// script, is the expected shape for most missions — it returns (false, nil),
// not an error. A malformed package.json is treated the same way (no build
// attempted) rather than surfacing a parse error: the whole feature is
// best-effort, and a project the build detector can't even parse should not
// hold up the mission with a logged error either.
func HasBuildScript(repoRoot string) (bool, error) {
	data, err := os.ReadFile(filepath.Join(repoRoot, "package.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	var pkg packageJSON
	if err := json.Unmarshal(data, &pkg); err != nil {
		return false, nil
	}

	script, ok := pkg.Scripts["build"]
	return ok && script != "", nil
}

// packageManagerCommand picks the package manager to invoke based on which
// lockfile is present in repoRoot, preferring the most specific match.
// Falls back to npm, which is always present alongside Node.
func packageManagerCommand(repoRoot string) (string, []string) {
	switch {
	case fileExists(filepath.Join(repoRoot, "pnpm-lock.yaml")):
		return "pnpm", []string{"run", "build"}
	case fileExists(filepath.Join(repoRoot, "yarn.lock")):
		return "yarn", []string{"build"}
	case fileExists(filepath.Join(repoRoot, "bun.lockb")):
		return "bun", []string{"run", "build"}
	default:
		return "npm", []string{"run", "build"}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// RunBuild runs the project's build script in repoRoot under BuildTimeout.
// A timeout or non-zero exit comes back as a plain error — the caller's
// contract (RunAndUpload) is to log it and skip, never retry.
func RunBuild(repoRoot string) error {
	ctx, cancel := context.WithTimeout(context.Background(), BuildTimeout)
	defer cancel()

	name, args := packageManagerCommand(repoRoot)
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = repoRoot

	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("preview build timed out after %s: %s", BuildTimeout, string(output))
		}
		return fmt.Errorf("preview build failed: %s: %w", string(output), err)
	}
	return nil
}

// ResolveOutputDir finds the build's static output directory, checking
// dist/ > build/ > out/ in order and returning the first one with an
// index.html at its root. Returns ("", false) if none qualify.
func ResolveOutputDir(repoRoot string) (string, bool) {
	for _, candidate := range outputDirCandidates {
		dir := filepath.Join(repoRoot, candidate)
		if fileExists(filepath.Join(dir, "index.html")) {
			return dir, true
		}
	}
	return "", false
}
