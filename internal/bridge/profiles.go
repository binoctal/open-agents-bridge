package bridge

import (
	"sync"

	"github.com/binoctal/open-agents-bridge/internal/logger"
)

// profileStore holds engine-profile env pushed by the user's own web session
// (profile:sync). It lives in memory only and is never written to the bridge
// config or applied with os.Setenv: each session gets its profile env through
// AdapterConfig.CustomEnv, so two sessions can run under two identities.
type profileStore struct {
	mu       sync.RWMutex
	profiles map[string]map[string]string // profileID -> env
	bound    map[string]string            // sessionID -> profileID
}

func newProfileStore() *profileStore {
	return &profileStore{profiles: map[string]map[string]string{}, bound: map[string]string{}}
}

func (p *profileStore) put(id string, env map[string]string) {
	p.mu.Lock()
	p.profiles[id] = env
	p.mu.Unlock()
}

func (p *profileStore) bind(sessionID, profileID string) {
	if sessionID == "" || profileID == "" {
		return
	}
	p.mu.Lock()
	p.bound[sessionID] = profileID
	p.mu.Unlock()
}

// resolve returns a copy of the env for the session's bound profile, or nil
// (default = the CLI's own login).
func (p *profileStore) resolve(sessionID string) map[string]string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	env, ok := p.profiles[p.bound[sessionID]]
	if !ok {
		return nil
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		out[k] = v
	}
	return out
}

// dropCache forgets pushed plaintext (new connection = the API re-pushes);
// session bindings stay so a recreate after reconnect still finds its profile.
func (p *profileStore) dropCache() {
	p.mu.Lock()
	p.profiles = map[string]map[string]string{}
	p.mu.Unlock()
}

func (b *Bridge) handleProfileSync(msg Message) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		return
	}
	id, _ := payload["profileId"].(string)
	raw, _ := payload["env"].(map[string]interface{})
	if id == "" {
		return
	}
	env := make(map[string]string, len(raw))
	for k, v := range raw {
		if s, ok := v.(string); ok {
			env[k] = s
		}
	}
	b.profileStore().put(id, env)
	// Key names only: values are secrets.
	b.logDebug("[%s] Profile %s synced (%d keys)", logger.ModBridge, id, len(env))
}

// bindSessionProfile records which profile a session runs under, from the
// payload of session:start / session:send / session:resume.
func (b *Bridge) bindSessionProfile(sessionID string, payload map[string]interface{}) {
	if pid, _ := payload["profileId"].(string); pid != "" {
		b.profileStore().bind(sessionID, pid)
	}
}

// profileStore returns the lazily created store (tests build Bridge literals).
func (b *Bridge) profileStore() *profileStore {
	b.profilesOnce.Do(func() {
		if b.profiles == nil {
			b.profiles = newProfileStore()
		}
	})
	return b.profiles
}
