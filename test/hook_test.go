package test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/open-agents/open-agents-bridge/internal/hook"
)

func TestHookServerStart(t *testing.T) {
	events := make(chan hook.HookEvent, 10)
	
	server := hook.NewHookServer(func(e hook.HookEvent) {
		events <- e
	})
	
	if err := server.Start(); err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer server.Stop()
	
	if server.Port() == 0 {
		t.Error("Server port should not be 0")
	}
}

func TestHookServerSessionStart(t *testing.T) {
	events := make(chan hook.HookEvent, 10)
	
	server := hook.NewHookServer(func(e hook.HookEvent) {
		events <- e
	})
	
	if err := server.Start(); err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer server.Stop()
	
	// Send session start event
	payload := map[string]interface{}{
		"session_id": "test-session-123",
		"data": map[string]interface{}{
			"cli_type": "claude",
			"work_dir": "/home/user/project",
		},
	}
	
	body, _ := json.Marshal(payload)
	url := "http://127.0.0.1:" + strconv.Itoa(server.Port()) + "/hook/session-start"
	
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Failed to send request: %v", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	
	// Verify event was received
	select {
	case event := <-events:
		if event.Type != "session:start" {
			t.Errorf("Event type = %s, want session:start", event.Type)
		}
		if event.SessionID != "test-session-123" {
			t.Errorf("SessionID = %s, want test-session-123", event.SessionID)
		}
		if event.Timestamp == 0 {
			t.Error("Timestamp should not be 0")
		}
	case <-time.After(time.Second):
		t.Error("Timeout waiting for event")
	}
}

func TestHookServerToolCall(t *testing.T) {
	events := make(chan hook.HookEvent, 10)
	
	server := hook.NewHookServer(func(e hook.HookEvent) {
		events <- e
	})
	
	if err := server.Start(); err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer server.Stop()
	
	// Send tool call event
	payload := map[string]interface{}{
		"session_id": "test-session-456",
		"data": map[string]interface{}{
			"tool":   "file_write",
			"path":   "/test.txt",
			"action": "write",
		},
	}
	
	body, _ := json.Marshal(payload)
	url := "http://127.0.0.1:" + strconv.Itoa(server.Port()) + "/hook/tool-call"
	
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Failed to send request: %v", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	
	// Verify event was received
	select {
	case event := <-events:
		if event.Type != "tool:call" {
			t.Errorf("Event type = %s, want tool:call", event.Type)
		}
		if event.SessionID != "test-session-456" {
			t.Errorf("SessionID = %s, want test-session-456", event.SessionID)
		}
	case <-time.After(time.Second):
		t.Error("Timeout waiting for event")
	}
}

func TestHookServerSessionEnd(t *testing.T) {
	events := make(chan hook.HookEvent, 10)
	
	server := hook.NewHookServer(func(e hook.HookEvent) {
		events <- e
	})
	
	if err := server.Start(); err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer server.Stop()
	
	// Send session end event
	payload := map[string]interface{}{
		"session_id": "test-session-789",
	}
	
	body, _ := json.Marshal(payload)
	url := "http://127.0.0.1:" + strconv.Itoa(server.Port()) + "/hook/session-end"
	
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("Failed to send request: %v", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	
	// Verify event was received
	select {
	case event := <-events:
		if event.Type != "session:end" {
			t.Errorf("Event type = %s, want session:end", event.Type)
		}
	case <-time.After(time.Second):
		t.Error("Timeout waiting for event")
	}
}

func TestHookServerHealth(t *testing.T) {
	server := hook.NewHookServer(nil)
	
	if err := server.Start(); err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer server.Stop()
	
	url := "http://127.0.0.1:" + strconv.Itoa(server.Port()) + "/health"
	
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("Failed to send request: %v", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "OK" {
		t.Errorf("Body = %s, want OK", body)
	}
}

func TestHookServerInvalidJSON(t *testing.T) {
	server := hook.NewHookServer(nil)
	
	if err := server.Start(); err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer server.Stop()
	
	url := "http://127.0.0.1:" + strconv.Itoa(server.Port()) + "/hook/session-start"
	
	resp, err := http.Post(url, "application/json", bytes.NewReader([]byte("invalid json")))
	if err != nil {
		t.Fatalf("Failed to send request: %v", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("Status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

func TestHookServerMethodNotAllowed(t *testing.T) {
	server := hook.NewHookServer(nil)
	
	if err := server.Start(); err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer server.Stop()
	
	url := "http://127.0.0.1:" + strconv.Itoa(server.Port()) + "/hook/session-start"
	
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("Failed to send request: %v", err)
	}
	defer resp.Body.Close()
	
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("Status = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
}