// Sandbox work-item handlers (add-cloud-executor 4.1). sandbox_up runs the
// digest-pinned runner image under runsc on the locked-down sbx-net with
// cgroup limits generated from the size class; sandbox_destroy force-removes
// the container by its fixed name. Both are idempotent by that fixed name:
// a re-offer after a lost report collides with the existing container, which
// is recovered (up) or a no-op (destroy), never a duplicate.
package agent

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/binoctal/open-agents/hosting-agent/internal/api"
)

// Sandbox hard limits from the security baseline (task group 0): the runtime
// and network are what the egress-lockdown rules and runsc registration were
// built around; pids-limit was the fork-bomb ceiling verified in 0.4.
const (
	sandboxRuntime   = "runsc"
	sandboxNetwork   = "sbx-net"
	sandboxPidsLimit = "64"
	// resolv.conf mounted over the container's own: on user-defined networks
	// docker forces 127.0.0.11 (its embedded DNS proxy) into resolv.conf, and
	// gVisor's netstack cannot reach that proxy -- every getaddrinfo inside
	// the box then dies with EAI_AGAIN (verified live: direct queries to real
	// resolvers succeed, 127.0.0.11 is ECONNREFUSED, and --dns only changes
	// what the unreachable proxy would forward to). The file is created by
	// ensureSandboxResolvConf from SANDBOX_DNS_SERVERS if absent.
	sandboxResolvConf = "/etc/hosting-agent/sbx-resolv.conf"
)

// ensureSandboxResolvConf is a seam so tests never touch the real path.
var ensureSandboxResolvConf = func() error { return writeSandboxResolvConf(sandboxResolvConf) }

// writeSandboxResolvConf materializes the mounted resolv.conf when missing.
// SANDBOX_DNS_SERVERS is a comma-separated list; default 1.1.1.1 (public,
// and the egress-lockdown rules on sbx-net already allow udp/tcp 53 out).
func writeSandboxResolvConf(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	servers := os.Getenv("SANDBOX_DNS_SERVERS")
	if servers == "" {
		servers = "1.1.1.1"
	}
	var b strings.Builder
	for _, s := range strings.Split(servers, ",") {
		if s = strings.TrimSpace(s); s != "" {
			fmt.Fprintf(&b, "nameserver %s\n", s)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// dockerRunner is the exec seam for sandbox container commands. The default
// runs the real docker CLI; tests inject a recorder.
type dockerRunner func(ctx context.Context, args ...string) (string, error)

func realDocker(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), fmt.Errorf("docker %s: %s: %s",
				strings.Join(args[:min(2, len(args))], " "), err, strings.TrimSpace(string(ee.Stderr)))
		}
		return string(out), err
	}
	return string(out), nil
}

func (a *Agent) docker() dockerRunner {
	if a.dockerRun == nil {
		return realDocker
	}
	return a.dockerRun
}

// sandboxUpArgs builds the docker run argument list from the work item. The
// plaintext task token rides only the container env (-e), never a label or
// the command line of a long-lived process someone could ps. imageName is the
// digest-verified local name (ImageRef minus its @sha256 pin -- a
// locally-built image is not addressable by its config digest, so the name is
// what docker accepts; the pin was already checked against it).
// lifecycleInteractiveSession is the work-item lifecycleMode of a cloud
// interactive session (task sandboxes leave it empty or "task").
const lifecycleInteractiveSession = "interactive-session"

func sandboxUpArgs(item api.WorkItem, imageName string) []string {
	sc := item.SizeClass
	args := []string{
		"run", "-d",
		"--name", item.ContainerName,
		"--runtime", sandboxRuntime,
		"--network", sandboxNetwork,
		"--memory", fmt.Sprintf("%dm", sc.MemoryMiB),
		"--cpus", fmt.Sprintf("%g", sc.VCPU),
		"--pids-limit", sandboxPidsLimit,
		// mode=1777 is explicit because the runner executes as the
		// unprivileged node user: a root-owned 0755 tmpfs would make the
		// CLI's HOME=/tmp state writes fail.
		"--tmpfs", fmt.Sprintf("/tmp:size=%dm,rw,noexec,mode=1777", sc.StorageMiB),
		// Real resolvers, not the unreachable 127.0.0.11 (see sandboxResolvConf).
		"-v", sandboxResolvConf + ":/etc/resolv.conf:ro",
		"-e", "API_BASE_URL=" + item.APIBaseURL,
	}
	if item.LifecycleMode == lifecycleInteractiveSession {
		// An interactive session container holds ONLY its sst_ credential and the
		// shadow identity it connects as; it never receives a task token or any
		// mission/task context, so a leaked env cannot touch task endpoints.
		args = append(args,
			"-e", "SESSION_TOKEN="+item.SessionToken,
			"-e", "SESSION_ID="+item.SessionID,
			"-e", "USER_ID="+item.UserID,
			"-e", "SHADOW_DEVICE_ID="+item.ExecutorID,
		)
		if item.IdleReclaimSeconds > 0 {
			args = append(args, "-e", "IDLE_RECLAIM_SECONDS="+strconv.Itoa(item.IdleReclaimSeconds))
		}
		if item.MaxLifetimeSeconds > 0 {
			args = append(args, "-e", "MAX_LIFETIME_SECONDS="+strconv.Itoa(item.MaxLifetimeSeconds))
		}
	} else {
		args = append(args,
			"-e", "TASK_TOKEN="+item.TaskToken,
			"-e", "MISSION_ID="+item.MissionID,
			"-e", "TASK_ID="+item.TaskID,
			"-e", "TASK_PROMPT="+item.TaskPrompt,
		)
	}
	// The dispatch generation: the runner echoes it back on task-result /
	// task-error so the callback can pair the report with the attempt that
	// ran it (G19). Omitted on 0 (payload-less item) -- the runner then
	// soft-passes, same as pre-attempt bridge runners.
	if item.Attempt > 0 {
		args = append(args, "-e", "ATTEMPT="+strconv.Itoa(item.Attempt))
	}
	// The mission's catalog model: the runner CLI passes it as --model so its
	// LLM calls name a model the outlet's catalog accepts. Omitted when empty
	// (payload-less item) -- the runner then falls back to the CLI default.
	if item.Model != "" {
		args = append(args, "-e", "MODEL="+item.Model)
	}
	// The sandbox's wall-clock deadline: absolute epoch ms so the runner
	// derives its CLI kill time from the authoritative window (deadline minus
	// a reporting margin) rather than a hardcoded 25min decoupled from it.
	// Skipped when absent (older API): the runner then falls back to 25min.
	if item.TimeoutAt != "" {
		if t, err := time.Parse(time.RFC3339, item.TimeoutAt); err == nil {
			args = append(args, "-e", "SANDBOX_DEADLINE_EPOCH_MS="+strconv.FormatInt(t.UnixMilli(), 10))
		}
	}
	return append(args,
		"--pull", "never", // digest-pinned image must be pre-pulled (task 4.3); never float
		imageName,
	)
}

// splitDigestRef splits "name@sha256:<64 lowercase hex>" into its parts.
// ok=false when the ref is not strictly digest-pinned: a floating ref must
// never run, and neither must a truncated or uppercase digest -- the supply
// chain pins the EXACT image the API vetted, not something that looks like it.
func splitDigestRef(ref string) (name, digest string, ok bool) {
	const marker = "@sha256:"
	at := strings.LastIndex(ref, marker)
	if at <= 0 {
		return "", "", false
	}
	d := ref[at+len(marker):]
	if len(d) != 64 {
		return "", "", false
	}
	for _, c := range d {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return "", "", false
		}
	}
	return ref[:at], d, true
}

// verifySandboxImage pins the local image to the work item's digest before
// anything runs. The pin is the image CONFIG digest (`docker image inspect
// --format {{.Id}}`) because the runner image is built locally on the node
// (design D8: local build + recorded digest -- a locally built image has no
// RepoDigest to compare against). Mismatch means the tag was re-pointed by a
// rebuild the API never vetted: refuse loudly, never run what arrived.
// Returns the digest-stripped name for the docker run.
func (a *Agent) verifySandboxImage(ctx context.Context, item api.WorkItem) (string, error) {
	name, want, ok := splitDigestRef(item.ImageRef)
	if !ok {
		return "", fmt.Errorf("image ref must be digest-pinned (name@sha256:<64hex>): %q", item.ImageRef)
	}
	out, err := a.docker()(ctx, "image", "inspect", "--format", "{{.Id}}", name)
	if err != nil {
		return "", fmt.Errorf("runner image %s not present locally (pre-pull/build required): %v", name, err)
	}
	local := strings.ToLower(strings.TrimSpace(out))
	if local != "sha256:"+want {
		return "", fmt.Errorf("image digest mismatch: local %s, pinned sha256:%s", local, want)
	}
	return name, nil
}

// handleSandboxUp starts the sandbox container and reports sandbox_up with
// its container id. A name conflict means the container is already up under
// this fixed name -- a re-offer after a lost report -- so the existing id is
// inspected and reported instead of failing.
func (a *Agent) handleSandboxUp(ctx context.Context, ep APIEndpoint, item api.WorkItem) error {
	// Reports go to the offering endpoint (D2); a 401 retires it (D3).
	report := func(event, containerID, detail string) error {
		if err := ep.Client.ReportSandbox(ctx, item.SandboxID, event, containerID, detail); err != nil {
			if api.Unauthorized(err) {
				a.disableEndpoint(ep)
			} else {
				a.logf("sandbox %s: report %s failed: %v", item.SandboxID, event, err)
			}
		}
		return nil
	}
	if item.SizeClass == nil || item.ImageRef == "" || item.ContainerName == "" {
		a.logf("sandbox %s: up item missing payload (sizeClass/imageRef/containerName)", item.SandboxID)
		return report(api.EventSandboxUpFailed, "", "malformed sandbox_up item")
	}
	if item.LifecycleMode == lifecycleInteractiveSession {
		if item.SessionToken == "" || item.SessionID == "" || item.ExecutorID == "" || item.UserID == "" {
			a.logf("sandbox %s: session up item missing payload (sessionToken/sessionId/executorId/userId)", item.SandboxID)
			return report(api.EventSandboxUpFailed, "", "malformed sandbox_up item")
		}
	} else if item.TaskToken == "" || item.TaskPrompt == "" {
		a.logf("sandbox %s: up item missing payload (taskToken/taskPrompt)", item.SandboxID)
		return report(api.EventSandboxUpFailed, "", "malformed sandbox_up item")
	}

	// Supply-chain gate (spec: digest mismatch refuses start): the pin is
	// checked BEFORE docker run so a re-pointed tag never executes.
	imageName, err := a.verifySandboxImage(ctx, item)
	if err != nil {
		a.logf("sandbox %s: refusing start: %v", item.SandboxID, err)
		return report(api.EventSandboxUpFailed, "", err.Error())
	}

	// The resolv.conf bind-mount target must exist or docker run fails on the
	// mount itself; fail early with a reason the API can surface instead.
	if err := ensureSandboxResolvConf(); err != nil {
		a.logf("sandbox %s: refusing start: %v", item.SandboxID, err)
		return report(api.EventSandboxUpFailed, "", fmt.Sprintf("resolv.conf: %v", err))
	}

	out, err := a.docker()(ctx, sandboxUpArgs(item, imageName)...)
	if err != nil {
		if strings.Contains(err.Error(), "already in use") || strings.Contains(out, "already in use") {
			// The container exists: recover its id and treat this as up.
			id, ierr := a.docker()(ctx, "inspect", "--format", "{{.Id}}", item.ContainerName)
			if ierr == nil && strings.TrimSpace(id) != "" {
				a.logf("sandbox %s: container %s already up (re-offer), id %s",
					item.SandboxID, item.ContainerName, shortID(strings.TrimSpace(id)))
				return report(api.EventSandboxUp, strings.TrimSpace(id), "")
			}
			a.logf("sandbox %s: inspect after name conflict failed: %v", item.SandboxID, ierr)
			// A conflicting name with no inspectable container is stuck state;
			// the API re-offers, and the destroy path clears it by name.
			return report(api.EventSandboxUpFailed, "", fmt.Sprintf("container name conflict, inspect failed: %v", ierr))
		}
		a.logf("sandbox %s: docker run failed: %v", item.SandboxID, err)
		return report(api.EventSandboxUpFailed, "", err.Error())
	}
	containerID := strings.TrimSpace(out)
	a.logf("sandbox %s: container %s up (image %s)", item.SandboxID, shortID(containerID), item.ImageRef)
	return report(api.EventSandboxUp, containerID, "")
}

// Workspace artifact export (cloud-session-shadow-device 4.5). The container's
// workspace is the only user data a session sandbox holds; everything under
// HOME=/tmp is a noexec tmpfs that dies with it by design. The export runs
// BEFORE `docker rm` and streams `docker cp` through gzip into a capped temp
// file, so an oversized workspace fails the export instead of eating the node.
const (
	artifactMaxBytes = 1 << 30 // 1 GiB compressed cap
)

// dockerCpTar is the streaming seam for `docker cp <c>:/workspace -`. It is
// NOT the dockerRunner seam: cp must stream megabytes to disk, never buffer
// them in a (string, error).
var dockerCpTar = func(ctx context.Context, containerName string, w io.Writer) error {
	cmd := exec.CommandContext(ctx, "docker", "cp", containerName+":/workspace", "-")
	cmd.Stdout = w
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker cp: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// cappedWriter fails the export once past the byte ceiling instead of letting
// a runaway workspace fill the node disk.
type cappedWriter struct {
	w        io.Writer
	written  int64
	max      int64
	exceeded bool
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c.written+int64(len(p)) > c.max {
		c.exceeded = true
		return 0, fmt.Errorf("artifact export exceeds %d bytes cap", c.max)
	}
	n, err := c.w.Write(p)
	c.written += int64(n)
	return n, err
}

// exportWorkspaceArtifact uploads the container's /workspace as a tar.gz to
// the item's presigned URL. Returns the artifactsStatus for the destroy
// report: "available" or "failed". Failure never blocks the destroy itself --
// the workspace is destroyed either way (the spec promise is the ORDER, not a
// guaranteed export).
func (a *Agent) exportWorkspaceArtifact(ctx context.Context, item api.WorkItem) string {
	f, err := os.CreateTemp("", "sbx-artifact-*.tar.gz")
	if err != nil {
		a.logf("sandbox %s: artifact temp file: %v", item.SandboxID, err)
		return "failed"
	}
	defer os.Remove(f.Name())
	defer f.Close()

	capped := &cappedWriter{w: f, max: artifactMaxBytes}
	gz := gzip.NewWriter(capped)
	cpErr := dockerCpTar(ctx, item.ContainerName, gz)
	gzErr := gz.Close()
	if cpErr != nil || gzErr != nil || capped.exceeded {
		a.logf("sandbox %s: artifact export failed: cp=%v gzip=%v capped=%v",
			item.SandboxID, cpErr, gzErr, capped.exceeded)
		return "failed"
	}
	size, err := f.Seek(0, io.SeekStart)
	if err != nil {
		a.logf("sandbox %s: artifact seek: %v", item.SandboxID, err)
		return "failed"
	}
	if err := a.Client.PutArtifact(ctx, item.ArtifactUploadURL, f, size); err != nil {
		a.logf("sandbox %s: artifact upload failed: %v", item.SandboxID, err)
		return "failed"
	}
	a.logf("sandbox %s: workspace artifact uploaded (%d bytes)", item.SandboxID, size)
	return "available"
}

// handleSandboxDestroy force-removes the container. "No such container" is
// success -- the box is gone, which is all this action promises. For an
// interactive session with an artifact URL, the workspace is exported first
// and the outcome rides the destroy report (4.5).
func (a *Agent) handleSandboxDestroy(ctx context.Context, ep APIEndpoint, item api.WorkItem) error {
	report := func(event, detail, artifacts string) error {
		if err := ep.Client.ReportSandboxOpts(ctx, item.SandboxID, event, "", detail, artifacts); err != nil {
			if api.Unauthorized(err) {
				a.disableEndpoint(ep)
			} else {
				a.logf("sandbox %s: report %s failed: %v", item.SandboxID, event, err)
			}
		}
		return nil
	}
	if item.ContainerName == "" {
		return report(api.EventSandboxDestroyFailed, "malformed sandbox_destroy item", "")
	}
	artifacts := ""
	if item.LifecycleMode == lifecycleInteractiveSession && item.ArtifactUploadURL != "" {
		artifacts = a.exportWorkspaceArtifact(ctx, item)
	}
	_, err := a.docker()(ctx, "rm", "-f", item.ContainerName)
	if err == nil {
		a.logf("sandbox %s: container %s removed", item.SandboxID, item.ContainerName)
		return report(api.EventSandboxDestroy, "", artifacts)
	}
	if strings.Contains(err.Error(), "No such") || strings.Contains(err.Error(), "no such") {
		a.logf("sandbox %s: container %s already gone", item.SandboxID, item.ContainerName)
		// A container that vanished before the export ran still reports the
		// export outcome: "failed" when nothing could be collected.
		if artifacts == "" && item.LifecycleMode == lifecycleInteractiveSession && item.ArtifactUploadURL != "" {
			artifacts = "failed"
		}
		return report(api.EventSandboxDestroy, "", artifacts)
	}
	a.logf("sandbox %s: docker rm failed: %v", item.SandboxID, err)
	return report(api.EventSandboxDestroyFailed, err.Error(), "")
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
