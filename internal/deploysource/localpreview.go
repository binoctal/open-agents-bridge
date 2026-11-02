// Local compose preview (local-compose-preview): run the mission's compose
// stack on the USER'S OWN machine for a look-don't-host preview and report
// localhost/LAN URLs. The platform stores one metadata row per mission and
// touches no Coolify app, no R2 tree, no billing -- this package is the whole
// bridge half: find the compose file, `docker compose up -d --wait` (or
// `stop`, containers kept), pick the port, assemble URLs, report.
//
// Failure semantics follow RunSourceUpload: log and return. The row keeps its
// pre-report state server-side, the next poll re-offers the item, and both
// verbs are idempotent on an already-settled stack — recovery is re-run, not
// retry bookkeeping (design D2: no lease state machine on the bridge side).
package deploysource

import (
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/binoctal/open-agents-bridge/internal/api"
	"github.com/binoctal/open-agents-bridge/internal/logger"
)

// LocalPreviewReporter is the subset of *api.Client this package needs, kept
// local so tests stub exactly the report boundary.
type LocalPreviewReporter interface {
	ReportLocalPreview(missionID string, report api.LocalPreviewReport) error
}

// maxLANAddresses caps the LAN tail of the URL list: a laptop on VPN plus
// dock plus wifi can enumerate a silly number of interfaces, and the card
// lists them all for the user to pick from — a cap keeps the report readable
// without hiding the common cases.
const maxLANAddresses = 8

// Docker command seams (package vars so tests stub the daemon boundary,
// same posture as dockerProbe in imagebuild.go).
var (
	composeRun = func(composeAbs string, args ...string) ([]byte, error) {
		return exec.Command("docker", append([]string{"compose", "-f", composeAbs}, args...)...).CombinedOutput()
	}
)

// RunLocalPreview executes one poll item. It ALWAYS reports a settle status
// except when the report send itself fails (then the row stays claimable and
// the next tick redoes the work).
func RunLocalPreview(client LocalPreviewReporter, work api.LocalPreviewWork, repoRoot string, logf Logf) {
	if logf == nil {
		logf = noopLogf
	}
	switch work.Action {
	case "start":
		runLocalPreviewStart(client, work, repoRoot, logf)
	case "stop":
		runLocalPreviewStop(client, work, repoRoot, logf)
	default:
		logf("[%s] local preview: unknown action %q for mission %s", logger.ModDeploy, work.Action, work.MissionID)
	}
}

// failLocalPreview reports the failed settle; a send failure only logs (the
// row stays pending server-side and the next poll re-offers it).
func failLocalPreview(client LocalPreviewReporter, missionID, reason, logTail string, logf Logf) {
	report := api.LocalPreviewReport{Status: "failed", FailReason: reason}
	if logTail != "" {
		report.FailLogTail = logTail
	}
	if err := client.ReportLocalPreview(missionID, report); err != nil {
		logf("[%s] local preview: report failed for mission %s: %v", logger.ModDeploy, missionID, err)
	}
	logf("[%s] local preview: mission %s failed: %s", logger.ModDeploy, missionID, reason)
}

func runLocalPreviewStart(client LocalPreviewReporter, work api.LocalPreviewWork, repoRoot string, logf Logf) {
	composeRel, err := findComposeOnDisk(repoRoot)
	if err != nil {
		// spec: no compose file in the tree is a settle, not a skip — the
		// card must tell the user their mission has nothing to preview.
		failLocalPreview(client, work.MissionID, err.Error(), "", logf)
		return
	}
	composeAbs := joinComposeAbs(repoRoot, composeRel)
	if err := dockerProbe(); err != nil {
		// spec: docker missing/unreachable is a settle with the reason said
		// plainly — the user's machine, the user's fix.
		failLocalPreview(client, work.MissionID, fmt.Sprintf("docker not usable on this machine: %v", err), "", logf)
		return
	}

	out, err := composeRun(composeAbs, "up", "-d", "--wait")
	if err != nil {
		failLocalPreview(client, work.MissionID,
			fmt.Sprintf("docker compose up failed: %v", err), tailOutput(out, 4000), logf)
		return
	}

	port, err := pickPreviewPort(composeAbs, work.IngressService, work.IngressPort)
	if err != nil {
		// The stack IS up; failing here would tell the user it broke when it
		// didn't. Fail with the port story so they can still look manually.
		failLocalPreview(client, work.MissionID, err.Error(), "", logf)
		return
	}
	urls := previewURLs(port)
	logf("[%s] local preview: mission %s stack up via %s -> port %d (%d url(s))",
		logger.ModDeploy, work.MissionID, composeRel, port, len(urls))
	report := api.LocalPreviewReport{Status: "running", URLs: urls, ComposePath: composeRel}
	if err := client.ReportLocalPreview(work.MissionID, report); err != nil {
		logf("[%s] local preview: report failed for mission %s: %v", logger.ModDeploy, work.MissionID, err)
	}
}

func runLocalPreviewStop(client LocalPreviewReporter, work api.LocalPreviewWork, repoRoot string, logf Logf) {
	composeRel, err := findComposeOnDisk(repoRoot)
	if err != nil {
		// Nothing to stop that this bridge can name — settle as failed so the
		// stop_requested row does not hang forever waiting for a machine that
		// will never find a compose file.
		failLocalPreview(client, work.MissionID, err.Error(), "", logf)
		return
	}
	if err := dockerProbe(); err != nil {
		failLocalPreview(client, work.MissionID, fmt.Sprintf("docker not usable on this machine: %v", err), "", logf)
		return
	}
	// stop keeps containers (design D6): a later start is `up -d` on the same
	// containers — volumes and images reused, no rebuild cost.
	out, err := composeRun(joinComposeAbs(repoRoot, composeRel), "stop")
	if err != nil {
		failLocalPreview(client, work.MissionID,
			fmt.Sprintf("docker compose stop failed: %v", err), tailOutput(out, 4000), logf)
		return
	}
	logf("[%s] local preview: mission %s stack stopped (containers kept)", logger.ModDeploy, work.MissionID)
	if err := client.ReportLocalPreview(work.MissionID, api.LocalPreviewReport{Status: "stopped"}); err != nil {
		logf("[%s] local preview: report failed for mission %s: %v", logger.ModDeploy, work.MissionID, err)
	}
}

func joinComposeAbs(repoRoot, composeRel string) string {
	joined := filepath.Join(repoRoot, filepath.FromSlash(composeRel))
	if abs, err := filepath.Abs(joined); err == nil {
		return abs
	}
	return joined
}

// composePsEntry is the slice of `docker compose ps --format json` we care
// about (compose v2 shape; extra members ignored by the decoder).
type composePsEntry struct {
	Service     string `json:"Service"`
	Name        string `json:"Name"`
	Publishers  []struct {
		URL           string `json:"URL"`
		TargetPort    int    `json:"TargetPort"`
		PublishedPort int    `json:"PublishedPort"`
	} `json:"Publishers"`
}

// pickPreviewPort implements the D3 priority: the declared ingress service
// (and its container port, when declared) first, then the compose host port
// mappings. Among the leftovers, a single distinct host port needs no
// guessing; several mean ambiguity, resolved by the lowest port and named in
// the error-free log — the URL list still shows what was chosen.
func pickPreviewPort(composeAbs, ingressService string, ingressPort int) (int, error) {
	out, err := composeRun(composeAbs, "ps", "--format", "json")
	if err != nil {
		return 0, fmt.Errorf("docker compose ps failed: %v: %s", err, tailOutput(out, 200))
	}
	entries, err := parseComposePs(out)
	if err != nil {
		return 0, err
	}
	if ingressService != "" {
		for _, e := range entries {
			if e.Service != ingressService {
				continue
			}
			if ingressPort > 0 {
				for _, p := range e.Publishers {
					if p.TargetPort == ingressPort && p.PublishedPort > 0 {
						return p.PublishedPort, nil
					}
				}
			}
			for _, p := range e.Publishers {
				if p.PublishedPort > 0 {
					return p.PublishedPort, nil
				}
			}
		}
		// A declared service with no live publishers falls through to the
		// generic scan — the declaration is a hint, not a gate.
	}
	ports := make(map[int]bool)
	for _, e := range entries {
		for _, p := range e.Publishers {
			if p.PublishedPort > 0 {
				ports[p.PublishedPort] = true
			}
		}
	}
	if len(ports) == 0 {
		return 0, fmt.Errorf("no published host ports in the compose stack (all services internal? add a ports: mapping)")
	}
	sorted := make([]int, 0, len(ports))
	for p := range ports {
		sorted = append(sorted, p)
	}
	sort.Ints(sorted)
	if len(sorted) > 1 {
		// Not an error: pick the lowest and say so. The user sees this URL
		// first and can tell if the wrong service won.
		return sorted[0], nil
	}
	return sorted[0], nil
}

// parseComposePs accepts both compose v2 wire shapes: an array of entries
// (multi-service, current) and a single object (older v2 builds).
func parseComposePs(out []byte) ([]composePsEntry, error) {
	trimmed := strings.TrimSpace(string(out))
	var entries []composePsEntry
	if err := json.Unmarshal([]byte(trimmed), &entries); err == nil {
		return entries, nil
	}
	var single composePsEntry
	if err := json.Unmarshal([]byte(trimmed), &single); err == nil && (single.Service != "" || single.Name != "") {
		return []composePsEntry{single}, nil
	}
	return nil, fmt.Errorf("unrecognized docker compose ps output: %s", tailOutput(out, 200))
}

// previewURLs assembles the URL list for a host port: localhost first (what
// the browser on THIS machine reaches), then the machine's non-loopback IPv4
// LAN addresses (what a phone on the same network reaches). All listed, none
// guessed — multi-NIC/VPN machines are the user's call, not ours.
func previewURLs(port int) []string {
	urls := []string{fmt.Sprintf("http://localhost:%d", port)}
	for _, ip := range lanIPv4s() {
		urls = append(urls, fmt.Sprintf("http://%s:%d", ip, port))
		if len(urls) >= 1+maxLANAddresses {
			break
		}
	}
	return urls
}

// lanIPv4s lists non-loopback IPv4 addresses of up interfaces, sorted for a
// deterministic report.
func lanIPv4s() []string {
	var out []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipNet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() {
				continue
			}
			out = append(out, ip4.String())
		}
	}
	sort.Strings(out)
	return out
}
