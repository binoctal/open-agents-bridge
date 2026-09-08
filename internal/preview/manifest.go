// Package preview implements the bridge side of add-preview-hosting: after a
// workflow task's git branch merges into main, optionally build the merged
// repo as a static site and upload the output to the platform's preview
// hosting service (tasks 4.1-4.5). Every function here is best-effort by
// design — nothing in this package blocks or fails a mission; see
// RunAndUpload, the only entry point bridge.go calls.
package preview

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ManifestFile is one file in a build's artifact manifest: a POSIX-style
// relative path plus its content hash and byte size. Kept separate from
// api.PreviewFile so this package's pure logic (exercised heavily by unit
// tests) doesn't need to import the API transport package.
type ManifestFile struct {
	Path   string
	SHA256 string
	Size   int64
}

// BuildManifest walks outputDir recursively and returns a sha256 manifest of
// every file, excluding any path ending in ".map" — source maps embed
// original source and must never be uploaded (spec acceptance line: "源码不
// 出设备"). Paths are relative to outputDir and always use forward slashes,
// regardless of host OS, matching the platform's manifest contract.
func BuildManifest(outputDir string) ([]ManifestFile, error) {
	var files []ManifestFile

	err := filepath.Walk(outputDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		rel, relErr := filepath.Rel(outputDir, path)
		if relErr != nil {
			return relErr
		}
		relSlash := filepath.ToSlash(rel)
		if strings.HasSuffix(relSlash, ".map") {
			return nil
		}

		sum, sumErr := SHA256File(path)
		if sumErr != nil {
			return sumErr
		}

		files = append(files, ManifestFile{
			Path:   relSlash,
			SHA256: sum,
			Size:   info.Size(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Deterministic order: makes the manifest reproducible for tests and
	// stable for logging, though the platform doesn't require it.
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// StaticTreeExcludedDirs are directories never included when a whole source
// tree (not a build output) is uploaded as a static site — the S5 no-build
// fallback in RunAndUpload. Must stay in sync with
// deploysource.ExcludedDirs (guarded by TestStaticTreeExclusionsMirrorDeploysource).
var StaticTreeExcludedDirs = map[string]bool{
	".git": true, "node_modules": true, "dist": true, "build": true,
	"out": true, ".next": true, ".cache": true, ".turbo": true, "coverage": true,
	".open-agents-bridge-worktrees": true,
}

// StaticTreeSensitivePatterns match basenames never uploaded in a fallback
// static manifest. Must stay in sync with deploysource.SensitivePatterns
// (same guard test).
var StaticTreeSensitivePatterns = []*regexp.Regexp{
	regexp.MustCompile(`^\.env(\..+)?$`),
	regexp.MustCompile(`\.(pem|key|p12|pfx)$`),
	regexp.MustCompile(`^id_rsa`),
	regexp.MustCompile(`^id_ed25519`),
	regexp.MustCompile(`^id_ecdsa`),
	regexp.MustCompile(`^service-account.+\.json$`),
	regexp.MustCompile(`^credentials.*\.json$`),
}

// BuildStaticTreeManifest is BuildManifest over a raw source tree: the S5
// no-build fallback serves repoRoot itself, so the same directories and
// sensitive files the deploy source packer strips must not enter a preview
// manifest either — BuildManifest alone was designed for dist outputs and
// would happily upload .git and .env.
func BuildStaticTreeManifest(root string) ([]ManifestFile, error) {
	excluded := StaticTreeExcludedDirs
	sensitive := func(base string) bool {
		for _, re := range StaticTreeSensitivePatterns {
			if re.MatchString(base) {
				return true
			}
		}
		return false
	}

	var files []ManifestFile
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if excluded[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		// Linked-worktree .git pointer file — see deploysource.PackSourceTree.
		if info.Name() == ".git" {
			return nil
		}
		if sensitive(info.Name()) {
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		relSlash := filepath.ToSlash(rel)
		if strings.HasSuffix(relSlash, ".map") {
			return nil
		}

		sum, sumErr := SHA256File(path)
		if sumErr != nil {
			return sumErr
		}
		files = append(files, ManifestFile{Path: relSlash, SHA256: sum, Size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// SHA256File hashes one file's contents. Exported for deploysource, which
// builds the same sha256 manifest contract over source trees.
func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// HasRootIndexHTML reports whether the manifest includes a root-level
// index.html. The platform rejects manifests without one (PREVIEW_INVALID_MANIFEST).
func HasRootIndexHTML(files []ManifestFile) bool {
	for _, f := range files {
		if f.Path == "index.html" {
			return true
		}
	}
	return false
}

// HasRuntimeJSON reports whether the manifest includes a root-level
// runtime.json — the runtime tree's anchor in the shared declare pipeline
// (a packed Next standalone tree has no index.html; the platform accepts
// either anchor, add-runtime-deploy-v0 D3).
func HasRuntimeJSON(files []ManifestFile) bool {
	for _, f := range files {
		if f.Path == "runtime.json" {
			return true
		}
	}
	return false
}
