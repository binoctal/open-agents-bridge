// Sandbox work-item handlers (add-cloud-executor 4.1). sandbox_up runs the
// digest-pinned runner image under runsc on the locked-down sbx-net with
// cgroup limits generated from the size class; sandbox_destroy force-removes
// the container by its fixed name. Both are idempotent by that fixed name:
// a re-offer after a lost report collides with the existing container, which
// is recovered (up) or a no-op (destroy), never a duplicate.
package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/binoctal/open-agents/hosting-agent/internal/api"
)

// Sandbox hard limits from the security baseline (task group 0): the runtime
// and network are what the egress-lockdown rules and runsc registration were
// built around; pids-limit was the fork-bomb ceiling verified in 0.4.
const (
	sandboxRuntime   = "runsc"
	sandboxNetwork   = "sbx-net"
	sandboxPidsLimit = "64"
)

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
// the command line of a long-lived process someone could ps.
func sandboxUpArgs(item api.WorkItem) []string {
	sc := item.SizeClass
	return []string{
		"run", "-d",
		"--name", item.ContainerName,
		"--runtime", sandboxRuntime,
		"--network", sandboxNetwork,
		"--memory", fmt.Sprintf("%dm", sc.MemoryMiB),
		"--cpus", fmt.Sprintf("%g", sc.VCPU),
		"--pids-limit", sandboxPidsLimit,
		"--tmpfs", fmt.Sprintf("/tmp:size=%dm,rw,noexec", sc.StorageMiB),
		"-e", "TASK_TOKEN=" + item.TaskToken,
		"-e", "API_BASE_URL=" + item.APIBaseURL,
		"-e", "MISSION_ID=" + item.MissionID,
		"-e", "TASK_ID=" + item.TaskID,
		"--pull", "never", // digest-pinned image must be pre-pulled (task 4.3); never float
		item.ImageRef,
	}
}

// handleSandboxUp starts the sandbox container and reports sandbox_up with
// its container id. A name conflict means the container is already up under
// this fixed name -- a re-offer after a lost report -- so the existing id is
// inspected and reported instead of failing.
func (a *Agent) handleSandboxUp(ctx context.Context, item api.WorkItem) error {
	report := func(event, containerID, detail string) error {
		if err := a.Client.ReportSandbox(ctx, item.SandboxID, event, containerID, detail); err != nil {
			if api.Unauthorized(err) {
				return err
			}
			a.logf("sandbox %s: report %s failed: %v", item.SandboxID, event, err)
		}
		return nil
	}
	if item.SizeClass == nil || item.ImageRef == "" || item.ContainerName == "" || item.TaskToken == "" {
		a.logf("sandbox %s: up item missing payload (sizeClass/imageRef/containerName/taskToken)", item.SandboxID)
		return report(api.EventSandboxUpFailed, "", "malformed sandbox_up item")
	}

	out, err := a.docker()(ctx, sandboxUpArgs(item)...)
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

// handleSandboxDestroy force-removes the container. "No such container" is
// success -- the box is gone, which is all this action promises.
func (a *Agent) handleSandboxDestroy(ctx context.Context, item api.WorkItem) error {
	report := func(event, detail string) error {
		if err := a.Client.ReportSandbox(ctx, item.SandboxID, event, "", detail); err != nil {
			if api.Unauthorized(err) {
				return err
			}
			a.logf("sandbox %s: report %s failed: %v", item.SandboxID, event, err)
		}
		return nil
	}
	if item.ContainerName == "" {
		return report(api.EventSandboxDestroyFailed, "malformed sandbox_destroy item")
	}
	_, err := a.docker()(ctx, "rm", "-f", item.ContainerName)
	if err == nil {
		a.logf("sandbox %s: container %s removed", item.SandboxID, item.ContainerName)
		return report(api.EventSandboxDestroy, "")
	}
	if strings.Contains(err.Error(), "No such") || strings.Contains(err.Error(), "no such") {
		a.logf("sandbox %s: container %s already gone", item.SandboxID, item.ContainerName)
		return report(api.EventSandboxDestroy, "")
	}
	a.logf("sandbox %s: docker rm failed: %v", item.SandboxID, err)
	return report(api.EventSandboxDestroyFailed, err.Error())
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
