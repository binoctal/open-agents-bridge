package main

import (
	"fmt"
	"log"
	"time"

	"github.com/gorilla/websocket"
	"github.com/open-agents/open-agents-bridge/internal/config"
)

type Message struct {
	Type      string      `json:"type"`
	Payload   interface{} `json:"payload"`
	Timestamp int64       `json:"timestamp"`
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal("Failed to load config:", err)
	}

	// Connect to server
	url := fmt.Sprintf("%s/ws/%s?type=bridge&deviceId=%s&token=%s",
		cfg.ServerURL, cfg.UserID, cfg.DeviceID, cfg.DeviceToken)

	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		log.Fatal("Failed to connect:", err)
	}
	defer conn.Close()

	log.Println("Connected. Sending permission request...")

	// Send permission request
	msg := Message{
		Type: "permission:request",
		Payload: map[string]interface{}{
			"id":             "perm_test_" + fmt.Sprint(time.Now().Unix()),
			"deviceId":       cfg.DeviceID,
			"sessionId":      "session_test",
			"permissionType": "file:write",
			"description":    "Write to file: /tmp/test.txt",
			"detail": map[string]string{
				"path":    "/tmp/test.txt",
				"command": "str_replace",
			},
			"risk":    "medium",
			"timeout": 60,
		},
		Timestamp: time.Now().UnixMilli(),
	}

	if err := conn.WriteJSON(msg); err != nil {
		log.Fatal("Failed to send:", err)
	}

	log.Println("Permission request sent. Waiting for response...")

	// Wait for response
	for {
		var response Message
		if err := conn.ReadJSON(&response); err != nil {
			log.Println("Read error:", err)
			break
		}

		log.Printf("Received: %s", response.Type)
		if response.Type == "permission:response" {
			payload := response.Payload.(map[string]interface{})
			approved := payload["approved"].(bool)
			log.Printf("Permission %s!", map[bool]string{true: "APPROVED", false: "DENIED"}[approved])
			break
		}
	}
}
