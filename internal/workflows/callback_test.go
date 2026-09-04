package workflows

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExtractArtifacts(t *testing.T) {
	cfg := DefaultCallbackConfig()
	cm := NewCallbackManager(cfg)

	tests := []struct {
		name           string
		input          string
		wantSummaryLen int
		wantTruncated  bool
	}{
		{
			name:           "short output",
			input:          "Hello World",
			wantSummaryLen: 11,
			wantTruncated:  false,
		},
		{
			name:           "exact 500 chars",
			input:          strings.Repeat("a", 500),
			wantSummaryLen: 500,
			wantTruncated:  false,
		},
		{
			name:           "over 500 chars",
			input:          strings.Repeat("a", 600),
			wantSummaryLen: 500,
			wantTruncated:  false, // summary is truncated, artifacts may be truncated
		},
		{
			name:           "over 100KB",
			input:          strings.Repeat("a", 101*1024),
			wantSummaryLen: 500,
			wantTruncated:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			summary, artifacts := cm.ExtractArtifacts([]byte(tt.input))

			if len(summary) > 500 {
				t.Errorf("summary length = %d, want <= 500", len(summary))
			}

			if tt.wantTruncated && !strings.Contains(artifacts, "truncated") {
				t.Error("expected artifacts to contain truncation notice")
			}

			if !tt.wantTruncated && strings.Contains(artifacts, "truncated") {
				t.Error("unexpected truncation notice in artifacts")
			}
		})
	}
}

func TestCallbackConfigDefaults(t *testing.T) {
	cfg := DefaultCallbackConfig()

	if cfg.Timeout != 30*60*1000*1000*1000 { // 30 minutes
		t.Errorf("default timeout = %v, want 30 minutes", cfg.Timeout)
	}

	// One initial try + one per retry delay (the schedule spans the API's
	// 1102 CPU-limit penalty window; see callbackRetryDelays).
	if cfg.MaxRetries != len(callbackRetryDelays)+1 {
		t.Errorf("default max retries = %d, want %d", cfg.MaxRetries, len(callbackRetryDelays)+1)
	}

	if cfg.MaxArtifactSize != 100*1024 {
		t.Errorf("default max artifact size = %d, want 100KB", cfg.MaxArtifactSize)
	}
}

func TestNewCallbackManagerNormalizesWSScheme(t *testing.T) {
	tests := []struct{ in, want string }{
		{"ws://localhost:8989", "http://localhost:8989"},
		{"wss://api.example.com", "https://api.example.com"},
		{"http://localhost:8989", "http://localhost:8989"},
		{"https://api.example.com", "https://api.example.com"},
		{"", ""},
	}
	for _, tt := range tests {
		cm := NewCallbackManager(CallbackConfig{APIURL: tt.in, DeviceID: "dev-1"})
		if cm.config.APIURL != tt.want {
			t.Errorf("APIURL %q normalized to %q, want %q", tt.in, cm.config.APIURL, tt.want)
		}
	}
}

func TestSendTaskResultUsesMissionEventRoute(t *testing.T) {
	var gotPath, gotAuth, gotSecret, gotDeviceID string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotSecret = r.Header.Get("X-Internal-Secret")
		gotDeviceID = r.Header.Get("X-Device-ID")
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cm := NewCallbackManager(CallbackConfig{APIURL: srv.URL, DeviceID: "dev-1", UserID: "user-1", DeviceToken: "devtok"})
	if err := cm.SendTaskResult(TaskResult{JobID: "j1", TaskID: "t1", Success: true}); err != nil {
		t.Fatalf("SendTaskResult: %v", err)
	}

	if gotPath != "/api/missions/internal/orchestrator/event" {
		t.Errorf("callback path = %q, want /api/missions/internal/orchestrator/event", gotPath)
	}
	// The device token is the only credential reachable from a user's machine:
	// the API's shared secret is server-side and has no delivery channel here,
	// which is why every callback used to 403.
	if gotAuth != "Bearer devtok" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer devtok")
	}
	if gotSecret != "" {
		t.Errorf("X-Internal-Secret = %q, want it unset", gotSecret)
	}
	if gotDeviceID != "dev-1" {
		t.Errorf("X-Device-ID = %q, want dev-1", gotDeviceID)
	}
	// The internal route has no JWT context; the payload must carry the
	// mission owner so the route can build the user-scoped orchestrator.
	payload, _ := gotBody["payload"].(map[string]any)
	if payload["userId"] != "user-1" {
		t.Errorf("payload.userId = %v, want user-1", payload["userId"])
	}
}

// --- Output batching (design D2) -------------------------------------------
// A real ACP turn emits one content block per assistant message; per-frame
// POSTs are what tripped the API's CPU limit (prod job_1788524351375,
// 2026-09-04). Frames must coalesce into one request per flush window.

// recordingServer captures every posted event body in order.
type recordingServer struct {
	srv     *httptest.Server
	bodies  chan map[string]any
	closing func()
}

func newRecordingServer(t *testing.T) *recordingServer {
	t.Helper()
	bodies := make(chan map[string]any, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var ev map[string]any
		json.Unmarshal(body, &ev)
		bodies <- ev
		w.WriteHeader(http.StatusOK)
	}))
	return &recordingServer{srv: srv, bodies: bodies, closing: srv.Close}
}

func (rs *recordingServer) nextBody(t *testing.T) map[string]any {
	t.Helper()
	select {
	case ev := <-rs.bodies:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a callback POST")
		return nil
	}
}

func newBatchingManager(rs *recordingServer, t *testing.T) *CallbackManager {
	t.Helper()
	return NewCallbackManager(CallbackConfig{
		APIURL:  rs.srv.URL,
		DeviceID: "dev-batch",
		CacheDir: t.TempDir(),
	})
}

func TestSendTaskOutputCoalescesFrames(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.closing()
	cm := newBatchingManager(rs, t)

	// Two rapid frames within one flush window → one POST with both.
	cm.SendTaskOutput("job-1", "task-1", "stdout", "frame one; ")
	cm.SendTaskOutput("job-1", "task-1", "stdout", "frame two")

	ev := rs.nextBody(t)
	if ev["type"] != "workflow:task_output" {
		t.Fatalf("event type = %v, want workflow:task_output", ev["type"])
	}
	payload, _ := ev["payload"].(map[string]any)
	if payload["content"] != "frame one; frame two" {
		t.Errorf("content = %q, want coalesced frames", payload["content"])
	}
	if payload["taskId"] != "task-1" || payload["stream"] != "stdout" {
		t.Errorf("payload taskId/stream = %v/%v, want task-1/stdout", payload["taskId"], payload["stream"])
	}

	// No second POST: the window coalesced both frames.
	select {
	case extra := <-rs.bodies:
		t.Fatalf("unexpected extra callback POST: %v", extra)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestSendTaskOutputStreamChangeFlushesOldBatch(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.closing()
	cm := newBatchingManager(rs, t)

	cm.SendTaskOutput("job-1", "task-1", "stdout", "out frame")
	// Same taskID, different stream: the buffered stdout batch must ship
	// before the stderr batch starts.
	cm.SendTaskOutput("job-1", "task-1", "stderr", "err frame")

	first := rs.nextBody(t)
	p1, _ := first["payload"].(map[string]any)
	if p1["stream"] != "stdout" || p1["content"] != "out frame" {
		t.Errorf("first flush = %v/%v, want stdout/out frame", p1["stream"], p1["content"])
	}
	second := rs.nextBody(t)
	p2, _ := second["payload"].(map[string]any)
	if p2["stream"] != "stderr" || p2["content"] != "err frame" {
		t.Errorf("second flush = %v/%v, want stderr/err frame", p2["stream"], p2["content"])
	}
}

func TestSendTaskOutputFlushesOnBufferFull(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.closing()
	cm := newBatchingManager(rs, t)

	big := strings.Repeat("x", outputFlushBytes)
	cm.SendTaskOutput("job-1", "task-full", "stdout", big)

	// A full buffer must flush without waiting for the timer.
	ev := rs.nextBody(t)
	payload, _ := ev["payload"].(map[string]any)
	if got := payload["content"].(string); len(got) < outputFlushBytes {
		t.Errorf("flushed content len = %d, want >= %d", len(got), outputFlushBytes)
	}
}

func TestSendTaskOutputCapsBufferWithDropMarker(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.closing()
	cm := newBatchingManager(rs, t)

	// Far past the 1MB per-batch cap: the overflow is dropped and the flush
	// carries a marker so scrollback shows the gap.
	cm.SendTaskOutput("job-1", "task-overflow", "stdout", strings.Repeat("y", outputMaxBuffer+512*1024))

	ev := rs.nextBody(t)
	payload, _ := ev["payload"].(map[string]any)
	content, _ := payload["content"].(string)
	if !strings.Contains(content, "dropped by bridge flush cap") {
		t.Error("expected drop marker in flushed content")
	}
	if want := outputMaxBuffer; len(content) < want || len(content) > want+256 {
		t.Errorf("flushed content len = %d, want ~%d (cap + marker)", len(content), want)
	}
}

// --- Retry schedule (design D3) --------------------------------------------

func TestCallbackRetryDelaysSpanPenaltyWindow(t *testing.T) {
	// The API's 1102 CPU-limit penalty window lasts 60-80s account-wide; the
	// old 1s/2s/4s schedule exhausted inside it and lost terminal callbacks.
	want := []time.Duration{time.Second, 15 * time.Second, 30 * time.Second, 60 * time.Second}
	if len(callbackRetryDelays) != len(want) {
		t.Fatalf("retry schedule = %v, want %v", callbackRetryDelays, want)
	}
	for i := range want {
		if callbackRetryDelays[i] != want[i] {
			t.Errorf("callbackRetryDelays[%d] = %v, want %v", i, callbackRetryDelays[i], want[i])
		}
	}
	var sum time.Duration
	for _, d := range callbackRetryDelays {
		sum += d
	}
	if sum < 60*time.Second {
		t.Errorf("retry schedule total = %v, want >= 60s to span the penalty window", sum)
	}
}

// --- Cache fallback (cacheEvent / RetryCachedEvents) ------------------------

func TestNewCallbackManagerDefaultsCacheDir(t *testing.T) {
	// The bridge core constructs CallbackConfig without CacheDir and
	// cacheEvent is gated on it — without the default a terminal callback
	// that exhausts its retries is lost for good (prod job_1788524351375).
	cm := NewCallbackManager(CallbackConfig{APIURL: "http://localhost:1", DeviceID: "dev-cd"})
	if !strings.Contains(cm.config.CacheDir, ".open-agents-bridge") {
		t.Errorf("default CacheDir = %q, want under .open-agents-bridge", cm.config.CacheDir)
	}
}

func TestRetryCachedEventsSendsAndRemoves(t *testing.T) {
	rs := newRecordingServer(t)
	defer rs.closing()
	dir := t.TempDir()
	cm := NewCallbackManager(CallbackConfig{
		APIURL:   rs.srv.URL,
		DeviceID: "dev-cache",
		CacheDir: dir,
	})

	cached := map[string]any{
		"type": "workflow:task_result",
		"payload": map[string]any{
			"jobId":  "job-c",
			"taskId": "task-c",
		},
	}
	data, _ := json.Marshal(cached)
	if err := os.WriteFile(filepath.Join(dir, "task-c.json"), data, 0644); err != nil {
		t.Fatalf("seed cache file: %v", err)
	}

	if err := cm.RetryCachedEvents(); err != nil {
		t.Fatalf("RetryCachedEvents: %v", err)
	}

	ev := rs.nextBody(t)
	if ev["type"] != "workflow:task_result" {
		t.Errorf("retried event type = %v, want workflow:task_result", ev["type"])
	}
	if _, err := os.Stat(filepath.Join(dir, "task-c.json")); !os.IsNotExist(err) {
		t.Error("cached event file should be removed after successful send")
	}
}
