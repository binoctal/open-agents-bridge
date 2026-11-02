// Same-machine local tier (byo-hosting-node D3): when a pending deploy
// targets a BYO node whose credentials sit on THIS machine (bridge and
// hosting-agent on one host — BYOD's natural shape), the built image tars
// never leave for R2. The flow is the image tier's build + docker-save +
// compose-rewrite with a different declare: tar entries carry LocalPath
// into the shared scratch root (the agent's store prefix-checks and
// sha-verifies them), and only the small rewritten compose uploads —
// complete fetches its text from R2 to run compose-policy. The privacy
// promise, image bytes never leave the device, holds by construction.
//
// Cross-machine rows (bridge on a laptop, node a VPS) never match the
// detection and keep the R2 path byte-for-byte; a failure here NEVER falls
// back to it — a user who picked their own machine chose "artifacts stay on
// this device", and a fallback would upload exactly what they chose to keep
// local (the same reasoning as the image tier's dockerProbe fail-fast).
package deploysource

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/binoctal/open-agents-bridge/internal/logger"
	"github.com/binoctal/open-agents-bridge/internal/preview"
)

const (
	// scratchRootEnv names the only directory tree the hosting-agent's store
	// accepts localPath entries under (its config.ScratchRoot). One env var
	// configures both processes on a one-host install, so the bridge's
	// exports land inside the agent's prefix by construction; the default
	// mirrors the agent's.
	scratchRootEnv = "HOSTING_AGENT_SCRATCH_ROOT"
	defaultScratch = "/var/lib/hosting-agent/scratch"

	// Node credential discovery mirrors hosting-agent's register --out
	// convention: env override first, then the default path. The bridge
	// reads only the NODE ID (never the secret) to recognize its own rows.
	nodeCredsEnv     = "HOSTING_NODE_CREDENTIALS"
	defaultNodeCreds = "/etc/hosting-agent/node.json"
	nodeIDMarkerName = "node-id" // marker register writes next to the creds file
)

// scratchRoot resolves the shared scratch root (test seam via env).
func scratchRoot() string {
	if v := strings.TrimSpace(os.Getenv(scratchRootEnv)); v != "" {
		return v
	}
	return defaultScratch
}

// localNodeID reports this machine's registered BYO node id, "" when this
// machine is not a user node. Lookup order, cheapest trust first:
//
//  1. HOSTING_NODE_ID — explicit override for unusual layouts
//  2. the credentials JSON (same-user installs where the 0600 file is
//     readable by the bridge process) — nodeId field only
//  3. the world-readable node-id marker register writes next to the
//     credentials (root installs, where node.json is 0600 root and the
//     user-owned bridge cannot read it; the node id is public material —
//     it rides every poll — so a 0644 marker leaks nothing)
//
// Every lookup is best-effort: unreadable, absent, or garbage answers ""
// and the caller keeps the unchanged R2 path.
func localNodeID() string {
	if v := strings.TrimSpace(os.Getenv("HOSTING_NODE_ID")); v != "" {
		return v
	}
	credsPath := strings.TrimSpace(os.Getenv(nodeCredsEnv))
	if credsPath == "" {
		credsPath = defaultNodeCreds
	}
	if raw, err := os.ReadFile(credsPath); err == nil {
		var c struct {
			NodeID string `json:"nodeId"`
		}
		if json.Unmarshal(raw, &c) == nil && c.NodeID != "" {
			return c.NodeID
		}
	}
	if raw, err := os.ReadFile(filepath.Join(filepath.Dir(credsPath), nodeIDMarkerName)); err == nil {
		if id := strings.TrimSpace(string(raw)); id != "" {
			return id
		}
	}
	return ""
}

// SameMachineTier is the local tier's single switch: true only when the
// work item names a node AND this machine holds that node's registration.
// Absent marker, unreadable files, platform rows, and cross-machine rows
// all answer false — the dispatch keeps the R2 path unchanged.
func SameMachineTier(workNodeID string) bool {
	if workNodeID == "" {
		return false
	}
	id := localNodeID()
	return id != "" && id == workNodeID
}

// RunLocalTierUpload builds the tree's services exactly like the image
// tier, then declares the tars as localPath entries instead of uploading
// them. Failure semantics match the image tier — log and return, the row
// stays pending server-side, the next poll retries (docker builds are
// idempotent) — plus the no-fallback rule above.
func RunLocalTierUpload(client Uploader, missionID, deploymentID, repoRoot, nodeArch string, logf Logf) {
	if logf == nil {
		logf = noopLogf
	}
	if err := runLocalTierUpload(client, missionID, deploymentID, repoRoot, nodeArch, logf); err != nil {
		logf("[%s] local-tier upload failed for deployment %s (no R2 fallback — fix and retry): %v",
			logger.ModDeploy, deploymentID, err)
	}
}

// buildSave is the per-service build+docker-save seam (real: buildAndSave),
// a package var for the same reason as dockerProbe: tests stub the daemon
// boundary without a docker install.
var buildSave = buildAndSave

func runLocalTierUpload(client Uploader, missionID, deploymentID, repoRoot, nodeArch string, logf Logf) error {
	if logf == nil {
		logf = noopLogf
	}
	composeRel, err := findComposeOnDisk(repoRoot)
	if err != nil {
		return err
	}
	if err := dockerProbe(); err != nil {
		return fmt.Errorf("docker not usable on this machine (local tier builds locally): %w", err)
	}
	// Exports live under the AGENT's scratch root (the only prefix its store
	// accepts localPath entries from), one dir per deployment, on persistent
	// storage — the tars must outlive this call until the agent materializes
	// them, so /tmp is wrong. Reused across retries like the image tier's
	// scratch (os.Create truncates in place).
	exportDir := filepath.Join(scratchRoot(), "bridge", deploymentID)
	if err := os.MkdirAll(exportDir, 0o755); err != nil {
		return fmt.Errorf("export dir %s (is the hosting-agent scratch root writable by your user? see %s): %w",
			exportDir, scratchRootEnv, err)
	}

	doc, services, err := loadComposeServices(filepath.Join(repoRoot, filepath.FromSlash(composeRel)))
	if err != nil {
		return err
	}
	var toBuild []string
	for _, svc := range services {
		if svc.hasBuild {
			toBuild = append(toBuild, svc.name)
		}
	}
	if len(toBuild) == 0 {
		return fmt.Errorf("compose %s has no `build:` services; the local tier has nothing to build", composeRel)
	}

	files := make([]preview.ManifestFile, 0, len(toBuild)+1)
	for _, svc := range services {
		if !svc.hasBuild {
			continue
		}
		tarPath, size, err := buildSave(repoRoot, composeRel, nodeArch, svc, ImageTag(deploymentID, svc.name), exportDir)
		if err != nil {
			return err
		}
		if size > MaxImageTarBytes {
			return fmt.Errorf("tar for service %s is %d bytes, over the %d byte ceiling; reduce the image",
				svc.name, size, MaxImageTarBytes)
		}
		files = append(files, preview.ManifestFile{
			Path:        svc.name + ".tar",
			SHA256:      fileSHA256(tarPath),
			Size:        size,
			Compression: CompressionGzip,
			// The whole point: the tar stays here and the manifest says so.
			// The API meters the declared size and refuses this mode for any
			// row not placed on this very node.
			LocalPath: tarPath,
		})
		logf("[%s] built service %s for deployment %s (local tier): gz tar %d bytes, staying on this machine",
			logger.ModDeploy, svc.name, deploymentID, size)
	}

	// The rewritten compose is the one member that DOES upload: complete
	// fetches its text from R2 to run compose-policy (tag correspondence and
	// sizing need the real bytes), and it is machine-derived — service
	// names, ports, limits — never user source.
	rewritten, err := os.Create(filepath.Join(exportDir, filepath.Base(composeRel)))
	if err != nil {
		return fmt.Errorf("rewrite compose: %w", err)
	}
	if err := rewriteCompose(doc, deploymentID, rewritten); err != nil {
		rewritten.Close()
		return err
	}
	if err := rewritten.Close(); err != nil {
		return fmt.Errorf("rewrite compose: %w", err)
	}
	rewrittenPath := filepath.Join(exportDir, filepath.Base(composeRel))
	files = append(files, preview.ManifestFile{
		Path:   filepath.Base(composeRel),
		SHA256: fileSHA256(rewrittenPath),
		Size:   fileSize(rewrittenPath),
	})

	// Single-shot declare (an image-tier pack is 2-3 members; chunked never
	// applies and the API rejects localPath on that wire anyway). The only
	// presigned PUTs that can come back are the compose.
	blob := func(path string) ([]byte, error) { return os.ReadFile(filepath.Join(exportDir, path)) }
	decl, err := client.DeclareHostedSource(missionID, deploymentID, toAPIFiles(files), filepath.Base(composeRel), nil)
	if err != nil {
		return fmt.Errorf("declare: %w", err)
	}
	if err := uploadAll(client, decl.Uploads, blob); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	if err := client.CompleteHostedSource(missionID, deploymentID, nil); err != nil {
		return fmt.Errorf("complete: %w", err)
	}
	logger.Info("[%s] image tier declared local for deployment %s (mission %s, node %s): %d service tar(s) never left this machine",
		logger.ModDeploy, deploymentID, missionID, localNodeID(), len(toBuild))
	return nil
}
