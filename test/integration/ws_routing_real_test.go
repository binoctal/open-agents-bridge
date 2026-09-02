package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const apiBase = "http://localhost:8989"
const wsBase = "ws://localhost:8989"

// testOrigin satisfies the API's CSRF origin check (must match an allowed
// dev origin).
const testOrigin = "http://localhost:5173"

// devSetupResponse mirrors the API response from POST /api/dev/setup
type devSetupResponse struct {
	User *struct {
		ID string `json:"id"`
	} `json:"user"`
	Device *struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	} `json:"device"`
	UserID   string `json:"userId"`
	DeviceID string `json:"deviceId"`
}

// loginResponse mirrors the API response from POST /api/auth/login
type loginResponse struct {
	Token string `json:"token"`
}

// wsMessage mirrors the wire message format
type wsMessage struct {
	Type      string      `json:"type"`
	Payload   interface{} `json:"payload"`
	Timestamp int64       `json:"timestamp"`
}

func isAPIRunning() bool {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(apiBase + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == 200
}

func createRealTestUser(t *testing.T, suffix int64) (userID, deviceID, deviceToken, jwt string) {
	t.Helper()

	// POST /api/dev/setup
	email := fmt.Sprintf("e2e-go-%d@test.local", suffix)
	payload, _ := json.Marshal(map[string]string{"email": email, "password": "testpassword123"})

	resp, err := postJSON(t, "/api/dev/setup", payload)
	if err != nil {
		t.Fatalf("dev/setup request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("dev/setup failed with status %d", resp.StatusCode)
	}

	var setup devSetupResponse
	if err := json.NewDecoder(resp.Body).Decode(&setup); err != nil {
		t.Fatalf("dev/setup decode failed: %v", err)
	}

	userID = setup.UserID
	if userID == "" {
		userID = setup.User.ID
	}
	deviceID = setup.DeviceID
	if deviceID == "" {
		deviceID = setup.Device.ID
	}
	deviceToken = setup.Device.Token

	// POST /api/auth/login
	loginPayload, _ := json.Marshal(map[string]string{"email": email, "password": "testpassword123"})
	loginResp, err := postJSON(t, "/api/auth/login", loginPayload)
	if err != nil {
		t.Fatalf("auth/login request failed: %v", err)
	}
	defer loginResp.Body.Close()

	var login loginResponse
	if err := json.NewDecoder(loginResp.Body).Decode(&login); err != nil {
		t.Fatalf("auth/login decode failed: %v", err)
	}

	jwt = login.Token
	return
}

func connectBridgeWS(t *testing.T, userID, deviceToken, deviceID string) *websocket.Conn {
	t.Helper()
	url := fmt.Sprintf("%s/ws/%s?type=bridge&token=%s&deviceId=%s", wsBase, userID, deviceToken, deviceID)
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("Bridge WS connect failed: %v", err)
	}
	return conn
}

func connectWebWS(t *testing.T, userID, jwt string) *websocket.Conn {
	t.Helper()
	url := fmt.Sprintf("%s/ws/%s?type=web&token=%s", wsBase, userID, jwt)
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("Web WS connect failed: %v", err)
	}
	return conn
}

func waitForWSType(t *testing.T, conn *websocket.Conn, msgType string, timeout time.Duration) wsMessage {
	t.Helper()
	deadline := time.Now().Add(timeout)
	conn.SetReadDeadline(deadline)

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("ReadMessage failed while waiting for %s: %v", msgType, err)
		}

		var msg wsMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if msg.Type == msgType {
			return msg
		}
	}
}

// ─── Tests ───

func TestBridgeWSConnectBroadcastsDeviceOnline(t *testing.T) {
	if !isAPIRunning() {
		t.Skip("API server not running on :8989")
	}

	suffix := time.Now().UnixNano()
	userID, deviceID, deviceToken, jwt := createRealTestUser(t, suffix)

	// Connect bridge
	bridgeWS := connectBridgeWS(t, userID, deviceToken, deviceID)
	defer bridgeWS.Close()

	time.Sleep(300 * time.Millisecond)

	// Connect web
	webWS := connectWebWS(t, userID, jwt)
	defer webWS.Close()

	// Web should receive devices:sync containing the bridge device
	msg := waitForWSType(t, webWS, "devices:sync", 5*time.Second)

	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload struct {
		Devices []struct {
			DeviceID string `json:"deviceId"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		t.Fatalf("Failed to parse devices:sync payload: %v", err)
	}

	found := false
	for _, d := range payload.Devices {
		if d.DeviceID == deviceID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("devices:sync does not contain device %s", deviceID)
	}
}

func TestBridgeDisconnectBroadcastsDeviceOffline(t *testing.T) {
	if !isAPIRunning() {
		t.Skip("API server not running on :8989")
	}

	suffix := time.Now().UnixNano()
	userID, deviceID, deviceToken, jwt := createRealTestUser(t, suffix)

	bridgeWS := connectBridgeWS(t, userID, deviceToken, deviceID)
	defer bridgeWS.Close()

	time.Sleep(300 * time.Millisecond)

	webWS := connectWebWS(t, userID, jwt)
	defer webWS.Close()

	// Consume devices:sync
	waitForWSType(t, webWS, "devices:sync", 5*time.Second)

	// Disconnect bridge
	bridgeWS.WriteMessage(websocket.CloseMessage,
		websocket.FormatCloseMessage(1000, "test cleanup"))
	bridgeWS.Close()

	// Web should get device:offline
	msg := waitForWSType(t, webWS, "device:offline", 5*time.Second)
	payloadBytes, _ := json.Marshal(msg.Payload)
	var payload struct {
		DeviceID string `json:"deviceId"`
	}
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		t.Fatalf("Failed to parse device:offline payload: %v", err)
	}
	if payload.DeviceID != deviceID {
		t.Errorf("device:offline deviceId = %s, want %s", payload.DeviceID, deviceID)
	}
}

func TestMessageRoutingsessionOutput(t *testing.T) {
	if !isAPIRunning() {
		t.Skip("API server not running on :8989")
	}

	suffix := time.Now().UnixNano()
	userID, deviceID, deviceToken, jwt := createRealTestUser(t, suffix)

	bridgeWS := connectBridgeWS(t, userID, deviceToken, deviceID)
	defer bridgeWS.Close()
	time.Sleep(300 * time.Millisecond)

	webWS := connectWebWS(t, userID, jwt)
	defer webWS.Close()
	waitForWSType(t, webWS, "devices:sync", 5*time.Second)

	// Bridge sends session:output
	msg := wsMessage{
		Type: "session:output",
		Payload: map[string]interface{}{
			"sessionId":  "sess_go_1",
			"deviceId":   deviceID,
			"outputType": "stdout",
			"content":    "Hello from Go bridge",
		},
		Timestamp: time.Now().UnixMilli(),
	}
	data, _ := json.Marshal(msg)
	if err := bridgeWS.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("WriteMessage failed: %v", err)
	}

	// Web receives it: the room batches session:output per session (50ms
	// flush) and broadcasts a session:output-batch carrying the lines.
	received := waitForWSType(t, webWS, "session:output-batch", 5*time.Second)
	payloadBytes, _ := json.Marshal(received.Payload)
	var payload struct {
		SessionId string   `json:"sessionId"`
		Lines     []string `json:"lines"`
	}
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		t.Fatalf("Failed to parse session:output-batch payload: %v", err)
	}
	if payload.SessionId != "sess_go_1" {
		t.Errorf("sessionId = %s, want sess_go_1", payload.SessionId)
	}
	if len(payload.Lines) != 1 || payload.Lines[0] != "Hello from Go bridge" {
		t.Errorf("lines = %v, want [Hello from Go bridge]", payload.Lines)
	}
}

func TestPermissionRoundTrip(t *testing.T) {
	if !isAPIRunning() {
		t.Skip("API server not running on :8989")
	}

	suffix := time.Now().UnixNano()
	userID, deviceID, deviceToken, jwt := createRealTestUser(t, suffix)

	bridgeWS := connectBridgeWS(t, userID, deviceToken, deviceID)
	defer bridgeWS.Close()
	time.Sleep(300 * time.Millisecond)

	webWS := connectWebWS(t, userID, jwt)
	defer webWS.Close()
	waitForWSType(t, webWS, "devices:sync", 5*time.Second)

	// Bridge sends permission:request
	reqMsg := wsMessage{
		Type: "permission:request",
		Payload: map[string]interface{}{
			"id":          "perm_go_1",
			"sessionId":   "sess_perm",
			"deviceId":    deviceID,
			"toolName":    "Bash",
			"description": "Run bash command",
			"risk":        "high",
		},
		Timestamp: time.Now().UnixMilli(),
	}
	data, _ := json.Marshal(reqMsg)
	bridgeWS.WriteMessage(websocket.TextMessage, data)

	// Web receives
	waitForWSType(t, webWS, "permission:request", 5*time.Second)

	// Web approves
	respMsg := wsMessage{
		Type: "permission:response",
		Payload: map[string]interface{}{
			"id":       "perm_go_1",
			"deviceId": deviceID,
			"approved": true,
		},
		Timestamp: time.Now().UnixMilli(),
	}
	respData, _ := json.Marshal(respMsg)
	webWS.WriteMessage(websocket.TextMessage, respData)

	// Bridge receives approval
	received := waitForWSType(t, bridgeWS, "permission:response", 5*time.Second)
	payloadBytes, _ := json.Marshal(received.Payload)
	var payload struct {
		Approved bool `json:"approved"`
	}
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		t.Fatalf("Failed to parse permission:response payload: %v", err)
	}
	if !payload.Approved {
		t.Error("approved = false, want true")
	}
}

// bytesReader helper. This used to be a hand-rolled reader whose exhaustion
// path returned fmt.Errorf("EOF") instead of the io.EOF sentinel — the HTTP
// client treats any non-sentinel read error as a real failure and aborts the
// request, so every POST failed with `Post ... EOF` whenever the dev server
// was actually up to receive it (masked in CI, where these tests skip).
func bytesReader(data []byte) *bytes.Reader {
	return bytes.NewReader(data)
}

// postJSON posts a JSON body with the Origin header the CSRF middleware
// requires (auth-hardening): an Origin-less request is rejected 403 before
// the route ever runs.
func postJSON(t *testing.T, path string, payload []byte) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, apiBase+path, bytesReader(payload))
	if err != nil {
		t.Fatalf("build %s request: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", testOrigin)
	return http.DefaultClient.Do(req)
}
