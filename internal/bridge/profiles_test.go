package bridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/binoctal/open-agents-bridge/internal/config"
)

func TestProfileStoreTwoSessionsTwoProfiles(t *testing.T) {
	b := &Bridge{}
	b.handleProfileSync(Message{Payload: map[string]interface{}{"profileId": "p1", "env": map[string]interface{}{"ANTHROPIC_API_KEY": "k1"}}})
	b.handleProfileSync(Message{Payload: map[string]interface{}{"profileId": "p2", "env": map[string]interface{}{"ANTHROPIC_API_KEY": "k2"}}})
	b.bindSessionProfile("s1", map[string]interface{}{"profileId": "p1"})
	b.bindSessionProfile("s2", map[string]interface{}{"profileId": "p2"})
	if got := b.profileStore().resolve("s1")["ANTHROPIC_API_KEY"]; got != "k1" {
		t.Fatalf("s1 got %q", got)
	}
	if got := b.profileStore().resolve("s2")["ANTHROPIC_API_KEY"]; got != "k2" {
		t.Fatalf("s2 got %q", got)
	}
}

func TestProfileStoreDefaultHasNoEnv(t *testing.T) {
	b := &Bridge{}
	b.handleProfileSync(Message{Payload: map[string]interface{}{"profileId": "p1", "env": map[string]interface{}{"X": "1"}}})
	if env := b.profileStore().resolve("unbound"); env != nil {
		t.Fatalf("default session must carry no profile env, got %v", env)
	}
}

func TestProfileStoreResyncDropsDeletedKey(t *testing.T) {
	b := &Bridge{}
	b.handleProfileSync(Message{Payload: map[string]interface{}{"profileId": "p1", "env": map[string]interface{}{"A": "1", "B": "2"}}})
	b.bindSessionProfile("s1", map[string]interface{}{"profileId": "p1"})
	b.handleProfileSync(Message{Payload: map[string]interface{}{"profileId": "p1", "env": map[string]interface{}{"A": "1"}}})
	if _, ok := b.profileStore().resolve("s1")["B"]; ok {
		t.Fatal("deleted key must not linger after re-sync")
	}
}

func TestProfileStoreDropCacheOnReconnect(t *testing.T) {
	b := &Bridge{}
	b.handleProfileSync(Message{Payload: map[string]interface{}{"profileId": "p1", "env": map[string]interface{}{"A": "1"}}})
	b.bindSessionProfile("s1", map[string]interface{}{"profileId": "p1"})
	b.profileStore().dropCache()
	if b.profileStore().resolve("s1") != nil {
		t.Fatal("plaintext must be dropped with the connection")
	}
}

func TestConfigSyncDoesNotTouchProcessEnv(t *testing.T) {
	b := &Bridge{config: &config.Config{DeviceID: "dev-1"}}
	t.Setenv("OA_PROFILE_PROBE", "")
	os.Unsetenv("OA_PROFILE_PROBE")
	b.handleConfigSync(Message{Payload: map[string]interface{}{"envVars": map[string]interface{}{"OA_PROFILE_PROBE": "leak"}}})
	if v, ok := os.LookupEnv("OA_PROFILE_PROBE"); ok {
		t.Fatalf("config:sync leaked into process env: %q", v)
	}
}

// Guard: bridge sources must not mutate the process env. The only exception
// is the cloud-sandbox gateway config, which is process-wide by design.
func TestNoOsSetenvInBridgeSources(t *testing.T) {
	root := filepath.Join("..")
	allowed := filepath.Join("config", "session.go")
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		if strings.HasSuffix(p, allowed) {
			return nil
		}
		data, _ := os.ReadFile(p)
		if strings.Contains(string(data), "os.Setenv(") {
			t.Errorf("%s calls os.Setenv; use per-session CustomEnv instead", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
