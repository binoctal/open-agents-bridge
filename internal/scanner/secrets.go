package scanner

import "regexp"

// SecretsScanner detects API keys, tokens, private keys, and credentials.
// Sources: detect-secrets (Yelp), LLM Guard secrets scanner
type SecretsScanner struct{}

func (s *SecretsScanner) Name() string { return "secrets" }

var secretRules = []struct {
	id, title, desc, pattern string
	level                    AlertLevel
}{
	// Exact format matches (near-zero false positives)
	{"aws_key", "AWS Access Key", "AWS access key detected. Consider rotating.", `AKIA[0-9A-Z]{16}`, AlertCritical},
	{"aws_secret", "AWS Secret Key", "AWS secret key detected. Rotate immediately.", `(?i)aws_secret_access_key\s*[:=]\s*[A-Za-z0-9/+=]{40}`, AlertCritical},
	{"private_key", "Private Key", "Private key content detected.", `-----BEGIN\s+(RSA|EC|DSA|OPENSSH|PGP|ENCRYPTED)?\s*PRIVATE KEY-----`, AlertCritical},
	{"github_token", "GitHub Token", "GitHub personal access token detected.", `ghp_[A-Za-z0-9]{36}`, AlertCritical},
	{"github_oauth", "GitHub OAuth Token", "GitHub OAuth token detected.", `gho_[A-Za-z0-9]{36}`, AlertCritical},
	{"slack_token", "Slack Token", "Slack token detected.", `xox[baprs]-[0-9a-zA-Z\-]{10,}`, AlertCritical},
	{"google_api", "Google API Key", "Google API key detected.", `AIza[0-9A-Za-z\-_]{35}`, AlertCritical},
	{"stripe_live", "Stripe Live Key", "Stripe live secret key detected.", `sk_live_[0-9a-zA-Z]{24,}`, AlertCritical},
	{"stripe_restricted", "Stripe Restricted Key", "Stripe restricted key detected.", `rk_live_[0-9a-zA-Z]{24,}`, AlertWarning},
	{"jwt_token", "JWT Token", "JSON Web Token detected.", `eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`, AlertWarning},
	{"npm_token", "NPM Token", "NPM access token detected.", `npm_[A-Za-z0-9]{36}`, AlertCritical},
	{"heroku_api", "Heroku API Key", "Heroku API key detected.", `(?i)heroku.*[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`, AlertWarning},

	// Context-triggered (keyword + value)
	{"generic_secret", "Possible Secret", "Possible secret or token found.", `(?i)(api[_-]?key|secret[_-]?key|access[_-]?token|auth[_-]?token|client[_-]?secret)\s*[:=]\s*['"]?[A-Za-z0-9_\-/.+]{20,}`, AlertWarning},
	{"password_field", "Password Value", "Password value detected.", `(?i)(password|passwd|pwd|pass)\s*[:=]\s*['"]?\S{6,}`, AlertWarning},
	{"connection_string", "Connection String", "Database connection string with credentials.", `(?i)(mysql|postgres|postgresql|mongodb|redis|amqp|mssql)://[^:\s]+:[^@\s]+@`, AlertWarning},
	{"bearer_token", "Bearer Token", "Authorization bearer token detected.", `(?i)bearer\s+[A-Za-z0-9_\-\.]{20,}`, AlertWarning},
}

var compiledSecretRules []compiledRule

func init() {
	for _, r := range secretRules {
		compiledSecretRules = append(compiledSecretRules, compiledRule{
			id: r.id, title: r.title, desc: r.desc, level: r.level,
			pattern: regexp.MustCompile(r.pattern),
		})
	}
}

type compiledRule struct {
	id, title, desc string
	level           AlertLevel
	pattern         *regexp.Regexp
}

func (s *SecretsScanner) Scan(text string, dir Direction) []Alert {
	var alerts []Alert
	for _, r := range compiledSecretRules {
		if loc := r.pattern.FindStringIndex(text); loc != nil {
			alerts = append(alerts, Alert{
				Category:    CategorySensitiveData,
				Level:       r.level,
				RuleID:      r.id,
				Title:       r.title,
				Description: r.desc,
				Match:       Redact(text[loc[0]:loc[1]]),
			})
		}
	}
	return alerts
}
