package rules

import (
	"path/filepath"
	"strings"

	"github.com/open-agents/open-agents-bridge/internal/config"
)

type Engine struct {
	rules []config.AutoApprovalRule
}

func NewEngine(rules []config.AutoApprovalRule) *Engine {
	return &Engine{rules: rules}
}

func (e *Engine) UpdateRules(rules []config.AutoApprovalRule) {
	e.rules = rules
}

// Evaluate checks permission request against rules
// Returns: action ("auto-approve", "ask", "deny"), matched rule ID
func (e *Engine) Evaluate(tool, path, command string) (string, string) {
	for _, rule := range e.rules {
		if e.matchRule(rule, tool, path, command) {
			return rule.Action, rule.ID
		}
	}
	return "ask", ""
}

func (e *Engine) matchRule(rule config.AutoApprovalRule, tool, path, command string) bool {
	if rule.Tool != "" && rule.Tool != "*" && rule.Tool != tool {
		return false
	}

	if rule.Pattern == "" || rule.Pattern == "*" {
		return true
	}

	// File operations: match path
	if strings.HasPrefix(tool, "fs_") && path != "" {
		if matched, _ := filepath.Match(rule.Pattern, path); matched {
			return true
		}
		if strings.Contains(rule.Pattern, "**") {
			if matched, _ := filepath.Match(strings.ReplaceAll(rule.Pattern, "**", "*"), path); matched {
				return true
			}
		}
	}

	// Commands: match command string
	if tool == "execute_bash" && command != "" {
		if strings.Contains(command, rule.Pattern) || strings.HasPrefix(command, rule.Pattern) {
			return true
		}
	}

	return false
}
