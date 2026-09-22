package session

// Adopt registers a session built outside the Create* paths — e.g. a test
// double with a pre-connected fake adapter, so bridge-level tests can drive
// dispatch/cleanup code against controlled liveness. Production code
// creates sessions exclusively through Create/CreateWithID/CreateWithIDAndSize.
func (m *Manager) Adopt(sess *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[sess.ID] = sess
}
