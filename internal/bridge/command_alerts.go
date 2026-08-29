package bridge

import (
	"regexp"
	"time"

	"github.com/open-agents/open-agents-bridge/internal/api"
	"github.com/open-agents/open-agents-bridge/internal/logger"
	"github.com/open-agents/open-agents-bridge/internal/permission"
)

// Command alert rules are matched against the command a permission request
// wants to run. They are an admin's view of what is worth knowing about, and
// deliberately separate from the auto-approval rules the user configures: an
// alert records what happened, it does not decide what happens. The one
// exception is `block`, which turns a request that would have gone to the user
// into a refusal — and even that never overrides an auto-approval the user
// already set up. Silently ignoring their own rule would be the worse surprise.

type compiledCommandRule struct {
	def     api.CommandAlertRule
	pattern *regexp.Regexp
}

// setCommandAlertRules compiles and installs the rule set. Rules that do not
// compile are dropped with a warning rather than taking the rest down with
// them; the pattern comes from an admin's text field, and one typo must not
// disable the pipeline.
func (b *Bridge) setCommandAlertRules(defs []api.CommandAlertRule) {
	compiled := make([]compiledCommandRule, 0, len(defs))
	for _, d := range defs {
		p, err := regexp.Compile(d.Pattern)
		if err != nil {
			b.logWarn("[%s] Command alert rule %s has an invalid pattern, skipped: %v", logger.ModPermission, d.ID, err)
			continue
		}
		compiled = append(compiled, compiledCommandRule{def: d, pattern: p})
	}

	b.commandAlertsMu.Lock()
	b.commandAlertRules = compiled
	b.commandAlertsMu.Unlock()
}

// syncCommandAlertRulesFromAPI fetches the rule set. A failure degrades to no
// rules: permission handling is on the critical path of every tool call, and
// it must not depend on the API being reachable.
func (b *Bridge) syncCommandAlertRulesFromAPI() {
	rules, err := b.apiClient.GetCommandAlertRules()
	if err != nil {
		b.logWarn("[%s] Failed to sync command alert rules from API: %v", logger.ModPermission, err)
		b.setCommandAlertRules(nil)
		return
	}

	b.setCommandAlertRules(rules)
	b.logInfo("[%s] Synced %d command alert rules from API", logger.ModPermission, len(rules))
}

// evaluateCommandAlerts returns every rule the command matches. Every hit is
// its own alert: two rules firing on one command are two things an admin
// wanted to know about.
func (b *Bridge) evaluateCommandAlerts(command string) []compiledCommandRule {
	if command == "" {
		return nil
	}

	b.commandAlertsMu.Lock()
	rules := b.commandAlertRules
	b.commandAlertsMu.Unlock()

	var hits []compiledCommandRule
	for _, r := range rules {
		if r.pattern.MatchString(command) {
			hits = append(hits, r)
		}
	}
	return hits
}

// strictestCommandAlertAction returns "block" if any hit blocks, else "warn".
func strictestCommandAlertAction(hits []compiledCommandRule) string {
	for _, h := range hits {
		if h.def.Action == "block" {
			return "block"
		}
	}
	return "warn"
}

type permissionOutcome int

const (
	// permissionAsk forwards the request to the web client for the user.
	permissionAsk permissionOutcome = iota
	permissionApprove
	permissionDeny
)

// decidePermission works out what happens to a request and which alert rules
// it tripped, without acting on either. Kept separate from the acting so the
// decision can be tested without a websocket.
func (b *Bridge) decidePermission(req permission.Request) (permissionOutcome, []compiledCommandRule) {
	// Check auto-approval rules
	path := ""
	command := ""
	if req.Detail != nil {
		if p, ok := req.Detail["path"].(string); ok {
			path = p
		}
		if c, ok := req.Detail["command"].(string); ok {
			command = c
		}
	}

	action, ruleID := b.rulesEngine.Evaluate(req.PermissionType, path, command)

	// Alerts are evaluated for every request, including the ones the user's own
	// rules answer without asking. An auto-approved dangerous command is
	// exactly the case an admin has no other way of finding out about.
	hits := b.evaluateCommandAlerts(command)

	switch action {
	case "auto-approve":
		b.logInfo("[%s] Auto-approved by rule %s: %s", logger.ModPermission, ruleID, req.Description)
		return permissionApprove, hits
	case "deny":
		b.logInfo("[%s] Auto-denied by rule %s: %s", logger.ModPermission, ruleID, req.Description)
		return permissionDeny, hits
	}

	// A blocking alert refuses a request that would otherwise have gone to the
	// user. It runs after the user's own rules, never over them.
	if strictestCommandAlertAction(hits) == "block" {
		b.logInfo("[%s] Denied by command alert rule: %s", logger.ModPermission, req.Description)
		return permissionDeny, hits
	}

	return permissionAsk, hits
}

// handlePermissionRequest is the permission handler's OnRequest callback: it
// decides a request locally where it can, and otherwise forwards it to the web
// client for the user to answer.
func (b *Bridge) handlePermissionRequest(req permission.Request) {
	req.DeviceID = b.config.DeviceID

	outcome, hits := b.decidePermission(req)

	if len(hits) > 0 {
		command, _ := req.Detail["command"].(string)
		go b.reportCommandAlerts(req, command, hits)
	}

	switch outcome {
	case permissionApprove:
		b.permHandler.Resolve(permission.Response{ID: req.ID, Approved: true})
	case permissionDeny:
		b.permHandler.Resolve(permission.Response{ID: req.ID, Approved: false})
	default:
		b.sendMessage(Message{
			Type:      "permission:request",
			Payload:   req,
			Timestamp: time.Now().UnixMilli(),
		})
	}
}

// reportCommandAlerts files one alert per hit. Called on its own goroutine:
// the network round-trip must never sit in front of a permission decision.
func (b *Bridge) reportCommandAlerts(req permission.Request, command string, hits []compiledCommandRule) {
	for _, h := range hits {
		title := h.def.Name
		if title == "" {
			title = "Command alert: " + h.def.ID
		}
		err := b.apiClient.ReportSecurityAlert(api.SecurityAlert{
			RuleID:      h.def.ID,
			Severity:    h.def.Severity,
			Title:       title,
			Description: h.def.Description,
			Command:     command,
			SessionID:   req.SessionID,
		})
		if err != nil {
			b.logWarn("[%s] Failed to report command alert %s: %v", logger.ModPermission, h.def.ID, err)
		}
	}
}
