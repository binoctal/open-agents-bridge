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

		sum, sumErr := sha256File(path)
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

func sha256File(path string) (string, error) {
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
