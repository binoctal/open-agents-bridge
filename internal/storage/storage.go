package storage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Message represents a chat message
type Message struct {
	ID        string    `json:"id"`
	SessionID string    `json:"sessionId"`
	Role      string    `json:"role"` // user, assistant, system
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

// SessionHistory stores messages for a session
type SessionHistory struct {
	SessionID string    `json:"sessionId"`
	DeviceID  string    `json:"deviceId"`
	CLIType   string    `json:"cliType"`
	WorkDir   string    `json:"workDir"`
	Messages  []Message `json:"messages"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Store manages session history persistence
type Store struct {
	dir      string
	sessions map[string]*SessionHistory
	mu       sync.RWMutex
}

// NewStore creates a new storage instance
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	s := &Store{
		dir:      dir,
		sessions: make(map[string]*SessionHistory),
	}
	s.loadAll()
	return s, nil
}

// CreateSession creates a new session
func (s *Store) CreateSession(sessionID, deviceID, cliType, workDir string) *SessionHistory {
	s.mu.Lock()
	defer s.mu.Unlock()

	h := &SessionHistory{
		SessionID: sessionID,
		DeviceID:  deviceID,
		CLIType:   cliType,
		WorkDir:   workDir,
		Messages:  []Message{},
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	s.sessions[sessionID] = h
	s.save(sessionID)
	return h
}

// AddMessage adds a message to a session
func (s *Store) AddMessage(sessionID string, msg Message) {
	s.mu.Lock()
	defer s.mu.Unlock()

	h, ok := s.sessions[sessionID]
	if !ok {
		h = &SessionHistory{
			SessionID: sessionID,
			Messages:  []Message{},
			CreatedAt: time.Now(),
		}
		s.sessions[sessionID] = h
	}

	msg.SessionID = sessionID
	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now()
	}
	h.Messages = append(h.Messages, msg)
	h.UpdatedAt = time.Now()
	s.save(sessionID)
}

// GetSession returns a session by ID
func (s *Store) GetSession(sessionID string) *SessionHistory {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessions[sessionID]
}

// GetMessages returns messages for a session
func (s *Store) GetMessages(sessionID string, limit int) []Message {
	s.mu.RLock()
	defer s.mu.RUnlock()

	h, ok := s.sessions[sessionID]
	if !ok {
		return nil
	}

	msgs := h.Messages
	if limit > 0 && len(msgs) > limit {
		msgs = msgs[len(msgs)-limit:]
	}
	return msgs
}

// ListSessions returns all sessions
func (s *Store) ListSessions() []*SessionHistory {
	s.mu.RLock()
	defer s.mu.RUnlock()

	list := make([]*SessionHistory, 0, len(s.sessions))
	for _, h := range s.sessions {
		list = append(list, h)
	}
	return list
}

func (s *Store) save(sessionID string) {
	h, ok := s.sessions[sessionID]
	if !ok {
		return
	}

	data, _ := json.MarshalIndent(h, "", "  ")
	path := filepath.Join(s.dir, sessionID+".json")
	os.WriteFile(path, data, 0644)
}

func (s *Store) loadAll() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}

	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}

		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}

		var h SessionHistory
		if json.Unmarshal(data, &h) == nil {
			s.sessions[h.SessionID] = &h
		}
	}
}
