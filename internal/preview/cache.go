package preview

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Cache is an on-disk, content-addressed store of built preview artifacts,
// keyed by missionId (the bridge's jobId — same value, see api.Client
// preview methods). It exists so a later revive (task 4.4) can re-upload
// without rebuilding.
//
// Retention: design.md says "until the mission is deleted or the user
// clears it." The bridge has no mission-deletion message today (checked
// bridge.go's workflow: message handlers — workflow:task_cleanup only
// removes one task's git worktree, not the mission-scoped preview cache),
// so nothing currently calls Clear automatically. It's exposed here for
// whichever future change adds that hook.
type Cache struct {
	baseDir string
	mu      sync.Mutex
}

// cacheManifestEntry is what gets persisted per mission.
type cacheManifestEntry struct {
	Files []ManifestFile `json:"files"`
}

// NewCache creates a Cache rooted at baseDir, creating it (and its blob/
// manifest subdirectories) if absent.
func NewCache(baseDir string) (*Cache, error) {
	if err := os.MkdirAll(filepath.Join(baseDir, "blobs"), 0755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(baseDir, "manifests"), 0755); err != nil {
		return nil, err
	}
	return &Cache{baseDir: baseDir}, nil
}

func (c *Cache) blobPath(sha256 string) string {
	return filepath.Join(c.baseDir, "blobs", sha256)
}

func (c *Cache) manifestPath(missionID string) string {
	return filepath.Join(c.baseDir, "manifests", missionID+".json")
}

// Store copies every manifest file's bytes (read from outputDir) into the
// content-addressed blob store and persists the manifest for missionID,
// overwriting whatever was cached for that mission before.
func (c *Cache) Store(missionID, outputDir string, files []ManifestFile) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, f := range files {
		dst := c.blobPath(f.SHA256)
		if fileExists(dst) {
			continue // content-addressed: identical bytes already stored
		}
		src := filepath.Join(outputDir, filepath.FromSlash(f.Path))
		data, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("cache store: read %s: %w", f.Path, err)
		}
		if err := os.WriteFile(dst, data, 0644); err != nil {
			return fmt.Errorf("cache store: write blob %s: %w", f.SHA256, err)
		}
	}

	entry := cacheManifestEntry{Files: files}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return os.WriteFile(c.manifestPath(missionID), data, 0644)
}

// Manifest returns the cached manifest for missionID, or (nil, false) if
// nothing is cached (never built, or cleared).
func (c *Cache) Manifest(missionID string) ([]ManifestFile, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	data, err := os.ReadFile(c.manifestPath(missionID))
	if err != nil {
		return nil, false
	}
	var entry cacheManifestEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, false
	}
	return entry.Files, true
}

// Blob reads back the cached bytes for a content hash.
func (c *Cache) Blob(sha256 string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return os.ReadFile(c.blobPath(sha256))
}

// Clear removes the cached manifest pointer for missionID. Blobs are left
// in place — they're content-addressed and may be shared across missions —
// so this only drops the per-mission index; reclaiming orphaned blobs would
// need a separate GC pass, out of scope for this task.
func (c *Cache) Clear(missionID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	err := os.Remove(c.manifestPath(missionID))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
