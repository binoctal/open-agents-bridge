package bridge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	b := &Bridge{config: &config.Config{MachineID: "dev-1"}}
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

// A mission task's session id is its task id: task_assign must bind that id to
// the dispatched profile so the env resolver hands the task the profile's env
// instead of silently falling back to CLI login. A non-git project fails the
// task right after the bind, which keeps this test free of a real CLI.
func TestTaskAssignBindsTaskSessionToProfile(t *testing.T) {
	b := newSendBridge(t)
	b.profileStore().put("prof-1", map[string]string{"ANTHROPIC_API_KEY": "sk-task"})

	assign := func(taskID string, extra map[string]interface{}) {
		payload := map[string]interface{}{
			"jobId": "job-p", "taskId": taskID, "agent": "replay", "title": "t",
			"worktreeBranch": "oa/job-p/" + taskID, "projectPath": t.TempDir(),
		}
		for k, v := range extra {
			payload[k] = v
		}
		b.handleWorkflowTaskAssign(Message{Type: "workflow:task_assign", Payload: payload, Timestamp: time.Now().UnixMilli()})
	}

	assign("task-with", map[string]interface{}{"profileId": "prof-1"})
	if env := b.profileStore().resolve("task-with"); env["ANTHROPIC_API_KEY"] != "sk-task" {
		t.Errorf("task bound to prof-1 resolved env = %v, want its key", env)
	}

	assign("task-without", nil)
	if env := b.profileStore().resolve("task-without"); env != nil {
		t.Errorf("task without profileId must resolve no env, got %v", env)
	}
}
