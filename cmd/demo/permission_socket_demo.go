package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"time"
)

type SocketRequest struct {
	Type string                 `json:"type"`
	Data map[string]interface{} `json:"data"`
}

type SocketResponse struct {
	Approved bool `json:"approved"`
}

func main() {
	// Connect to Bridge's Unix socket
	conn, err := net.Dial("unix", "/tmp/open-agents.sock")
	if err != nil {
		log.Fatal("Failed to connect to socket:", err)
	}
	defer conn.Close()

	log.Println("Connected to Bridge socket. Sending permission request...")

	// Send permission request
	req := SocketRequest{
		Type: "permission_request",
		Data: map[string]interface{}{
			"action":      "file_write",
			"path":        "/tmp/test.txt",
			"risk":        "medium",
			"description": "Write to test file",
		},
	}

	encoder := json.NewEncoder(conn)
	if err := encoder.Encode(req); err != nil {
		log.Fatal("Failed to send request:", err)
	}

	log.Println("Request sent. Waiting for response...")

	// Set read timeout
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))

	// Read response
	var resp SocketResponse
	decoder := json.NewDecoder(conn)
	if err := decoder.Decode(&resp); err != nil {
		log.Fatal("Failed to read response:", err)
	}

	if resp.Approved {
		fmt.Println("✅ Permission APPROVED!")
	} else {
		fmt.Println("❌ Permission DENIED!")
	}
}
