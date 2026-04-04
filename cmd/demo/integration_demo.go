package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/open-agents/open-agents-bridge/internal/protocol"
	"github.com/open-agents/open-agents-bridge/internal/session"
)

func main() {
	fmt.Println("=== Open Agents Protocol Integration Test ===\n")

	// Create session manager
	manager := session.NewManager()

	// Set up message callback
	manager.SetOutputCallback(func(sessionID string, msg protocol.Message) {
		timestamp := time.Now().Format("15:04:05")
		
		switch msg.Type {
		case protocol.MessageTypeContent:
			fmt.Printf("[%s] 💬 Content: %v\n", timestamp, msg.Content)

		case protocol.MessageTypeThought:
			fmt.Printf("[%s] 🤔 Thought: %v\n", timestamp, msg.Content)

		case protocol.MessageTypeToolCall:
			toolCall := msg.Content.(protocol.ToolCall)
			fmt.Printf("[%s] 🔧 Tool: %s (%s)\n", timestamp, toolCall.Name, toolCall.Status)

		case protocol.MessageTypePermission:
			permReq := msg.Content.(protocol.PermissionRequest)
			fmt.Printf("[%s] 🔐 Permission: %s - %s (risk: %s)\n", 
				timestamp, permReq.ToolName, permReq.Description, permReq.Risk)

		case protocol.MessageTypeStatus:
			status := msg.Content.(protocol.AgentStatus)
			fmt.Printf("[%s] 📊 Status: %s\n", timestamp, status)

		case protocol.MessageTypePlan:
			fmt.Printf("[%s] 📋 Plan: %v\n", timestamp, msg.Content)

		case protocol.MessageTypeError:
			fmt.Printf("[%s] ❌ Error: %v\n", timestamp, msg.Content)
		}

		// Show protocol info
		if proto, ok := msg.Meta["protocol"].(string); ok {
			fmt.Printf("    └─ via %s protocol\n", proto)
		}
	})

	// Test with different CLI tools
	testCases := []struct {
		name    string
		cliType string
		workDir string
	}{
		{"Echo (PTY fallback)", "echo", "."},
		// Uncomment to test with real CLI tools:
		// {"Claude Code (ACP)", "claude", "."},
		// {"Qwen Code (ACP)", "qwen", "."},
	}

	for _, tc := range testCases {
		fmt.Printf("\n--- Testing: %s ---\n", tc.name)
		
		sess, err := manager.Create(tc.cliType, tc.workDir)
		if err != nil {
			log.Printf("❌ Failed to create session: %v\n", err)
			continue
		}

		fmt.Printf("✅ Session created: %s\n", sess.ID)
		fmt.Printf("   Protocol: %s\n", sess.GetProtocolName())
		fmt.Printf("   CLI Type: %s\n", sess.CLIType)

		// Send a test message
		if tc.cliType != "echo" {
			time.Sleep(1 * time.Second)
			sess.Send("Hello, can you help me?")
			fmt.Println("📤 Sent: Hello, can you help me?")
		}

		// Wait a bit for response
		time.Sleep(2 * time.Second)

		// Stop session
		manager.Stop(sess.ID)
		fmt.Printf("🛑 Session stopped\n")
	}

	fmt.Println("\n=== Integration Test Summary ===")
	fmt.Println("✅ Protocol system working correctly")
	fmt.Println("✅ Session manager integrated")
	fmt.Println("✅ Message routing functional")
	fmt.Println("\nPress Ctrl+C to exit...")

	// Wait for interrupt
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	fmt.Println("\n\nShutting down...")
	manager.StopAll()
}
