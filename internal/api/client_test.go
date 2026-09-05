package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/binoctal/open-agents-bridge/internal/config"
)

func TestNewClient_WSSConversion(t *testing.T) {
	cfg := &config.Config{ServerURL: "wss://api.example.com/ws"}
	c := NewClient(cfg)
	if c.baseURL != "https://api.example.com" {
		t.Errorf("expected https://api.example.com, got %s", c.baseURL)
	}
}

func TestNewClient_WSConversion(t *testing.T) {
	cfg := &config.Config{ServerURL: "ws://localhost:8787/ws"}
	c := NewClient(cfg)
	if c.baseURL != "http://localhost:8787" {
		t.Errorf("expected http://localhost:8787, got %s", c.baseURL)
	}
}

func TestNewClient_WSSuffixStripped(t *testing.T) {
	cfg := &config.Config{ServerURL: "https://api.example.com/ws", DeviceToken: "tok123"}
	c := NewClient(cfg)
	if c.baseURL != "https://api.example.com" {
		t.Errorf("expected https://api.example.com, got %s", c.baseURL)
	}
}

func TestClient_GetPermissionRules(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/bridge/permission-rules" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok123" {
			t.Errorf("unexpected auth header: %s", r.Header.Get("Authorization"))
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"rules": []map[string]string{
				{"id": "r1", "pattern": "*", "tool": "fs_read", "action": "auto-approve"},
			},
		})
	}))
	defer server.Close()

	cfg := &config.Config{ServerURL: server.URL, DeviceToken: "tok123"}
	c := NewClient(cfg)

	rules, err := c.GetPermissionRules("")
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	if rules[0].ID != "r1" {
		t.Errorf("expected r1, got %s", rules[0].ID)
	}
}

func TestClient_GetAgentConfig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(AgentConfig{
			SystemPrompt: "You are helpful",
			AllowedTools: []string{"fs_read", "fs_write"},
		})
	}))
	defer server.Close()

	cfg := &config.Config{ServerURL: server.URL, DeviceToken: "tok"}
	c := NewClient(cfg)

	agentCfg, err := c.GetAgentConfig("agent1")
	if err != nil {
		t.Fatal(err)
	}
	if agentCfg.SystemPrompt != "You are helpful" {
		t.Errorf("unexpected system prompt: %s", agentCfg.SystemPrompt)
	}
}

func TestClient_ReportSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := &config.Config{ServerURL: server.URL, DeviceToken: "tok"}
	c := NewClient(cfg)

	err := c.ReportSession(SessionReport{SessionID: "s1", CLIType: "claude"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestClient_ApiError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"forbidden"}`))
	}))
	defer server.Close()

	cfg := &config.Config{ServerURL: server.URL, DeviceToken: "bad"}
	c := NewClient(cfg)

	_, err := c.GetPermissionRules("")
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
}

// Preview hosting (add-preview-hosting, task 4.3)

func TestClient_CreatePreview(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/api/missions/internal/mission-1/previews" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var body struct {
			Files []PreviewFile      `json:"files"`
			Meta  *DeclarePreviewMeta `json:"meta"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Files) != 1 || body.Files[0].Path != "index.html" {
			t.Fatalf("unexpected files in request: %+v", body.Files)
		}
		if body.Meta == nil || body.Meta.HTMLRewrites != 2 || body.Meta.FileCount != 1 {
			t.Fatalf("unexpected meta in request: %+v", body.Meta)
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"previewId": "p1",
			"subdomain": "abc123",
			"url":       "https://preview.openagents.top/abc123/",
			"revived":   false,
			"uploads": []map[string]string{
				{"path": "index.html", "url": "https://r2.example.com/put-index"},
			},
		})
	}))
	defer server.Close()

	cfg := &config.Config{ServerURL: server.URL, DeviceToken: "tok123"}
	c := NewClient(cfg)

	resp, err := c.CreatePreview("mission-1", []PreviewFile{{Path: "index.html", SHA256: "abc", Size: 5}}, &DeclarePreviewMeta{HTMLRewrites: 2, FileCount: 1})
	if err != nil {
		t.Fatal(err)
	}
	if resp.PreviewID != "p1" || len(resp.Uploads) != 1 {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestClient_CreatePreview_QuotaError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{
				"code":    "PREVIEW_QUOTA_EXCEEDED",
				"message": "Free plan allows one active preview",
			},
		})
	}))
	defer server.Close()

	cfg := &config.Config{ServerURL: server.URL, DeviceToken: "tok123"}
	c := NewClient(cfg)

	_, err := c.CreatePreview("mission-1", []PreviewFile{{Path: "index.html", SHA256: "abc", Size: 5}}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	previewErr, ok := err.(*PreviewAPIError)
	if !ok {
		t.Fatalf("expected *PreviewAPIError, got %T: %v", err, err)
	}
	if previewErr.Code != "PREVIEW_QUOTA_EXCEEDED" {
		t.Errorf("unexpected code: %s", previewErr.Code)
	}
}

func TestClient_CompletePreview(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/missions/internal/mission-1/previews/p1/complete" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["taskId"] != "merge" || body["kind"] != "static" {
			t.Fatalf("unexpected complete body: %+v", body)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "previewId": "p1"})
	}))
	defer server.Close()

	cfg := &config.Config{ServerURL: server.URL, DeviceToken: "tok123"}
	c := NewClient(cfg)

	if err := c.CompletePreview("mission-1", "p1", CompletePreviewBody{TaskID: "merge", Kind: "static"}); err != nil {
		t.Fatal(err)
	}
}

// A zero-valued complete body must serialize to an EMPTY body — the wire
// shape an old-bridge revive produces and the platform's G19 soft-compat
// treats as "no taskId, skip snapshot registration".
func TestClient_CompletePreview_EmptyBodySendsNoJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) != 0 {
			t.Fatalf("expected empty request body, got %q", data)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
	}))
	defer server.Close()

	cfg := &config.Config{ServerURL: server.URL, DeviceToken: "tok123"}
	c := NewClient(cfg)

	if err := c.CompletePreview("mission-1", "p1", CompletePreviewBody{}); err != nil {
		t.Fatal(err)
	}
}

func TestClient_ReportArtifactKind(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/api/missions/internal/mission-1/artifact-kind" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["kind"] != "runtime" {
			t.Fatalf("unexpected kind in request: %+v", body)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
	}))
	defer server.Close()

	cfg := &config.Config{ServerURL: server.URL, DeviceToken: "tok123"}
	c := NewClient(cfg)

	if err := c.ReportArtifactKind("mission-1", "runtime"); err != nil {
		t.Fatal(err)
	}
}

func TestClient_GetPendingRevives(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/missions/internal/previews/pending-revives" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"revives": []map[string]string{
				{"missionId": "m1", "previewId": "p1"},
			},
		})
	}))
	defer server.Close()

	cfg := &config.Config{ServerURL: server.URL, DeviceToken: "tok123"}
	c := NewClient(cfg)

	revives, err := c.GetPendingRevives()
	if err != nil {
		t.Fatal(err)
	}
	if len(revives) != 1 || revives[0].MissionID != "m1" {
		t.Fatalf("unexpected revives: %+v", revives)
	}
}

func TestClient_UploadPreviewFile(t *testing.T) {
	var receivedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("expected PUT, got %s", r.Method)
		}
		body := make([]byte, r.ContentLength)
		r.Body.Read(body)
		receivedBody = body
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := &config.Config{ServerURL: server.URL, DeviceToken: "tok123"}
	c := NewClient(cfg)

	if err := c.UploadPreviewFile(server.URL+"/put-target", []byte("hello world")); err != nil {
		t.Fatal(err)
	}
	if string(receivedBody) != "hello world" {
		t.Errorf("unexpected uploaded body: %q", receivedBody)
	}
}

func TestClient_UploadPreviewFile_NonSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	cfg := &config.Config{ServerURL: server.URL, DeviceToken: "tok123"}
	c := NewClient(cfg)

	if err := c.UploadPreviewFile(server.URL, []byte("data")); err == nil {
		t.Fatal("expected error for non-2xx PUT response")
	}
}
