package permission

import (
	"sync"
	"time"
)

// Request represents a permission request from CLI
type Request struct {
	ID             string         `json:"id"`
	SessionID      string         `json:"sessionId"`
	DeviceID       string         `json:"deviceId"`
	PermissionType string         `json:"permissionType"` // file:read | file:write | command:exec
	Description    string         `json:"description"`
	Detail         map[string]any `json:"detail,omitempty"`
	Risk           string         `json:"risk"` // low | medium | high
	Timeout        int            `json:"timeout"`
}

// Response represents a permission response
type Response struct {
	ID       string `json:"id"`
	Approved bool   `json:"approved"`
}

// Handler manages pending permission requests
type Handler struct {
	pending  map[string]chan Response
	mu       sync.Mutex
	onRequest func(Request)
}

func NewHandler() *Handler {
	return &Handler{
		pending: make(map[string]chan Response),
	}
}

// OnRequest sets callback for new permission requests
func (h *Handler) OnRequest(callback func(Request)) {
	h.onRequest = callback
}

// Submit adds a new permission request and waits for response
func (h *Handler) Submit(req Request) (bool, error) {
	h.mu.Lock()
	ch := make(chan Response, 1)
	h.pending[req.ID] = ch
	h.mu.Unlock()

	defer func() {
		h.mu.Lock()
		delete(h.pending, req.ID)
		h.mu.Unlock()
	}()

	if h.onRequest != nil {
		h.onRequest(req)
	}

	timeout := time.Duration(req.Timeout) * time.Second
	if timeout == 0 {
		timeout = 60 * time.Second
	}

	select {
	case resp := <-ch:
		return resp.Approved, nil
	case <-time.After(timeout):
		return false, nil
	}
}

// Resolve completes a pending permission request
func (h *Handler) Resolve(resp Response) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if ch, ok := h.pending[resp.ID]; ok {
		ch <- resp
	}
}

// GetPending returns all pending requests
func (h *Handler) GetPending() []Request {
	h.mu.Lock()
	defer h.mu.Unlock()

	var reqs []Request
	for id := range h.pending {
		reqs = append(reqs, Request{ID: id})
	}
	return reqs
}
