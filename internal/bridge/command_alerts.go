package bridge

import (
	"regexp"
	"time"

	"github.com/binoctal/open-agents-bridge/internal/api"
	"github.com/binoctal/open-agents-bridge/internal/logger"
	"github.com/binoctal/open-agents-bridge/internal/permission"
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

// firstBlockingCommandAlert returns the id of the first hit that blocks, or ""
// if none of them do. The id — not just the fact that something blocked — is
// what the audit record needs.
func firstBlockingCommandAlert(hits []compiledCommandRule) string {
	for _, h := range hits {
		if h.def.Action == "block" {
			if h.def.ID == "" {
				// An unnamed rule still blocks. Returning "" here would read as
				// "nothing blocked" and quietly send the request to the user.
				return "unnamed-command-alert"
			}
			return h.def.ID
		}
	}
	return ""
}

type permissionOutcome int

const (
	// permissionAsk forwards the request to the web client for the user.
	permissionAsk permissionOutcome = iota
	permissionApprove
	permissionDeny
)

// decidePermission works out what happens to a request, which rule decided it,
// and which alert rules it tripped, without acting on any of them. Kept
// separate from the acting so the decision can be tested without a websocket.
//
// The rule id is returned, not just logged, because a locally-resolved request
// is reported to the server for the audit trail and "which rule let this
// through" is the one thing that record is for.
func (b *Bridge) decidePermission(req permission.Request) (permissionOutcome, string, []compiledCommandRule) {
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

	// Match on the tool name, not PermissionType. The rules the user writes
	// name tools the way the CLI does (fs_read, execute_bash, …) — that is the
	// vocabulary the rules UI offers and the vocabulary the engine tests
	// against. PermissionType is the coarse display label (file:read,
	// command:exec), and passing it here meant no rule with a tool or a
	// pattern ever matched: every request fell through to "ask".
	action, ruleID := b.rulesEngine.Evaluate(req.ToolName, path, command)

	// Alerts are evaluated for every request, including the ones the user's own
	// rules answer without asking. An auto-approved dangerous command is
	// exactly the case an admin has no other way of finding out about.
	hits := b.evaluateCommandAlerts(command)

	switch action {
	case "auto-approve":
		b.logInfo("[%s] Auto-approved by rule %s: %s", logger.ModPermission, ruleID, req.Description)
		return permissionApprove, ruleID, hits
	case "deny":
		b.logInfo("[%s] Auto-denied by rule %s: %s", logger.ModPermission, ruleID, req.Description)
		return permissionDeny, ruleID, hits
	}

	// A blocking alert refuses a request that would otherwise have gone to the
	// user. It runs after the user's own rules, never over them.
	if blocker := firstBlockingCommandAlert(hits); blocker != "" {
		b.logInfo("[%s] Denied by command alert rule %s: %s", logger.ModPermission, blocker, req.Description)
		return permissionDeny, blocker, hits
	}

	return permissionAsk, "", hits
}

// handlePermissionRequest is the permission handler's OnRequest callback: it
// decides a request locally where it can, and otherwise forwards it to the web
// client for the user to answer.
func (b *Bridge) handlePermissionRequest(req permission.Request) {
	req.DeviceID = b.config.DeviceID

	outcome, ruleID, hits := b.decidePermission(req)

	if len(hits) > 0 {
		command, _ := req.Detail["command"].(string)
		go b.reportCommandAlerts(req, command, hits)
	}

	// Every branch below that does NOT forward the request to the web client
	// must also report the decision, or it leaves no trace anywhere: the server
	// never sees the request at all, so `permission_requests` would record the
	// device's human approvals and silently omit everything its rules decided.
	// `permission_decision_reported_test.go` enforces this on new branches.
	switch outcome {
	case permissionApprove:
		b.permHandler.Resolve(permission.Response{ID: req.ID, Approved: true})
		go b.reportPermissionDecision(req, true, ruleID)
	case permissionDeny:
		b.permHandler.Resolve(permission.Response{ID: req.ID, Approved: false})
		go b.reportPermissionDecision(req, false, ruleID)
	default:
		b.sendMessage(Message{
			Type:      "permission:request",
			Payload:   req,
			Timestamp: time.Now().UnixMilli(),
		})
	}
}

// reportPermissionDecision records a request this bridge resolved by itself.
//
// Called on its own goroutine, always after the decision has been delivered:
// the report is a round-trip over the public internet and an agent is blocked
// on the answer. A failure costs one audit row and is logged; it never retries
// into the decision path and never changes what was decided.
func (b *Bridge) reportPermissionDecision(req permission.Request, approved bool, ruleID string) {
	if b.apiClient == nil {
		return
	}
	err := b.apiClient.ReportPermissionDecision(api.PermissionDecision{
		ID:             req.ID,
		Approved:       approved,
		RuleID:         ruleID,
		SessionID:      req.SessionID,
		PermissionType: req.PermissionType,
		ToolName:       req.ToolName,
		Description:    req.Description,
		Detail:         req.Detail,
		Risk:           req.Risk,
		DecidedAt:      time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		b.logWarn("[%s] Failed to report permission decision %s: %v", logger.ModPermission, req.ID, err)
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
