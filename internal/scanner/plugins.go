package scanner

import (
	"fmt"
	"regexp"
	"strings"
)

// DangerousCmdScanner detects dangerous system commands in AI output.
// Only scans output direction.
type DangerousCmdScanner struct{}

func (d *DangerousCmdScanner) Name() string { return "dangerous_cmd" }

var cmdRules = []struct {
	id, title, desc, pattern string
	level                    AlertLevel
}{
	{"rm_rf", "Destructive Delete", "Recursive force delete on root path.", `rm\s+-[a-zA-Z]*r[a-zA-Z]*f[a-zA-Z]*\s+/`, AlertCritical},
	{"find_delete", "Destructive Delete", "find with -delete on root path.", `find\s+/\s+.*-delete`, AlertCritical},
	{"curl_pipe", "Pipe to Shell", "Remote script piped to shell.", `curl\s+.*\|\s*(bash|sh|zsh|python)`, AlertWarning},
	{"wget_pipe", "Pipe to Shell", "Remote script piped to shell.", `wget\s+.*\|\s*(bash|sh|zsh|python)`, AlertWarning},
	{"chmod_777", "Insecure Permissions", "World-writable permissions.", `chmod\s+777`, AlertWarning},
	{"chmod_suid", "SUID Bit", "Setting SUID bit on file.", `chmod\s+[u+]*s`, AlertWarning},
	{"dd_disk", "Disk Write", "Direct disk write operation.", `dd\s+if=.*of=/dev/`, AlertCritical},
	{"mkfs", "Format Disk", "Filesystem format command.", `mkfs\.\w+\s+/dev/`, AlertCritical},
	{"etc_write", "System File Write", "Write to system config directory.", `>\s*/etc/`, AlertWarning},
	{"sudoers_edit", "Sudoers Modification", "Modifying sudoers file.", `(?i)(?:visudo|/etc/sudoers)`, AlertWarning},
	{"iptables_flush", "Firewall Flush", "Flushing all firewall rules.", `iptables\s+-F`, AlertWarning},
	{"kill_all", "Kill All Processes", "Killing all processes.", `kill\s+-9\s+-1`, AlertCritical},
}

var compiledCmdRules []compiledRule

func init() {
	for _, r := range cmdRules {
		compiledCmdRules = append(compiledCmdRules, compiledRule{
			id: r.id, title: r.title, desc: r.desc, level: r.level,
			pattern: regexp.MustCompile(r.pattern),
		})
	}
}

func (d *DangerousCmdScanner) Scan(text string, dir Direction) []Alert {
	if dir != DirOutput {
		return nil
	}
	var alerts []Alert
	for _, r := range compiledCmdRules {
		if loc := r.pattern.FindStringIndex(text); loc != nil {
			match := text[loc[0]:loc[1]]
			if len(match) > 60 {
				match = match[:60] + "..."
			}
			alerts = append(alerts, Alert{
				Category:    CategoryDangerousCmd,
				Level:       r.level,
				RuleID:      r.id,
				Title:       r.title,
				Description: r.desc,
				Match:       match,
			})
		}
	}
	return alerts
}

// PathScanner detects access to sensitive file paths.
type PathScanner struct{}

func (p *PathScanner) Name() string { return "path_access" }

var sensitivePaths = []string{
	"~/.ssh", "~/.aws", "~/.gnupg", "~/.config/gcloud",
	"~/.kube/config", "~/.docker/config.json",
	"~/.npmrc", "~/.pypirc", "~/.netrc",
}

func (p *PathScanner) Scan(text string, dir Direction) []Alert {
	if dir != DirOutput {
		return nil
	}
	var alerts []Alert
	for _, path := range sensitivePaths {
		short := path
		if strings.HasPrefix(path, "~/") {
			short = path[2:]
		}
		if strings.Contains(text, path) || strings.Contains(text, short) {
			alerts = append(alerts, Alert{
				Category:    CategoryPathAccess,
				Level:       AlertWarning,
				RuleID:      "path_" + strings.ReplaceAll(strings.ReplaceAll(path, "/", "_"), ".", ""),
				Title:       "Sensitive Path Access",
				Description: fmt.Sprintf("Access to sensitive path %s detected.", path),
				Match:       path,
			})
		}
	}
	return alerts
}

// CustomRuleScanner loads user-defined rules from config.
type CustomRuleScanner struct {
	rules []compiledRule
}

func (c *CustomRuleScanner) Name() string { return "custom" }

// CustomRuleDef is the JSON structure for user-defined rules
type CustomRuleDef struct {
	ID       string `json:"id"`
	Pattern  string `json:"pattern"`
	Category string `json:"category"`
	Level    string `json:"level"`
	Title    string `json:"title"`
	Desc     string `json:"desc"`
}

const maxCustomRules = 50

// NewCustomRuleScanner creates a scanner from user-defined rules.
func NewCustomRuleScanner(defs []CustomRuleDef) *CustomRuleScanner {
	s := &CustomRuleScanner{}
	for i, d := range defs {
		if i >= maxCustomRules {
			break
		}
		p, err := regexp.Compile(d.Pattern)
		if err != nil {
			continue
		}
		lvl := AlertWarning
		if d.Level == "critical" {
			lvl = AlertCritical
		}
		cat := AlertCategory(d.Category)
		if cat == "" {
			cat = CategorySensitiveData
		}
		title := d.Title
		if title == "" {
			title = "Custom Rule: " + d.ID
		}
		s.rules = append(s.rules, compiledRule{
			id: d.ID, title: title, desc: d.Desc, level: lvl, pattern: p,
		})
	}
	return s
}

func (c *CustomRuleScanner) Scan(text string, dir Direction) []Alert {
	// Limit scan input length to prevent regex performance issues
	if len(text) > 10000 {
		text = text[:10000]
	}
	var alerts []Alert
	for _, r := range c.rules {
		if loc := r.pattern.FindStringIndex(text); loc != nil {
			alerts = append(alerts, Alert{
				Category:    CategorySensitiveData,
				Level:       r.level,
				RuleID:      "custom_" + r.id,
				Title:       r.title,
				Description: r.desc,
				Match:       Redact(text[loc[0]:loc[1]]),
			})
		}
	}
	return alerts
}
