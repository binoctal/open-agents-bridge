package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/open-agents/open-agents-bridge/internal/protocol"
)

// SessionSnapshot represents a point-in-time snapshot of a session
type SessionSnapshot struct {
	SessionID string                 `json:"session_id"`
	CLIType   string                 `json:"cli_type"`
	WorkDir   string                 `json:"work_dir"`
	PermMode  string                 `json:"perm_mode"`
	History   []protocol.Message     `json:"history"`
	Context   map[string]interface{} `json:"context"`
	Timestamp time.Time              `json:"timestamp"`
	Version   string                 `json:"version"`
}

// SnapshotManager manages session snapshots
type SnapshotManager struct {
	snapshotDir string
	mu          sync.RWMutex
}

// NewSnapshotManager creates a new snapshot manager
func NewSnapshotManager(snapshotDir string) *SnapshotManager {
	return &SnapshotManager{
		snapshotDir: snapshotDir,
	}
}

// TakeSnapshot creates a snapshot of a session
func (sm *SnapshotManager) TakeSnapshot(sess *Session, history []protocol.Message) (*SessionSnapshot, error) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	snapshot := &SessionSnapshot{
		SessionID: sess.ID,
		CLIType:   sess.CLIType,
		WorkDir:   sess.WorkDir,
		PermMode:  sess.PermissionMode,
		History:   history,
		Context: map[string]interface{}{
			"status":     sess.Status,
			"created_at": sess.CreatedAt,
			"job_id":     sess.JobID,
			"task_id":    sess.TaskID,
		},
		Timestamp: time.Now(),
		Version:   "1.0",
	}

	// Ensure snapshot directory exists
	if err := os.MkdirAll(sm.snapshotDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create snapshot directory: %w", err)
	}

	// Save snapshot to disk
	snapshotPath := filepath.Join(sm.snapshotDir, fmt.Sprintf("%s.json", sess.ID))
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to marshal snapshot: %w", err)
	}

	if err := os.WriteFile(snapshotPath, data, 0644); err != nil {
		return nil, fmt.Errorf("failed to write snapshot: %w", err)
	}

	return snapshot, nil
}

// RestoreSnapshot restores a session from a snapshot
func (sm *SnapshotManager) RestoreSnapshot(sessionID string) (*SessionSnapshot, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	snapshotPath := filepath.Join(sm.snapshotDir, fmt.Sprintf("%s.json", sessionID))
	data, err := os.ReadFile(snapshotPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("snapshot not found for session %s", sessionID)
		}
		return nil, fmt.Errorf("failed to read snapshot: %w", err)
	}

	var snapshot SessionSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, fmt.Errorf("failed to unmarshal snapshot: %w", err)
	}

	return &snapshot, nil
}

// DeleteSnapshot removes a snapshot
func (sm *SnapshotManager) DeleteSnapshot(sessionID string) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	snapshotPath := filepath.Join(sm.snapshotDir, fmt.Sprintf("%s.json", sessionID))
	if err := os.Remove(snapshotPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete snapshot: %w", err)
	}

	return nil
}

// ListSnapshots returns all available snapshots
func (sm *SnapshotManager) ListSnapshots() ([]string, error) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	entries, err := os.ReadDir(sm.snapshotDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("failed to read snapshot directory: %w", err)
	}

	var sessionIDs []string
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			sessionID := entry.Name()[:len(entry.Name())-5] // Remove .json
			sessionIDs = append(sessionIDs, sessionID)
		}
	}

	return sessionIDs, nil
}

// CleanOldSnapshots removes snapshots older than the specified duration
func (sm *SnapshotManager) CleanOldSnapshots(maxAge time.Duration) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	entries, err := os.ReadDir(sm.snapshotDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read snapshot directory: %w", err)
	}

	now := time.Now()
	cleaned := 0

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		age := now.Sub(info.ModTime())
		if age > maxAge {
			snapshotPath := filepath.Join(sm.snapshotDir, entry.Name())
			if err := os.Remove(snapshotPath); err == nil {
				cleaned++
			}
		}
	}

	return nil
}

// VerifySession verifies that a session is still functional
func VerifySession(sess *Session) error {
	if sess == nil {
		return fmt.Errorf("session is nil")
	}

	if sess.Protocol == nil {
		return fmt.Errorf("session protocol is nil")
	}

	if !sess.Protocol.IsConnected() {
		return fmt.Errorf("session protocol is not connected")
	}

	// Send ping message to verify connection
	pingMsg := protocol.Message{
		Type:    protocol.MessageTypePing,
		Content: "verification_ping",
	}

	if err := sess.Protocol.SendMessage(pingMsg); err != nil {
		return fmt.Errorf("failed to send verification ping: %w", err)
	}

	return nil
}
