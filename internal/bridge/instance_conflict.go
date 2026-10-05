package bridge

import (
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/binoctal/open-agents-bridge/internal/logger"
)

const (
	// closeCodeAnotherInstance is sent by the server when another live bridge
	// instance already owns this device's connection. It is deliberately NOT a
	// permanent close code: a legitimate restart briefly looks like a conflict
	// until the old connection ages out of the server's liveness window.
	closeCodeAnotherInstance = 4009

	// maxInstanceConflictRetries is how many 4009 rejections are retried
	// (after instanceConflictBackoff each) before the bridge goes on standby.
	maxInstanceConflictRetries = 3
)

// keepAliveInterval is the liveness-frame cadence. The server treats a
// connection silent for 90s as dead, so 30s leaves two missed beats. Var for tests.
var keepAliveInterval = 30 * time.Second

// instanceConflictBackoff is longer than the server's 90s/3 liveness window
// spread so three retries outlast a stale predecessor. Var for tests.
var instanceConflictBackoff = 60 * time.Second

// newInstanceID returns a random per-process identifier. It is regenerated on
// every start so the server can tell a restart from a duplicate process.
func newInstanceID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return time.Now().Format("20060102150405.000000000")
	}
	return hex.EncodeToString(buf)
}

// onInstanceConflict handles a 4009 close. It returns true when the bridge
// should stop reconnecting (standby, or shutdown during the backoff).
func (b *Bridge) onInstanceConflict() bool {
	b.connMu.Lock()
	if b.conn != nil {
		b.conn.Close()
		b.conn = nil
	}
	b.connMu.Unlock()

	b.conflictCount++
	if b.conflictCount > maxInstanceConflictRetries {
		b.standby.Store(true)
		b.logError("[%s] Another bridge instance is already online for this device; this instance is on standby (no reconnects, no heartbeat). Stop the other instance and restart this one.", logger.ModBridge)
		return true
	}

	b.logWarn("[%s] Another bridge instance is online for this device (rejected %d/%d); retrying in %v",
		logger.ModBridge, b.conflictCount, maxInstanceConflictRetries, instanceConflictBackoff)
	select {
	case <-b.done:
		return true
	case <-time.After(instanceConflictBackoff):
		return false
	}
}
