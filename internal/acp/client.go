package acp

import (
	"bufio"
	"encoding/json"
	"io"
	"os/exec"
	"sync"

	"github.com/open-agents/open-agents-bridge/internal/logger"
)

// ACPClient manages communication with ACP-compatible CLI tools
type ACPClient struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
	
	mu       sync.Mutex
	handlers map[string]func(ACPMessage)
	nextID   int
}

// ACPMessage represents an ACP protocol message
type ACPMessage struct {
	JSONRPC string                 `json:"jsonrpc"`
	ID      int                    `json:"id,omitempty"`
	Method  string                 `json:"method,omitempty"`
	Params  map[string]interface{} `json:"params,omitempty"`
	Result  interface{}            `json:"result,omitempty"`
	Error   *ACPError              `json:"error,omitempty"`
}

// ACPError represents an ACP error
type ACPError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// NewACPClient creates a new ACP client
func NewACPClient(command string, args []string) (*ACPClient, error) {
	cmd := exec.Command(command, args...)
	
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	
	client := &ACPClient{
		cmd:      cmd,
		stdin:    stdin,
		stdout:   stdout,
		stderr:   stderr,
		handlers: make(map[string]func(ACPMessage)),
		nextID:   1,
	}
	
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	
	// Start reading responses
	go client.readLoop()
	go client.readStderr()
	
	return client, nil
}

// Initialize sends initialize request
func (c *ACPClient) Initialize() error {
	msg := ACPMessage{
		JSONRPC: "2.0",
		ID:      c.getNextID(),
		Method:  "initialize",
		Params: map[string]interface{}{
			"clientInfo": map[string]string{
				"name":    "open-agents-bridge",
				"version": "1.0.0",
			},
		},
	}
	
	return c.send(msg)
}

// SendMessage sends a message to the CLI
func (c *ACPClient) SendMessage(content string) error {
	msg := ACPMessage{
		JSONRPC: "2.0",
		ID:      c.getNextID(),
		Method:  "chat/send",
		Params: map[string]interface{}{
			"content": content,
		},
	}
	
	return c.send(msg)
}

// OnToolCall registers a handler for tool call notifications
func (c *ACPClient) OnToolCall(handler func(ACPMessage)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handlers["tool/call"] = handler
}

// OnMessage registers a handler for message notifications
func (c *ACPClient) OnMessage(handler func(ACPMessage)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handlers["chat/message"] = handler
}

// Close closes the ACP client
func (c *ACPClient) Close() error {
	c.stdin.Close()
	return c.cmd.Wait()
}

func (c *ACPClient) send(msg ACPMessage) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	
	c.mu.Lock()
	defer c.mu.Unlock()
	
	_, err = c.stdin.Write(append(data, '\n'))
	return err
}

func (c *ACPClient) readLoop() {
	scanner := bufio.NewScanner(c.stdout)
	for scanner.Scan() {
		var msg ACPMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			logger.Warn("[%s] Parse error: %v", logger.ModACP, err)
			continue
		}
		
		c.handleMessage(msg)
	}
}

func (c *ACPClient) readStderr() {
	scanner := bufio.NewScanner(c.stderr)
	for scanner.Scan() {
		logger.Debug("[%s] stderr: %s", logger.ModACP, scanner.Text())
	}
}

func (c *ACPClient) handleMessage(msg ACPMessage) {
	// Handle notifications (no ID)
	if msg.ID == 0 && msg.Method != "" {
		c.mu.Lock()
		handler := c.handlers[msg.Method]
		c.mu.Unlock()
		
		if handler != nil {
			handler(msg)
		}
		return
	}
	
	// Handle responses
	if msg.ID != 0 {
		logger.Debug("[%s] response: %+v", logger.ModACP, msg)
	}
}

func (c *ACPClient) getNextID() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.nextID
	c.nextID++
	return id
}
