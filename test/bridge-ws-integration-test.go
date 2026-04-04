package test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// MockMessage 模拟消息结构
type MockMessage struct {
	Type      string                 `json:"type"`
	SessionID string                 `json:"session_id,omitempty"`
	Data      map[string]interface{} `json:"data,omitempty"`
	Timestamp int64                  `json:"timestamp,omitempty"`
}

// MockWebSocketServer 创建一个模拟的WebSocket服务器
type MockWebSocketServer struct {
	server   *httptest.Server
	upgrader websocket.Upgrader
	conns    map[string]*websocket.Conn
	mu       sync.RWMutex
	messages []MockMessage
}

// NewMockWebSocketServer 创建新的模拟WebSocket服务器
func NewMockWebSocketServer() *MockWebSocketServer {
	mock := &MockWebSocketServer{
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
		conns:    make(map[string]*websocket.Conn),
		messages: make([]MockMessage, 0),
	}

	mock.server = httptest.NewServer(http.HandlerFunc(mock.handleWebSocket))
	return mock
}

// handleWebSocket 处理WebSocket连接
func (m *MockWebSocketServer) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := m.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// 从路径中提取session ID
	sessionID := strings.TrimPrefix(r.URL.Path, "/ws/")
	if sessionID == "" {
		sessionID = "test-session"
	}

	m.mu.Lock()
	m.conns[sessionID] = conn
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		delete(m.conns, sessionID)
		m.mu.Unlock()
	}()

	// 读取消息
	for {
		var msg MockMessage
		err := conn.ReadJSON(&msg)
		if err != nil {
			break
		}

		m.mu.Lock()
		m.messages = append(m.messages, msg)
		m.mu.Unlock()

		// 回显消息
		response := MockMessage{
			Type:      "ack",
			SessionID: sessionID,
			Data: map[string]interface{}{
				"received": msg.Type,
			},
			Timestamp: time.Now().Unix(),
		}
		conn.WriteJSON(response)
	}
}

// SendMessage 发送消息到指定session
func (m *MockWebSocketServer) SendMessage(sessionID string, msg MockMessage) error {
	m.mu.RLock()
	conn, ok := m.conns[sessionID]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("session not found: %s", sessionID)
	}

	return conn.WriteJSON(msg)
}

// GetMessages 获取收到的所有消息
func (m *MockWebSocketServer) GetMessages() []MockMessage {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]MockMessage{}, m.messages...)
}

// GetURL 获取服务器URL
func (m *MockWebSocketServer) GetURL() string {
	return strings.Replace(m.server.URL, "http://", "ws://", 1)
}

// Close 关闭服务器
func (m *MockWebSocketServer) Close() {
	m.server.Close()
}

// TestBridgeWebSocketCommunication 测试Bridge和WebSocket的通信
func TestBridgeWebSocketCommunication(t *testing.T) {
	// 创建模拟WebSocket服务器
	mockServer := NewMockWebSocketServer()
	defer mockServer.Close()

	t.Run("基本连接测试", func(t *testing.T) {
		// 创建WebSocket客户端
		wsURL := mockServer.GetURL() + "/ws/test-session-1"
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		require.NoError(t, err)
		defer conn.Close()

		// 发送测试消息
		testMsg := MockMessage{
			Type: "test",
			Data: map[string]interface{}{
				"content": "Hello from bridge",
			},
			Timestamp: time.Now().Unix(),
		}

		err = conn.WriteJSON(testMsg)
		require.NoError(t, err)

		// 接收响应
		var response MockMessage
		err = conn.ReadJSON(&response)
		require.NoError(t, err)

		assert.Equal(t, "ack", response.Type)
		assert.Equal(t, "test-session-1", response.SessionID)
	})

	t.Run("多消息发送测试", func(t *testing.T) {
		wsURL := mockServer.GetURL() + "/ws/test-session-2"
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		require.NoError(t, err)
		defer conn.Close()

		// 发送多条消息
		messageTypes := []string{"init", "command", "output", "close"}
		for _, msgType := range messageTypes {
			msg := MockMessage{
				Type: msgType,
				Data: map[string]interface{}{
					"payload": fmt.Sprintf("data for %s", msgType),
				},
				Timestamp: time.Now().Unix(),
			}

			err = conn.WriteJSON(msg)
			require.NoError(t, err)

			// 接收确认
			var response MockMessage
			err = conn.ReadJSON(&response)
			require.NoError(t, err)
			assert.Equal(t, "ack", response.Type)
		}
	})

	t.Run("并发连接测试", func(t *testing.T) {
		numClients := 5
		var wg sync.WaitGroup
		wg.Add(numClients)

		for i := 0; i < numClients; i++ {
			go func(clientID int) {
				defer wg.Done()

				sessionID := fmt.Sprintf("concurrent-session-%d", clientID)
				wsURL := mockServer.GetURL() + "/ws/" + sessionID
				conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
				if err != nil {
					t.Errorf("Client %d failed to connect: %v", clientID, err)
					return
				}
				defer conn.Close()

				// 发送消息
				msg := MockMessage{
					Type: "concurrent-test",
					Data: map[string]interface{}{
						"client_id": clientID,
					},
					Timestamp: time.Now().Unix(),
				}

				err = conn.WriteJSON(msg)
				if err != nil {
					t.Errorf("Client %d failed to send message: %v", clientID, err)
					return
				}

				// 接收响应
				var response MockMessage
				err = conn.ReadJSON(&response)
				if err != nil {
					t.Errorf("Client %d failed to receive response: %v", clientID, err)
					return
				}

				if response.Type != "ack" {
					t.Errorf("Client %d received unexpected response type: %s", clientID, response.Type)
				}
			}(i)
		}

		wg.Wait()
	})

	t.Run("双向通信测试", func(t *testing.T) {
		sessionID := "bidirectional-session"
		wsURL := mockServer.GetURL() + "/ws/" + sessionID
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		require.NoError(t, err)
		defer conn.Close()

		// 客户端发送消息
		clientMsg := MockMessage{
			Type: "client-message",
			Data: map[string]interface{}{
				"from": "bridge",
			},
			Timestamp: time.Now().Unix(),
		}
		err = conn.WriteJSON(clientMsg)
		require.NoError(t, err)

		// 接收服务器响应
		var ackMsg MockMessage
		err = conn.ReadJSON(&ackMsg)
		require.NoError(t, err)
		assert.Equal(t, "ack", ackMsg.Type)

		// 服务器主动发送消息
		serverMsg := MockMessage{
			Type: "server-push",
			Data: map[string]interface{}{
				"from": "server",
			},
			Timestamp: time.Now().Unix(),
		}
		err = mockServer.SendMessage(sessionID, serverMsg)
		require.NoError(t, err)

		// 客户端接收服务器推送
		var receivedMsg MockMessage
		err = conn.ReadJSON(&receivedMsg)
		require.NoError(t, err)
		assert.Equal(t, "server-push", receivedMsg.Type)
	})

	t.Run("消息序列化测试", func(t *testing.T) {
		wsURL := mockServer.GetURL() + "/ws/serialize-session"
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		require.NoError(t, err)
		defer conn.Close()

		// 发送包含复杂数据的消息
		complexMsg := MockMessage{
			Type: "complex",
			Data: map[string]interface{}{
				"string":  "test",
				"number":  42,
				"boolean": true,
				"array":   []interface{}{1, 2, 3},
				"nested": map[string]interface{}{
					"key": "value",
				},
			},
			Timestamp: time.Now().Unix(),
		}

		err = conn.WriteJSON(complexMsg)
		require.NoError(t, err)

		// 验证服务器能正确接收
		var response MockMessage
		err = conn.ReadJSON(&response)
		require.NoError(t, err)
		assert.Equal(t, "ack", response.Type)

		// 验证收到的消息
		messages := mockServer.GetMessages()
		assert.Greater(t, len(messages), 0)

		// 找到我们发送的消息
		found := false
		for _, msg := range messages {
			if msg.Type == "complex" {
				found = true
				assert.NotNil(t, msg.Data)
				break
			}
		}
		assert.True(t, found, "Complex message not found in received messages")
	})

	t.Run("连接超时测试", func(t *testing.T) {
		wsURL := mockServer.GetURL() + "/ws/timeout-session"
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		require.NoError(t, err)
		defer conn.Close()

		// 设置读取超时
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))

		// 发送消息
		msg := MockMessage{
			Type:      "test",
			Timestamp: time.Now().Unix(),
		}
		err = conn.WriteJSON(msg)
		require.NoError(t, err)

		// 应该能收到响应（在超时之前）
		var response MockMessage
		err = conn.ReadJSON(&response)
		require.NoError(t, err)
		assert.Equal(t, "ack", response.Type)
	})

	t.Run("重连测试", func(t *testing.T) {
		sessionID := "reconnect-session"
		wsURL := mockServer.GetURL() + "/ws/" + sessionID

		// 第一次连接
		conn1, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		require.NoError(t, err)

		msg := MockMessage{
			Type:      "before-disconnect",
			Timestamp: time.Now().Unix(),
		}
		err = conn1.WriteJSON(msg)
		require.NoError(t, err)

		// 关闭连接
		conn1.Close()
		time.Sleep(100 * time.Millisecond)

		// 重新连接
		conn2, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		require.NoError(t, err)
		defer conn2.Close()

		msg2 := MockMessage{
			Type:      "after-reconnect",
			Timestamp: time.Now().Unix(),
		}
		err = conn2.WriteJSON(msg2)
		require.NoError(t, err)

		var response MockMessage
		err = conn2.ReadJSON(&response)
		require.NoError(t, err)
		assert.Equal(t, "ack", response.Type)
	})
}

// TestWebSocketMessageProtocol 测试WebSocket消息协议
func TestWebSocketMessageProtocol(t *testing.T) {
	mockServer := NewMockWebSocketServer()
	defer mockServer.Close()

	t.Run("消息类型验证", func(t *testing.T) {
		wsURL := mockServer.GetURL() + "/ws/protocol-session"
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		require.NoError(t, err)
		defer conn.Close()

		messageTypes := []string{
			"init",
			"command",
			"output",
			"error",
			"close",
			"ping",
			"pong",
		}

		for _, msgType := range messageTypes {
			msg := MockMessage{
				Type:      msgType,
				Timestamp: time.Now().Unix(),
			}
			err = conn.WriteJSON(msg)
			require.NoError(t, err)

			var response MockMessage
			err = conn.ReadJSON(&response)
			require.NoError(t, err)
			assert.Equal(t, "ack", response.Type)
		}
	})

	t.Run("时间戳验证", func(t *testing.T) {
		wsURL := mockServer.GetURL() + "/ws/timestamp-session"
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		require.NoError(t, err)
		defer conn.Close()

		now := time.Now().Unix()
		msg := MockMessage{
			Type:      "timestamp-test",
			Timestamp: now,
		}
		err = conn.WriteJSON(msg)
		require.NoError(t, err)

		var response MockMessage
		err = conn.ReadJSON(&response)
		require.NoError(t, err)

		// 验证响应时间戳在合理范围内
		assert.True(t, response.Timestamp >= now)
		assert.True(t, response.Timestamp <= time.Now().Unix()+2)
	})
}
