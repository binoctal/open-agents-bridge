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

	// Connect as web client
	url := fmt.Sprintf("ws://localhost:8787/ws/%s?type=web&token=%s",
		cfg.UserID, cfg.DeviceToken)

	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		log.Fatal("Failed to connect:", err)
	}
	defer conn.Close()

	log.Println("Connected as web client. Starting session...")

	// Send session:start
	msg := Message{
		Type: "session:start",
		Payload: map[string]interface{}{
			"deviceId": cfg.DeviceID,
			"cliType":  "kiro",
			"workDir":  ".",
			"command":  "chat \"Hello, what's 2+2?\"",
		},
		Timestamp: time.Now().UnixMilli(),
	}

	if err := conn.WriteJSON(msg); err != nil {
		log.Fatal("Failed to send:", err)
	}

	log.Println("Session start request sent. Listening for responses...")

	// Listen for responses
	for i := 0; i < 20; i++ {
		var response Message
		if err := conn.ReadJSON(&response); err != nil {
			log.Println("Read error:", err)
			break
		}

		log.Printf("Received: %s", response.Type)
		
		if response.Type == "session:output" {
			payload := response.Payload.(map[string]interface{})
			content := payload["content"].(string)
			outputType := payload["outputType"].(string)
			fmt.Printf("[%s] %s\n", outputType, content)
		} else if response.Type == "session:started" {
			payload := response.Payload.(map[string]interface{})
			sessionId := payload["sessionId"].(string)
			log.Printf("Session started: %s", sessionId)
		}
		
		time.Sleep(100 * time.Millisecond)
	}
}
