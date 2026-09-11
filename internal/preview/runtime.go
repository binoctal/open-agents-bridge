package preview

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// RuntimeManifest is the runtime.json contract (add-runtime-deploy-v0 D3):
// the node's executor reads it to know how to start the tree. It is written
// at the ROOT of the packed standalone tree, where it doubles as the
// manifest's required root anchor for the declare call (a runtime tree has
// no index.html; the platform accepts either anchor).
type RuntimeManifest struct {
	// Version is the contract version; consumers reject unknown majors.
	Version int `json:"version"`
	// StartCommand is argv for the container entry (cwd = tree root).
	StartCommand []string `json:"startCommand"`
	// Port is the in-container listen port (PORT env is set to it).
	Port int `json:"port"`
	// HealthCheckPath is the liveness probe path (any HTTP response = alive).
	HealthCheckPath string `json:"healthCheckPath"`
}

// nextStandaloneRuntime is what v0 packs for every detected runtime build:
// the Next standalone convention (node server.js, PORT=3000, probe GET /).
var nextStandaloneRuntime = RuntimeManifest{
	Version:         1,
	StartCommand:    []string{"node", "server.js"},
	Port:            3000,
	HealthCheckPath: "/",
}

// The node containers the platform runs are linux/amd64 (node:22-slim /
// node:20-slim on the current single-node fleet). Native addons packed into
// the standalone tree are platform-tagged, so a BYOD bridge on a different
// host produces .node binaries the container cannot load. hostGOOS/hostGOARCH
// are vars so unit tests can simulate a mismatched host.
var (
	hostGOOS   = runtime.GOOS
	hostGOARCH = runtime.GOARCH
)

// HostMatchesNodeTarget reports whether native binaries produced on this
// host would load inside the node containers.
func HostMatchesNodeTarget() bool {
	return hostGOOS == "linux" && hostGOARCH == "amd64"
}

// HasNativeAddons reports whether the packed tree contains native loadable
// binaries (*.node). Conservative gate for the runtime upload (task 2.2):
// when addons exist and the host platform differs from the node target, the
// terminal degrades to not-runnable instead of shipping a tree that is
// guaranteed to crash on require. Pure-JS trees pass on every host — the
// container only supplies the node binary, not the packages.
func HasNativeAddons(root string) (bool, error) {
	found := false
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, ".node") {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found, err
}

// copyDir recursively copies src into dst (dst/src-basename semantics: the
// contents of src land under dst). Existing files are overwritten, which is
// fine — dst is the fresh standalone output of this very build.
func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
}

// PackNextStandalone assembles the deployable runtime tree for a Next
// non-export build (task 6.1): `.next/standalone` + `.next/static` + `public`
// + `runtime.json`, all under `.next/standalone` itself (the canonical Next
// standalone recipe — the server bundle does NOT include static assets or
// public files). Returns the standalone dir, ready for BuildManifest.
//
// Only Next is packed in v0; a Nuxt/Nitro runtime build returns an error and
// the caller logs and skips, exactly like every other best-effort path.
func PackNextStandalone(repoRoot string) (string, error) {
	standalone := filepath.Join(repoRoot, ".next", "standalone")
	if !fileExists(filepath.Join(standalone, "server.js")) {
		return "", fmt.Errorf("no .next/standalone/server.js (output: 'standalone' missing from next.config, or not a Next build)")
	}

	// The standalone server bundle serves neither of these; copy them in.
	for _, pair := range [][2]string{
		{filepath.Join(repoRoot, ".next", "static"), filepath.Join(standalone, ".next", "static")},
		{filepath.Join(repoRoot, "public"), filepath.Join(standalone, "public")},
	} {
		src, dst := pair[0], pair[1]
		if !fileExists(src) {
			continue // a project without public/ is legitimate
		}
		if err := copyDir(src, dst); err != nil {
			return "", fmt.Errorf("copy %s: %w", src, err)
		}
	}

	data, err := json.MarshalIndent(nextStandaloneRuntime, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(standalone, "runtime.json"), append(data, '\n'), 0o644); err != nil {
		return "", fmt.Errorf("write runtime.json: %w", err)
	}
	return standalone, nil
}
