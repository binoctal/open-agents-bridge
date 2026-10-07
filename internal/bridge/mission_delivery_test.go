package bridge

import (
	"testing"

	"github.com/binoctal/open-agents-bridge/internal/config"
)

func TestTaskStartedWithBaseCarriesBaseline(t *testing.T) {
	b := &Bridge{config: &config.Config{MachineID: "dev-1"}}

	// Unknown task: plain frame, no base fields.
	p := b.taskStartedWithBase("job-1", "t-0").Payload.(map[string]interface{})
	if _, has := p["baseCommit"]; has {
		t.Fatalf("unexpected baseCommit for unknown task: %v", p)
	}

	b.setTaskBase("t-1", taskBase{Branch: "main", Commit: "abc123"})
	p = b.taskStartedWithBase("job-1", "t-1").Payload.(map[string]interface{})
	if p["baseBranch"] != "main" || p["baseCommit"] != "abc123" {
		t.Fatalf("base fields missing: %v", p)
	}
	if p["taskId"] != "t-1" || p["jobId"] != "job-1" {
		t.Fatalf("core fields lost: %v", p)
	}
}
