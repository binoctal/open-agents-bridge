package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

// Local dev server for Bridge <-> Web communication
// Run: go run scripts/dev-server.go

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type Client struct {
	conn     *websocket.Conn
	userID   string
	clientType string // "web" or "bridge"
	deviceID string
}

type Hub struct {
	clients map[*Client]bool
	mu      sync.RWMutex
}

var hub = &Hub{clients: make(map[*Client]bool)}

func (h *Hub) register(c *Client) {
	h.mu.Lock()
	h.clients[c] = true
	h.mu.Unlock()
	log.Printf("[+] %s connected: user=%s device=%s", c.clientType, c.userID, c.deviceID)
	h.broadcastStatus()
}

func (h *Hub) unregister(c *Client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	log.Printf("[-] %s disconnected: user=%s", c.clientType, c.userID)
	h.broadcastStatus()
}

func (h *Hub) broadcastStatus() {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var bridges, webs int
	for c := range h.clients {
		if c.clientType == "bridge" {
			bridges++
		} else {
			webs++
		}
	}
	log.Printf("[status] bridges=%d webs=%d", bridges, webs)
}

func (h *Hub) forward(from *Client, msg []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for c := range h.clients {
		if c.userID == from.userID && c != from {
			c.conn.WriteMessage(websocket.TextMessage, msg)
		}
	}
}

func handleWS(w http.ResponseWriter, r *http.Request) {
	userID := r.URL.Query().Get("userId")
	if userID == "" {
		userID = r.PathValue("userId")
	}
	clientType := r.URL.Query().Get("type")
	deviceID := r.URL.Query().Get("deviceId")

	if userID == "" {
		http.Error(w, "userId required", http.StatusBadRequest)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade error: %v", err)
		return
	}

	client := &Client{
		conn:       conn,
		userID:     userID,
		clientType: clientType,
		deviceID:   deviceID,
	}

	hub.register(client)
	defer hub.unregister(client)
	defer conn.Close()

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}

		var data map[string]interface{}
		if err := json.Unmarshal(msg, &data); err == nil {
			log.Printf("[%s->] %s: %s", client.clientType, data["type"], string(msg)[:min(100, len(msg))])
		}

		hub.forward(client, msg)
	}
}

func handlePair(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Code string `json:"code"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	// Mock pairing response
	resp := map[string]interface{}{
		"userId":      "dev_user_123",
		"deviceId":    fmt.Sprintf("dev_device_%s", req.Code),
		"deviceToken": fmt.Sprintf("token_%s", req.Code),
		"serverUrl":   "ws://localhost:8787",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
	log.Printf("[pair] code=%s -> deviceId=%s", req.Code, resp["deviceId"])
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("ok"))
}

func main() {
	http.HandleFunc("/ws/{userId}", handleWS)
	http.HandleFunc("/api/pair", handlePair)
	http.HandleFunc("/health", handleHealth)

	// CORS middleware
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == "OPTIONS" {
			return
		}
		http.DefaultServeMux.ServeHTTP(w, r)
	})

	addr := ":8787"
	log.Printf("Dev server starting on http://localhost%s", addr)
	log.Printf("  WebSocket: ws://localhost%s/ws/{userId}", addr)
	log.Printf("  Pair API:  http://localhost%s/api/pair", addr)
	log.Fatal(http.ListenAndServe(addr, handler))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
