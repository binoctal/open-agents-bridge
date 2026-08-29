package rules

import (
	"testing"

	"github.com/binoctal/open-agents-bridge/internal/config"
)

func TestEvaluate_StarPattern(t *testing.T) {
	e := NewEngine([]config.AutoApprovalRule{
		{ID: "r1", Pattern: "*", Tool: "*", Action: "auto-approve"},
	})
	action, id := e.Evaluate("any_tool", "/some/path", "some command")
	if action != "auto-approve" {
		t.Errorf("expected auto-approve, got %s", action)
	}
	if id != "r1" {
		t.Errorf("expected r1, got %s", id)
	}
}

func TestEvaluate_DefaultAsk(t *testing.T) {
	e := NewEngine([]config.AutoApprovalRule{})
	action, id := e.Evaluate("some_tool", "/path", "cmd")
	if action != "ask" {
		t.Errorf("expected ask, got %s", action)
	}
	if id != "" {
		t.Errorf("expected empty id, got %s", id)
	}
}

func TestEvaluate_ToolMismatch(t *testing.T) {
	e := NewEngine([]config.AutoApprovalRule{
		{ID: "r1", Pattern: "*", Tool: "fs_read", Action: "auto-approve"},
	})
	action, _ := e.Evaluate("fs_write", "/file.txt", "")
	if action != "ask" {
		t.Errorf("expected ask for mismatched tool, got %s", action)
	}
}

func TestEvaluate_FsToolPathMatch(t *testing.T) {
	e := NewEngine([]config.AutoApprovalRule{
		{ID: "r1", Pattern: "/home/user/*", Tool: "fs_read", Action: "auto-approve"},
	})
	action, id := e.Evaluate("fs_read", "/home/user/file.txt", "")
	if action != "auto-approve" {
		t.Errorf("expected auto-approve for matching path, got %s", action)
	}
	if id != "r1" {
		t.Errorf("expected r1, got %s", id)
	}
}

func TestEvaluate_FsToolPathMismatch(t *testing.T) {
	e := NewEngine([]config.AutoApprovalRule{
		{ID: "r1", Pattern: "/home/user/*", Tool: "fs_read", Action: "auto-approve"},
	})
	action, _ := e.Evaluate("fs_read", "/etc/passwd", "")
	if action != "ask" {
		t.Errorf("expected ask for non-matching path, got %s", action)
	}
}

func TestEvaluate_ExecuteBashCommandMatch(t *testing.T) {
	e := NewEngine([]config.AutoApprovalRule{
		{ID: "r1", Pattern: "git status", Tool: "execute_bash", Action: "auto-approve"},
	})
	action, id := e.Evaluate("execute_bash", "", "git status --short")
	if action != "auto-approve" {
		t.Errorf("expected auto-approve, got %s", action)
	}
	if id != "r1" {
		t.Errorf("expected r1, got %s", id)
	}
}

func TestEvaluate_RulePriority(t *testing.T) {
	e := NewEngine([]config.AutoApprovalRule{
		{ID: "deny_rule", Pattern: "*", Tool: "*", Action: "deny"},
		{ID: "approve_rule", Pattern: "*", Tool: "*", Action: "auto-approve"},
	})
	action, id := e.Evaluate("any_tool", "", "")
	if action != "deny" {
		t.Errorf("expected first matching rule (deny), got %s", action)
	}
	if id != "deny_rule" {
		t.Errorf("expected deny_rule, got %s", id)
	}
}

func TestEngine_UpdateRules(t *testing.T) {
	e := NewEngine([]config.AutoApprovalRule{
		{ID: "r1", Pattern: "*", Tool: "*", Action: "deny"},
	})
	action, _ := e.Evaluate("any_tool", "", "")
	if action != "deny" {
		t.Fatalf("expected deny before update, got %s", action)
	}

	e.UpdateRules([]config.AutoApprovalRule{
		{ID: "r2", Pattern: "*", Tool: "*", Action: "auto-approve"},
	})
	action, _ = e.Evaluate("any_tool", "", "")
	if action != "auto-approve" {
		t.Errorf("expected auto-approve after update, got %s", action)
	}
}
