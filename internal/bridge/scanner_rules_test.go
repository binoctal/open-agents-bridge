package bridge

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/open-agents/open-agents-bridge/internal/api"
	"github.com/open-agents/open-agents-bridge/internal/config"
	"github.com/open-agents/open-agents-bridge/internal/scanner"
)

// Rules are checked by scanning, not by reading back state: what matters is
// which rules the scanner actually applies after each update, and the merged
// set is only visible from the outside that way.
func newRuleBridge() *Bridge {
	return &Bridge{scanner: scanner.New()}
}

func rule(id, pattern string) scanner.CustomRuleDef {
	return scanner.CustomRuleDef{ID: id, Pattern: pattern, Title: id, Level: "warning"}
}

// hits reports the rule ids that fire against the text.
func hits(b *Bridge, text string) map[string]bool {
	out := map[string]bool{}
	for _, a := range b.scanner.Scan(text) {
		out[a.RuleID] = true
	}
	return out
}

func TestClearingUserRulesKeepsOrgRules(t *testing.T) {
	b := newRuleBridge()
	b.setOrgScannerRules([]scanner.CustomRuleDef{rule("org_secret", "ORGTOKEN")})
	b.setUserScannerRules([]scanner.CustomRuleDef{rule("mine", "MYTOKEN")})

	if got := hits(b, "ORGTOKEN MYTOKEN"); !got["custom_org_secret"] || !got["custom_mine"] {
		t.Fatalf("both rule sets should be live, got %v", got)
	}

	// The settings page sends the user's whole list; an empty list means the
	// user deleted their rules, not that the admin's rules go with them.
	b.setUserScannerRules(nil)

	got := hits(b, "ORGTOKEN MYTOKEN")
	if !got["custom_org_secret"] {
		t.Error("org rule was cleared by a user-side update")
	}
	if got["custom_mine"] {
		t.Error("user rule should be gone")
	}
}

func TestOrgRefreshKeepsUserRules(t *testing.T) {
	b := newRuleBridge()
	b.setUserScannerRules([]scanner.CustomRuleDef{rule("mine", "MYTOKEN")})
	b.setOrgScannerRules([]scanner.CustomRuleDef{rule("org_a", "ATOKEN")})

	// An admin removes one rule and adds another.
	b.setOrgScannerRules([]scanner.CustomRuleDef{rule("org_b", "BTOKEN")})

	got := hits(b, "ATOKEN BTOKEN MYTOKEN")
	if !got["custom_mine"] {
		t.Error("user rule was cleared by an org-side update")
	}
	if !got["custom_org_b"] {
		t.Error("new org rule not applied")
	}
	if got["custom_org_a"] {
		t.Error("removed org rule still firing")
	}
}

func TestRuleCapDropsUserRulesNotOrgRules(t *testing.T) {
	// The scanner keeps only the first N custom rules. Ordering decides who
	// loses the overflow, and it must not be the admin.
	var org []scanner.CustomRuleDef
	for i := 0; i < 50; i++ {
		org = append(org, rule(fmt.Sprintf("org_%d", i), fmt.Sprintf("ORGTOKEN%d", i)))
	}

	b := newRuleBridge()
	b.setOrgScannerRules(org)
	b.setUserScannerRules([]scanner.CustomRuleDef{rule("mine", "MYTOKEN")})

	got := hits(b, "ORGTOKEN0 ORGTOKEN49 MYTOKEN")
	if !got["custom_org_0"] || !got["custom_org_49"] {
		t.Errorf("org rules should survive the cap, got %v", got)
	}
	if got["custom_mine"] {
		t.Error("user rule should have been dropped by the cap, not an org rule")
	}
}

func TestInvalidPatternDoesNotDisableTheRest(t *testing.T) {
	b := newRuleBridge()
	b.setOrgScannerRules([]scanner.CustomRuleDef{
		rule("org_broken", "([unclosed"),
		rule("org_good", "GOODTOKEN"),
	})

	got := hits(b, "GOODTOKEN")
	if !got["custom_org_good"] {
		t.Errorf("a rule that does not compile must not take the others down, got %v", got)
	}
}

func TestOrgSyncFailureLeavesUserRulesScanning(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	b := newRuleBridge()
	b.apiClient = api.NewClient(&config.Config{ServerURL: ts.URL})
	b.setUserScannerRules([]scanner.CustomRuleDef{rule("mine", "MYTOKEN")})

	b.syncOrgScannerRulesFromAPI()

	if got := hits(b, "MYTOKEN"); !got["custom_mine"] {
		t.Errorf("a failed org sync must not stop the device scanning, got %v", got)
	}
}

func TestOrgSyncAppliesFetchedRules(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"rules":[{"id":"org_r1","pattern":"ORGTOKEN","category":"secrets","level":"critical","title":"Org rule","desc":"d"}]}`)
	}))
	defer ts.Close()

	b := newRuleBridge()
	b.apiClient = api.NewClient(&config.Config{ServerURL: ts.URL})

	b.syncOrgScannerRulesFromAPI()

	alerts := b.scanner.Scan("ORGTOKEN")
	if len(alerts) != 1 {
		t.Fatalf("expected the fetched rule to fire, got %v", alerts)
	}
	if alerts[0].Level != scanner.AlertCritical {
		t.Errorf("severity should survive the trip, got %v", alerts[0].Level)
	}
}

// The settings page pushes the user's rules over the websocket. That path,
// not just the state helpers, has to route through the merge.
func TestScannerRulesSyncMessageKeepsOrgRules(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // SaveScannerRules writes under the config dir

	b := newRuleBridge()
	b.config = &config.Config{DeviceID: "dev-1"}
	b.setOrgScannerRules([]scanner.CustomRuleDef{rule("org_secret", "ORGTOKEN")})

	b.handleScannerRulesSync(Message{
		Type: "scanner:rules:sync",
		Payload: map[string]interface{}{
			"rules": []interface{}{
				map[string]interface{}{"id": "mine", "pattern": "MYTOKEN", "title": "Mine", "level": "warning"},
			},
		},
	})

	got := hits(b, "ORGTOKEN MYTOKEN")
	if !got["custom_org_secret"] {
		t.Error("a user-side sync cleared the org rules")
	}
	if !got["custom_mine"] {
		t.Error("the pushed user rule is not live")
	}
}

func TestStaleOrgRulesAreNotKeptAfterAFailedSync(t *testing.T) {
	// An admin who deletes a rule expects it to stop firing even if the next
	// sync fails; carrying the old set forward would keep enforcing it.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	b := newRuleBridge()
	b.apiClient = api.NewClient(&config.Config{ServerURL: ts.URL})
	b.setOrgScannerRules([]scanner.CustomRuleDef{rule("org_old", "OLDTOKEN")})

	b.syncOrgScannerRulesFromAPI()

	if got := hits(b, "OLDTOKEN"); got["custom_org_old"] {
		t.Error("a deleted org rule kept firing after a failed sync")
	}
}
