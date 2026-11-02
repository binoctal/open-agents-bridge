package deploysource

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/binoctal/open-agents-bridge/internal/api"
)

// stubReporter captures report calls; failOn makes the Nth send fail so the
// send-failure path (log, no panic) is exercisable.
type stubReporter struct {
	mu       sync.Mutex
	reports  []api.LocalPreviewReport
	missions []string
	failOn   int
}

func (s *stubReporter) ReportLocalPreview(missionID string, report api.LocalPreviewReport) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.missions = append(s.missions, missionID)
	s.reports = append(s.reports, report)
	if len(s.reports) == s.failOn {
		return errors.New("send failed")
	}
	return nil
}

// stubDocker swaps the three command seams for the duration of one test.
func stubDocker(t *testing.T, upErr error, psOut string, psErr error) {
	t.Helper()
	prev := composeRun
	prevProbe := dockerProbe
	composeRun = func(composeAbs string, args ...string) ([]byte, error) {
		if args[0] == "ps" {
			if psErr != nil {
				return nil, psErr
			}
			return []byte(psOut), nil
		}
		if upErr != nil {
			return []byte("simulated docker failure output\n"), upErr
		}
		return []byte("ok"), nil
	}
	dockerProbe = func() error { return nil }
	t.Cleanup(func() { composeRun = prev; dockerProbe = prevProbe })
}

// seedComposeTree writes a convention-path compose file so findComposeOnDisk
// succeeds; returns the repo root.
func seedComposeTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "deploy", "compose.yaml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestRunLocalPreviewStartReportsRunningWithURLs(t *testing.T) {
	root := seedComposeTree(t)
	stubDocker(t, nil, `[{"Name":"proj-web-1","Service":"web","Publishers":[{"URL":"0.0.0.0","TargetPort":3000,"PublishedPort":3000,"Protocol":"tcp"}]}]`, nil)
	rep := &stubReporter{}

	RunLocalPreview(rep, api.LocalPreviewWork{MissionID: "m1", Action: "start"}, root, nil)

	if len(rep.reports) != 1 {
		t.Fatalf("expected exactly one report, got %d", len(rep.reports))
	}
	got := rep.reports[0]
	if got.Status != "running" {
		t.Fatalf("status = %q, want running", got.Status)
	}
	if got.ComposePath != "deploy/compose.yaml" {
		t.Fatalf("composePath = %q, want deploy/compose.yaml", got.ComposePath)
	}
	if len(got.URLs) == 0 || got.URLs[0] != "http://localhost:3000" {
		t.Fatalf("urls = %v, want localhost first", got.URLs)
	}
	for _, u := range got.URLs {
		if strings.HasPrefix(u, "http://127.") {
			t.Fatalf("loopback leaked into url list: %v", got.URLs)
		}
	}
}

func TestRunLocalPreviewStartNoComposeSettlesFailed(t *testing.T) {
	// Empty tree: the card must learn there is nothing to preview, not wait
	// forever (spec: 无 compose -> failed).
	root := t.TempDir()
	stubDocker(t, nil, "[]", nil)
	rep := &stubReporter{}

	RunLocalPreview(rep, api.LocalPreviewWork{MissionID: "m1", Action: "start"}, root, nil)

	if len(rep.reports) != 1 || rep.reports[0].Status != "failed" {
		t.Fatalf("reports = %+v, want one failed", rep.reports)
	}
	if !strings.Contains(rep.reports[0].FailReason, "no convention-path compose file") {
		t.Fatalf("failReason = %q, want the no-compose story", rep.reports[0].FailReason)
	}
}

func TestRunLocalPreviewStartDockerUnusableSettlesFailed(t *testing.T) {
	root := seedComposeTree(t)
	stubDocker(t, nil, "[]", nil)
	prevProbe := dockerProbe
	dockerProbe = func() error { return errors.New("cannot connect to the Docker daemon") }
	t.Cleanup(func() { dockerProbe = prevProbe })
	rep := &stubReporter{}

	RunLocalPreview(rep, api.LocalPreviewWork{MissionID: "m1", Action: "start"}, root, nil)

	if len(rep.reports) != 1 || rep.reports[0].Status != "failed" {
		t.Fatalf("reports = %+v, want one failed", rep.reports)
	}
	if !strings.Contains(rep.reports[0].FailReason, "docker not usable") {
		t.Fatalf("failReason = %q, want the docker story", rep.reports[0].FailReason)
	}
}

func TestRunLocalPreviewStartUpFailureCarriesLogTail(t *testing.T) {
	root := seedComposeTree(t)
	stubDocker(t, errors.New("exit status 1"), "", nil)
	rep := &stubReporter{}

	RunLocalPreview(rep, api.LocalPreviewWork{MissionID: "m1", Action: "start"}, root, nil)

	got := rep.reports[0]
	if got.Status != "failed" || !strings.Contains(got.FailReason, "docker compose up failed") {
		t.Fatalf("report = %+v, want up failure", got)
	}
	if !strings.Contains(got.FailLogTail, "simulated docker failure output") {
		t.Fatalf("failLogTail = %q, want the command output tail", got.FailLogTail)
	}
}

func TestRunLocalPreviewStopReportsStopped(t *testing.T) {
	root := seedComposeTree(t)
	stubDocker(t, nil, "[]", nil)
	rep := &stubReporter{}

	RunLocalPreview(rep, api.LocalPreviewWork{MissionID: "m1", Action: "stop"}, root, nil)

	if len(rep.reports) != 1 || rep.reports[0].Status != "stopped" {
		t.Fatalf("reports = %+v, want one stopped", rep.reports)
	}
}

func TestRunLocalPreviewReportSendFailureDoesNotPanic(t *testing.T) {
	root := seedComposeTree(t)
	stubDocker(t, nil, "[]", nil)
	rep := &stubReporter{failOn: 1}

	RunLocalPreview(rep, api.LocalPreviewWork{MissionID: "m1", Action: "stop"}, root, nil)

	if len(rep.reports) != 1 {
		t.Fatalf("reports = %+v, want the single attempted report", rep.reports)
	}
}

func TestRunLocalPreviewUnknownActionIsIgnored(t *testing.T) {
	rep := &stubReporter{}
	RunLocalPreview(rep, api.LocalPreviewWork{MissionID: "m1", Action: "explode"}, t.TempDir(), nil)
	if len(rep.reports) != 0 {
		t.Fatalf("reports = %+v, want none for an unknown action", rep.reports)
	}
}

func TestPickPreviewPortIngressDeclarationWins(t *testing.T) {
	ps := `[{"Service":"web","Publishers":[{"TargetPort":3000,"PublishedPort":8080}]},{"Service":"api","Publishers":[{"TargetPort":3000,"PublishedPort":3000},{"TargetPort":5432,"PublishedPort":15432}]}]`
	stubDocker(t, nil, ps, nil)

	// Declared ingress api:3000 -> the api service's target-3000 publisher.
	port, err := pickPreviewPort("/x/compose.yml", "api", 3000)
	if err != nil || port != 3000 {
		t.Fatalf("port = %d err = %v, want 3000 nil", port, err)
	}
	// Same hint without a container port: first publisher of that service.
	port, err = pickPreviewPort("/x/compose.yml", "api", 0)
	if err != nil || port != 3000 {
		t.Fatalf("port = %d err = %v, want 3000 nil", port, err)
	}
	// A hint naming a service with no publishers falls through to the scan.
	port, err = pickPreviewPort("/x/compose.yml", "missing", 0)
	if err != nil || port != 3000 {
		t.Fatalf("port = %d err = %v, want fallback to lowest 3000", port, err)
	}
}

func TestPickPreviewPortNoHintSingleAndMultiple(t *testing.T) {
	stubDocker(t, nil, `[{"Service":"web","Publishers":[{"TargetPort":3000,"PublishedPort":9000}]}]`, nil)
	port, err := pickPreviewPort("/x/compose.yml", "", 0)
	if err != nil || port != 9000 {
		t.Fatalf("port = %d err = %v, want 9000 nil", port, err)
	}

	stubDocker(t, nil, `[{"Service":"a","Publishers":[{"TargetPort":80,"PublishedPort":8081}]},{"Service":"b","Publishers":[{"TargetPort":3000,"PublishedPort":3000}]}]`, nil)
	port, err = pickPreviewPort("/x/compose.yml", "", 0)
	if err != nil || port != 3000 {
		t.Fatalf("port = %d err = %v, want lowest 3000 on ambiguity", port, err)
	}
}

func TestPickPreviewPortNoPublishedPortsFails(t *testing.T) {
	stubDocker(t, nil, `[{"Service":"web","Publishers":[]}]`, nil)
	_, err := pickPreviewPort("/x/compose.yml", "", 0)
	if err == nil || !strings.Contains(err.Error(), "no published host ports") {
		t.Fatalf("err = %v, want the no-ports story", err)
	}
}

func TestParseComposePsSingleObjectShape(t *testing.T) {
	entries, err := parseComposePs([]byte(`{"Name":"x-1","Service":"web","Publishers":[{"TargetPort":3000,"PublishedPort":3000}]}`))
	if err != nil || len(entries) != 1 || entries[0].Service != "web" {
		t.Fatalf("entries = %+v err = %v, want one web entry", entries, err)
	}
	if _, err := parseComposePs([]byte(`not json`)); err == nil {
		t.Fatal("garbage output must be an error, not a silent empty list")
	}
}

func TestPreviewURLsLocalhostFirst(t *testing.T) {
	urls := previewURLs(4173)
	if len(urls) == 0 || urls[0] != "http://localhost:4173" {
		t.Fatalf("urls = %v, want localhost first", urls)
	}
	// Whatever the machine's interface zoo, the list stays capped.
	if len(urls) > 1+maxLANAddresses {
		t.Fatalf("urls len = %d, cap %d", len(urls), 1+maxLANAddresses)
	}
	for i, u := range urls {
		if i > 0 && !strings.HasPrefix(u, "http://") {
			t.Fatalf("url %d = %q, malformed", i, u)
		}
	}
}
