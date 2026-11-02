// Image tier (docker-image-deploy-tier): build every `build:`-keyed service
// of the tree's compose file in the LOCAL docker, save each image as a tar,
// rewrite the compose to plain `image:` references, and upload tar+compose
// through the same two-phase declare/complete protocol the source tier uses.
// The source tree itself never leaves the machine — the pack is exactly the
// tars plus one rewritten compose file (design D2/D3).
package deploysource

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/binoctal/open-agents-bridge/internal/logger"
	"github.com/binoctal/open-agents-bridge/internal/preview"
	"gopkg.in/yaml.v3"
)

// MaxImageTarBytes mirrors the platform's MAX_IMAGE_TAR_BYTES
// (apps/api/src/services/compose-policy.ts): the R2 single-PUT ceiling. A
// bigger image cannot ride the declare channel at all, so fail before any
// bytes move rather than mid-upload.
const MaxImageTarBytes = int64(4_831_838_208) // 4.5 GiB

// ImageTag is the local-tag contract between bridge and hosting node:
// build tags, rewritten compose references and checkImageTierRules (the
// platform's complete-time validator) all derive the same string, and the
// <deploymentID> segment keeps concurrent deployments from cross-mounting
// each other's images.
func ImageTag(deploymentID, service string) string {
	// Docker repository names must be lowercase; deployment ids are mixed-case
	// base62 (e.g. MlqbhZR340sM), so fold the whole tag. rmiDeployImages in the
	// hosting-agent folds the same way — keep the two in sync.
	return strings.ToLower("oa-deploy-" + deploymentID + "-" + service)
}

// RunImageTierUpload is the image-tier entry point, dispatched from the same
// pending poll as RunSourceUpload when the row declares tier=image. Failure
// semantics match the source tier exactly: log and return — the row stays
// pending server-side and the next poll retries (docker builds are
// idempotent; re-running a failed service rebuilds only what changed).
//
// Old-bridge skew note: a bridge that predates this function never sees
// Tier on the poll item and runs the source tier instead, so an image row
// degrades to a node-side build rather than breaking.
func RunImageTierUpload(client Uploader, missionID, deploymentID, repoRoot string, logf Logf) {
	if logf == nil {
		logf = noopLogf
	}
	if err := runImageTierUpload(client, missionID, deploymentID, repoRoot, logf); err != nil {
		logf("[%s] image-tier upload failed for deployment %s: %v", logger.ModDeploy, deploymentID, err)
	}
}

func runImageTierUpload(client Uploader, missionID, deploymentID, repoRoot string, logf Logf) error {
	composeRel, err := findComposeOnDisk(repoRoot)
	if err != nil {
		return err
	}
	if err := dockerProbe(); err != nil {
		return fmt.Errorf("docker not usable on this machine (image tier builds locally): %w", err)
	}

	// Scratch space is per-deployment and reused across retries (MkdirAll is
	// idempotent, os.Create truncates in place); cross-deployment leftovers
	// are left to the OS's tmp reaping — this process never rm -rf's.
	scratch := filepath.Join(os.TempDir(), "oa-image-tier", deploymentID)
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		return fmt.Errorf("scratch dir: %w", err)
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
		return fmt.Errorf("compose %s has no `build:` services; the image tier has nothing to build (use the source tier)", composeRel)
	}

	files := make([]preview.ManifestFile, 0, len(toBuild)+1)
	for _, svc := range services {
		if !svc.hasBuild {
			continue
		}
		tarPath, size, err := buildAndSave(repoRoot, composeRel, svc, ImageTag(deploymentID, svc.name), scratch)
		if err != nil {
			return err
		}
		if size > MaxImageTarBytes {
			return fmt.Errorf("tar for service %s is %d bytes, over the %d byte single-PUT ceiling; reduce the image or use the source tier",
				svc.name, size, MaxImageTarBytes)
		}
		files = append(files, preview.ManifestFile{Path: svc.name + ".tar", SHA256: fileSHA256(tarPath), Size: size, Compression: CompressionGzip})
		logf("[%s] built service %s for deployment %s: gz tar %d bytes", logger.ModDeploy, svc.name, deploymentID, size)
	}

	rewritten, err := os.Create(filepath.Join(scratch, filepath.Base(composeRel)))
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
	rewrittenPath := filepath.Join(scratch, filepath.Base(composeRel))
	files = append(files, preview.ManifestFile{
		Path:   filepath.Base(composeRel),
		SHA256: fileSHA256(rewrittenPath),
		Size:   fileSize(rewrittenPath),
	})

	// Two or three members never need the chunked path; the single-shot
	// declare (assemble manifest server-side) is the whole protocol here.
	blob := func(path string) ([]byte, error) { return os.ReadFile(filepath.Join(scratch, path)) }
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
	logger.Info("[%s] image tier uploaded for deployment %s (mission %s): %d service tar(s) + rewritten %s",
		logger.ModDeploy, deploymentID, missionID, len(toBuild), filepath.Base(composeRel))
	return nil
}

// dockerProbe fail-fasts before any build work: no daemon or no permission
// means the image tier cannot run on this machine, and silently falling back
// to the source tier would upload the tree the user chose to keep local. A
// package variable so tests can stub the daemon boundary.
var dockerProbe = func() error {
	cmd := exec.Command("docker", "version", "--format", "{{.Server.Version}}")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker version: %v: %s", err, tailOutput(out, 200))
	}
	return nil
}

// findComposeOnDisk is the on-disk counterpart of DetectComposePath: same
// convention table, same order, but probing the filesystem directly because
// the image tier never packs a manifest to detect through.
func findComposeOnDisk(root string) (string, error) {
	for _, p := range composeConventionPaths {
		if st, err := os.Stat(filepath.Join(root, filepath.FromSlash(p))); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("no convention-path compose file in %s (tried %s)", root, strings.Join(composeConventionPaths, ", "))
}

// composeService is one entry of the services map, split out while scanning
// the yaml node tree.
type composeService struct {
	name     string
	node     *yaml.Node // the service's mapping node, for build translation
	hasBuild bool
	hasImage bool
}

// loadComposeServices parses the compose into a yaml.Node document (comments,
// anchors and sibling keys survive untouched) and lists its services.
func loadComposeServices(path string) (*yaml.Node, []composeService, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read compose: %w", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, nil, fmt.Errorf("parse compose %s: %w", path, err)
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("compose %s is not a mapping", path)
	}
	servicesNode := mappingValue(root, "services")
	if servicesNode == nil || servicesNode.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("compose %s has no services mapping", path)
	}
	var out []composeService
	for i := 0; i+1 < len(servicesNode.Content); i += 2 {
		name := servicesNode.Content[i].Value
		body := servicesNode.Content[i+1]
		out = append(out, composeService{
			name:     name,
			node:     body,
			hasBuild: mappingValue(body, "build") != nil,
			hasImage: mappingValue(body, "image") != nil,
		})
	}
	return &doc, out, nil
}

// mappingValue returns the value node for key in a mapping node, nil when
// absent.
func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// buildAndSave runs docker build for one service (translating the compose
// `build:` key — string context or {context, dockerfile, args, target}) and
// docker-saves the tagged image into scratch/<service>.tar, gzip-compressed.
func buildAndSave(repoRoot, composeRel string, svc composeService, tag, scratch string) (string, int64, error) {
	args, err := buildArgs(repoRoot, filepath.Dir(filepath.Join(repoRoot, filepath.FromSlash(composeRel))), mappingValue(svc.node, "build"))
	if err != nil {
		return "", 0, err
	}
	args = append(args, "-t", tag)
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		return "", 0, fmt.Errorf("docker build %s: %v: %s", svc.name, err, tailOutput(out, 400))
	}
	tarPath := filepath.Join(scratch, svc.name+".tar")
	save := exec.Command("docker", "save", tag)
	size, err := saveGzipped(save, tarPath, svc.name)
	if err != nil {
		return "", 0, err
	}
	return tarPath, size, nil
}

// saveGzipped streams cmd's stdout through gzip (BestSpeed) into tarPath and
// returns the compressed size (image-tier-gzip-and-default D1/D3). The PATH
// keeps the .tar suffix — the three-end contract keys off it — while the
// bytes compress; docker load -i sniffs the gzip magic by content, so every
// deployed agent loads these unchanged and old-bridge plain tars stay legal
// forever. BestSpeed: docker layers are mostly pre-compressed binaries, so
// higher levels buy <10% ratio at ~3x the wall clock. Split from
// buildAndSave so the pipe contract (gzip magic on disk, size = compressed
// bytes) is unit-testable without a docker daemon.
func saveGzipped(cmd *exec.Cmd, tarPath, svcName string) (int64, error) {
	f, err := os.Create(tarPath)
	if err != nil {
		return 0, fmt.Errorf("docker save %s: %v", svcName, err)
	}
	gz, err := gzip.NewWriterLevel(f, gzip.BestSpeed)
	if err != nil {
		f.Close()
		return 0, fmt.Errorf("docker save %s: %v", svcName, err)
	}
	cmd.Stdout = gz
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	// Close in pipe order so every buffered byte lands even on failure; the
	// partial file only matters for the error tail.
	gzErr := gz.Close()
	fErr := f.Close()
	if runErr != nil {
		return 0, fmt.Errorf("docker save %s: %v: %s", svcName, runErr, tailOutput(errBuf.Bytes(), 400))
	}
	if gzErr != nil {
		return 0, fmt.Errorf("docker save %s: gzip: %v", svcName, gzErr)
	}
	if fErr != nil {
		return 0, fmt.Errorf("docker save %s: %v", svcName, fErr)
	}
	return fileSize(tarPath), nil
}

// buildArgs translates a compose `build:` key into a `docker build` argv
// prefix. Supported: string form (`build: ./dir`), and mapping form with
// context / dockerfile / args / target. Sibling keys the compose spec allows
// (labels, cache_from, platforms...) are ignored for v1 — plain single-arch
// builds cover the dang-node path (design OQ3: builder and node arch must
// match anyway, so multi-arch machinery buys nothing yet).
func buildArgs(repoRoot, composeDir string, build *yaml.Node) ([]string, error) {
	if build == nil {
		return nil, fmt.Errorf("service has no build key")
	}
	var argv []string
	switch build.Kind {
	case yaml.ScalarNode: // build: ./context
		ctx, err := resolveContext(repoRoot, composeDir, build.Value)
		if err != nil {
			return nil, err
		}
		argv = []string{"build", ctx}
	case yaml.MappingNode:
		context := "."
		if n := mappingValue(build, "context"); n != nil {
			var err error
			if context, err = resolveContext(repoRoot, composeDir, n.Value); err != nil {
				return nil, err
			}
		}
		argv = []string{"build", context}
		if n := mappingValue(build, "dockerfile"); n != nil {
			// Compose resolves dockerfile against the CONTEXT, but docker -f
			// resolves against the daemon's cwd — join the (absolute) context
			// so `dockerfile: app/Dockerfile` works from any cwd.
			argv = append(argv, "-f", filepath.Join(context, filepath.FromSlash(n.Value)))
		}
		if n := mappingValue(build, "target"); n != nil {
			argv = append(argv, "--target", n.Value)
		}
		if args := mappingValue(build, "args"); args != nil && args.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(args.Content); i += 2 {
				k := args.Content[i].Value
				v := args.Content[i+1]
				if v.Tag == "!!null" {
					argv = append(argv, "--build-arg", k) // pull from the daemon's env
					continue
				}
				argv = append(argv, "--build-arg", k+"="+v.Value)
			}
		}
	default:
		return nil, fmt.Errorf("unsupported `build:` key shape (line %d)", build.Line)
	}
	return argv, nil
}

// resolveContext resolves a compose build context against the compose file's
// directory (compose semantics), then makes it absolute so the docker build
// runs from any cwd.
func resolveContext(repoRoot, composeDir, context string) (string, error) {
	context = filepath.Join(composeDir, filepath.FromSlash(context))
	return filepath.Abs(context)
}

// rewriteCompose re-emits the compose document with every `build:` key
// replaced by `image: <ImageTag>` (an existing `image:` alongside `build:` —
// rare but legal — is overwritten; registry-image services keep their
// reference untouched). Node-level rewrite keeps profiles, depends_on,
// healthchecks, comments and key order exactly as authored.
func rewriteCompose(doc *yaml.Node, deploymentID string, out io.Writer) error {
	services := mappingValue(doc.Content[0], "services")
	for i := 0; i+1 < len(services.Content); i += 2 {
		name := services.Content[i].Value
		body := services.Content[i+1]
		buildIdx := mappingIndex(body, "build")
		if buildIdx < 0 {
			continue
		}
		// Drop key and value nodes of `build`.
		body.Content = append(body.Content[:buildIdx], body.Content[buildIdx+2:]...)
		imageIdx := mappingIndex(body, "image")
		tag := ImageTag(deploymentID, name)
		if imageIdx >= 0 {
			setScalar(body.Content[imageIdx+1], tag) // index hits the KEY node; +1 is its value
		} else {
			body.Content = append(body.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "image"},
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: tag})
		}
	}
	enc := yaml.NewEncoder(out)
	defer enc.Close()
	return enc.Encode(doc.Content[0])
}

func mappingIndex(node *yaml.Node, key string) int {
	if node == nil || node.Kind != yaml.MappingNode {
		return -1
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return i
		}
	}
	return -1
}

func setScalar(node *yaml.Node, value string) {
	node.Kind = yaml.ScalarNode
	node.Tag = "!!str"
	node.Value = value
	node.Style = 0
}

// fileSHA256 hashes streaming: an image tar is multi-GiB and reading it into
// memory just to hash would double the peak footprint of an already large
// pipeline (the PUT leg is bytes-based by protocol; hashing need not be).
func fileSHA256(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// tailOutput keeps the last n bytes of a failed command's output — docker
// build failures scroll their real error past the top, and the fail log must
// carry it (spec: fail log carries the failing service and the tail of the
// docker output).
func tailOutput(out []byte, n int) string {
	s := strings.TrimSpace(string(bytes.TrimSpace(out)))
	if len(s) > n {
		s = "..." + s[len(s)-n:]
	}
	return s
}
