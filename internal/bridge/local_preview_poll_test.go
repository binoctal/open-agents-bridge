package bridge

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/binoctal/open-agents-bridge/internal/api"
	"github.com/binoctal/open-agents-bridge/internal/config"
	"github.com/binoctal/open-agents-bridge/internal/workflows"
)

// local-compose-preview dispatch: a poll item for this machine turns into a
// report POST on the internal route, and the in-flight claim keeps one tick
// from double-running a mission's compose action. The executor's docker
// boundaries are covered in deploysource; here the repoRoot is an EMPTY tree,
// so the item settles as failed ("no convention-path compose file") without
// any docker on the test machine.

type reportCapture struct {
	mu       sync.Mutex
	received []api.LocalPreviewReport
	pinged   chan struct{}
}

func newLocalPreviewBridge(t *testing.T, pendingBody string) (*Bridge, *reportCapture) {
	t.Helper()
	capture := &reportCapture{pinged: make(chan struct{}, 8)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/missions/internal/hosted-deployments/pending" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(pendingBody))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/missions/internal/") && strings.HasSuffix(r.URL.Path, "/local-previews/report") {
			body, _ := io.ReadAll(r.Body)
			var report api.LocalPreviewReport
			_ = json.Unmarshal(body, &report)
			capture.mu.Lock()
			capture.received = append(capture.received, report)
			capture.mu.Unlock()
			capture.pinged <- struct{}{}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{
		ServerURL:    "ws" + strings.TrimPrefix(srv.URL, "http"),
		MachineID:    "machine-lp",
		MachineToken: "token-lp",
	}
	// Empty repo root: the start settles as failed (no compose file) with
	// zero docker involvement — exactly the seam this suite wants.
	b := &Bridge{apiClient: api.NewClient(cfg), worktreeManager: workflows.NewWorktreeManager(t.TempDir())}
	return b, capture
}

func TestPollDeploySourcesDispatchesLocalPreviewWork(t *testing.T) {
	b, capture := newLocalPreviewBridge(t,
		`{"deploys":[],"features":["chunked_declare","source_upload"],"localPreviews":[{"missionId":"m1","action":"start"}]}`)

	if !b.pollDeploySources() {
		t.Fatal("poll with source_upload granted must continue")
	}
	select {
	case <-capture.pinged:
	case <-time.After(10 * time.Second):
		t.Fatal("no report POST arrived for the local preview item")
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if len(capture.received) != 1 {
		t.Fatalf("received %+v, want one report", capture.received)
	}
	got := capture.received[0]
	if got.Status != "failed" || !strings.Contains(got.FailReason, "no convention-path compose file") {
		t.Fatalf("report = %+v, want the empty-tree failed settle", got)
	}
}

func TestPollDeploySourcesOldAPILocalPreviewsIsNoop(t *testing.T) {
	// Old API shape: no localPreviews member. Nothing dispatches, polling
	// continues (the rolling-upgrade window).
	b, capture := newLocalPreviewBridge(t, `{"deploys":[],"features":["chunked_declare","source_upload"]}`)
	if !b.pollDeploySources() {
		t.Fatal("old-API poll must continue")
	}
	select {
	case <-capture.pinged:
		t.Fatal("nothing should report when the API sent no local previews")
	case <-time.After(150 * time.Millisecond):
	}
}

func TestClaimLocalPreviewIsIdempotentUntilDone(t *testing.T) {
	b := &Bridge{}
	if !b.claimLocalPreview("m1") {
		t.Fatal("first claim must win")
	}
	if b.claimLocalPreview("m1") {
		t.Fatal("second claim of the same mission must lose")
	}
	if !b.claimLocalPreview("m2") {
		t.Fatal("another mission must still claim")
	}
	b.localPreviewDone("m1")
	if !b.claimLocalPreview("m1") {
		t.Fatal("claim after done must win again")
	}
}
