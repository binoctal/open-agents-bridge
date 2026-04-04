package scanner

import "regexp"

// PIIScanner detects personally identifiable information.
// Source: Microsoft Presidio predefined recognizers
type PIIScanner struct{}

func (p *PIIScanner) Name() string { return "pii" }

var piiRules = []struct {
	id, title, desc, pattern string
	level                    AlertLevel
}{
	// Email
	{"pii_email", "Email Address", "Email address detected in output.", `[a-zA-Z0-9._%+\-]{2,}@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`, AlertWarning},
	// IPv4
	{"pii_ipv4", "IP Address", "IPv4 address detected.", `\b(?:(?:25[0-5]|2[0-4]\d|[01]?\d\d?)\.){3}(?:25[0-5]|2[0-4]\d|[01]?\d\d?)\b`, AlertWarning},
	// Phone numbers (international)
	{"pii_phone_intl", "Phone Number", "International phone number detected.", `\+[1-9]\d{6,14}`, AlertWarning},
	// Phone numbers (China)
	{"pii_phone_cn", "Phone Number (CN)", "Chinese mobile number detected.", `\b1[3-9]\d{9}\b`, AlertWarning},
	// China ID card
	{"pii_id_cn", "ID Card (CN)", "Chinese national ID number detected.", `\b[1-9]\d{5}(?:19|20)\d{2}(?:0[1-9]|1[0-2])(?:0[1-9]|[12]\d|3[01])\d{3}[\dXx]\b`, AlertCritical},
	// Credit card (Visa, Mastercard, Amex patterns)
	{"pii_credit_card", "Credit Card", "Credit card number detected.", `\b(?:4\d{3}|5[1-5]\d{2}|3[47]\d{2}|6(?:011|5\d{2}))[- ]?\d{4}[- ]?\d{4}[- ]?\d{3,4}\b`, AlertCritical},
	// IBAN
	{"pii_iban", "IBAN", "International bank account number detected.", `\b[A-Z]{2}\d{2}[A-Z0-9]{4}\d{7}(?:[A-Z0-9]?\d{0,16})\b`, AlertWarning},
	// SSN (US)
	{"pii_ssn", "SSN (US)", "US Social Security Number detected.", `\b\d{3}-\d{2}-\d{4}\b`, AlertCritical},
	// Passport (generic)
	{"pii_passport", "Passport Number", "Possible passport number detected.", `(?i)passport\s*(?:no|number|#|:)\s*[A-Z0-9]{6,9}`, AlertWarning},
}

var compiledPIIRules []compiledRule

func init() {
	for _, r := range piiRules {
		compiledPIIRules = append(compiledPIIRules, compiledRule{
			id: r.id, title: r.title, desc: r.desc, level: r.level,
			pattern: regexp.MustCompile(r.pattern),
		})
	}
}

func (p *PIIScanner) Scan(text string, dir Direction) []Alert {
	var alerts []Alert
	for _, r := range compiledPIIRules {
		if loc := r.pattern.FindStringIndex(text); loc != nil {
			alerts = append(alerts, Alert{
				Category:    CategoryPII,
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
