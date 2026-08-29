package bridge

import (
	"github.com/open-agents/open-agents-bridge/internal/api"
	"github.com/open-agents/open-agents-bridge/internal/config"
	"github.com/open-agents/open-agents-bridge/internal/logger"
	"github.com/open-agents/open-agents-bridge/internal/scanner"
)

// Custom scanner rules reach this device from two places: the admin panel
// pushes organization-wide rules through the API, and the user maintains their
// own set from the settings page. The scanner has a single "custom" plugin and
// ReplaceCustomRules swaps it out entirely, so the two sets are held here
// separately and merged on every change. Pushing either one straight through
// would silently delete the other.

// mergeScannerRules puts organization rules ahead of the user's. Order is not
// cosmetic: NewCustomRuleScanner keeps only the first maxCustomRules entries,
// so a user with a long list of their own cannot push an admin rule off the
// end.
func mergeScannerRules(org, user []scanner.CustomRuleDef) []scanner.CustomRuleDef {
	merged := make([]scanner.CustomRuleDef, 0, len(org)+len(user))
	merged = append(merged, org...)
	merged = append(merged, user...)
	return merged
}

// applyScannerRules hands the merged set to the scanner.
func (b *Bridge) applyScannerRules() {
	b.scannerRulesMu.Lock()
	merged := mergeScannerRules(b.orgScannerRules, b.userScannerRules)
	orgCount, userCount := len(b.orgScannerRules), len(b.userScannerRules)
	b.scannerRulesMu.Unlock()

	b.scanner.ReplaceCustomRules(merged)
	b.logDebug("[%s] Custom rules applied: %d org + %d user", logger.ModScanner, orgCount, userCount)
}

// setUserScannerRules replaces the user's half and re-applies.
func (b *Bridge) setUserScannerRules(defs []scanner.CustomRuleDef) {
	b.scannerRulesMu.Lock()
	b.userScannerRules = defs
	b.scannerRulesMu.Unlock()
	b.applyScannerRules()
}

// setOrgScannerRules replaces the organization's half and re-applies.
func (b *Bridge) setOrgScannerRules(defs []scanner.CustomRuleDef) {
	b.scannerRulesMu.Lock()
	b.orgScannerRules = defs
	b.scannerRulesMu.Unlock()
	b.applyScannerRules()
}

// loadUserScannerRules seeds the user's half from disk at startup.
func (b *Bridge) loadUserScannerRules() {
	b.setUserScannerRules(scanner.LoadCustomRuleDefs(config.ConfigDir()))
}

// syncOrgScannerRulesFromAPI fetches the organization rules. A failure
// degrades to an empty organization set: the device keeps scanning with the
// builtin plugins and the user's own rules, and the next sync restores the
// rest. Carrying a stale set forward would instead keep enforcing a rule an
// admin has already deleted.
func (b *Bridge) syncOrgScannerRulesFromAPI() {
	rules, err := b.apiClient.GetScannerRules()
	if err != nil {
		b.logWarn("[%s] Failed to sync scanner rules from API: %v", logger.ModScanner, err)
		b.setOrgScannerRules(nil)
		return
	}

	b.setOrgScannerRules(orgRuleDefs(rules))
	b.logInfo("[%s] Synced %d org scanner rules from API", logger.ModScanner, len(rules))
}

// orgRuleDefs converts the transport shape into the scanner's.
func orgRuleDefs(rules []api.ScannerRule) []scanner.CustomRuleDef {
	defs := make([]scanner.CustomRuleDef, 0, len(rules))
	for _, r := range rules {
		defs = append(defs, scanner.CustomRuleDef{
			ID:       r.ID,
			Pattern:  r.Pattern,
			Category: r.Category,
			Level:    r.Level,
			Title:    r.Title,
			Desc:     r.Desc,
		})
	}
	return defs
}
