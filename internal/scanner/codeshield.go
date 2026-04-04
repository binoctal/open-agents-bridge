package scanner

import "regexp"

// CodeShieldScanner detects insecure code patterns in AI-generated code.
// Source: LlamaFirewall CodeShield / Insecure Code Detector (ICD)
// Only scans output direction (AI → User).
type CodeShieldScanner struct{}

func (c *CodeShieldScanner) Name() string { return "codeshield" }

var codeRules = []struct {
	id, title, desc, pattern string
	level                    AlertLevel
}{
	// CWE-89: SQL Injection
	{"cwe89_concat", "SQL Injection", "String concatenation in SQL query. Use parameterized queries.",
		`(?i)(?:SELECT|INSERT|UPDATE|DELETE|DROP|ALTER)\s+.*(?:\+\s*\w|\$\{|%s|%v|f['"]|\.format\()`, AlertCritical},
	{"cwe89_fstring", "SQL Injection (f-string)", "F-string in SQL query detected.",
		`(?i)f['"]\s*(?:SELECT|INSERT|UPDATE|DELETE)\s+`, AlertCritical},

	// CWE-79: XSS
	{"cwe79_innerhtml", "XSS Risk", "Direct innerHTML assignment with user input.",
		`(?i)\.innerHTML\s*=`, AlertWarning},
	{"cwe79_dangerously", "XSS Risk (React)", "dangerouslySetInnerHTML usage detected.",
		`dangerouslySetInnerHTML`, AlertWarning},
	{"cwe79_document_write", "XSS Risk", "document.write usage detected.",
		`document\.write\s*\(`, AlertWarning},

	// CWE-798: Hardcoded Credentials
	{"cwe798_hardcoded", "Hardcoded Password", "Hardcoded password in code.",
		`(?i)(?:password|passwd|pwd|secret)\s*=\s*['"][^'"]{4,}['"]`, AlertWarning},

	// CWE-78: Command Injection
	{"cwe78_os_system", "Command Injection", "os.system() with string concatenation.",
		`(?i)os\.system\s*\(\s*(?:f['"]|['"].*\+|.*\.format)`, AlertCritical},
	{"cwe78_subprocess_shell", "Command Injection", "subprocess with shell=True.",
		`(?i)subprocess\.(?:call|run|Popen)\s*\(.*shell\s*=\s*True`, AlertCritical},
	{"cwe78_exec", "Code Execution", "eval()/exec() with external input.",
		`(?i)(?:eval|exec)\s*\(\s*(?:request|input|argv|sys\.stdin|params|query)`, AlertCritical},
	{"cwe78_backtick", "Command Injection (Ruby/Shell)", "Backtick command execution.",
		"(?i)`\\s*(?:\\$\\{|#\\{).*`", AlertWarning},

	// CWE-22: Path Traversal
	{"cwe22_path", "Path Traversal", "File path constructed from user input without validation.",
		`(?i)(?:open|readFile|readFileSync|writeFile)\s*\(\s*(?:request|input|params|query|argv)`, AlertWarning},

	// CWE-502: Insecure Deserialization
	{"cwe502_pickle", "Insecure Deserialization", "pickle.loads() with untrusted data.",
		`(?i)pickle\.loads?\s*\(`, AlertWarning},
	{"cwe502_yaml_load", "Insecure YAML Load", "yaml.load() without SafeLoader.",
		`(?i)yaml\.load\s*\([^)]*\)\s*$`, AlertWarning},
	{"cwe502_marshal", "Insecure Deserialization", "Marshal.load with untrusted data.",
		`(?i)Marshal\.load\s*\(`, AlertWarning},

	// CWE-327: Weak Crypto
	{"cwe327_md5", "Weak Hash", "MD5 is cryptographically broken. Use SHA-256+.",
		`(?i)(?:hashlib\.md5|MD5\.Create|crypto\.createHash\s*\(\s*['"]md5)`, AlertWarning},
	{"cwe327_sha1", "Weak Hash", "SHA-1 is deprecated. Use SHA-256+.",
		`(?i)(?:hashlib\.sha1|SHA1\.Create|crypto\.createHash\s*\(\s*['"]sha1)`, AlertWarning},
}

var compiledCodeRules []compiledRule

func init() {
	for _, r := range codeRules {
		compiledCodeRules = append(compiledCodeRules, compiledRule{
			id: r.id, title: r.title, desc: r.desc, level: r.level,
			pattern: regexp.MustCompile(r.pattern),
		})
	}
}

func (c *CodeShieldScanner) Scan(text string, dir Direction) []Alert {
	// Only scan output direction (AI-generated code)
	if dir != DirOutput {
		return nil
	}
	var alerts []Alert
	for _, r := range compiledCodeRules {
		if loc := r.pattern.FindStringIndex(text); loc != nil {
			match := text[loc[0]:loc[1]]
			if len(match) > 80 {
				match = match[:80] + "..."
			}
			alerts = append(alerts, Alert{
				Category:    CategoryCodeSecurity,
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
