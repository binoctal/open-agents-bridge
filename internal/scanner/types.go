package scanner

// AlertLevel indicates severity
type AlertLevel string

const (
	AlertWarning  AlertLevel = "warning"
	AlertCritical AlertLevel = "critical"
)

// AlertCategory groups alert types
type AlertCategory string

const (
	CategorySensitiveData AlertCategory = "sensitive_data"
	CategoryDangerousCmd  AlertCategory = "dangerous_command"
	CategoryPathAccess    AlertCategory = "path_access"
	CategoryCodeSecurity  AlertCategory = "code_security"
	CategoryPII           AlertCategory = "pii"
)

// Direction indicates scan direction
type Direction int

const (
	DirOutput Direction = iota // CLI → User
	DirInput                   // User → CLI
)

// Alert represents a detected security issue
type Alert struct {
	Category    AlertCategory `json:"category"`
	Level       AlertLevel    `json:"level"`
	RuleID      string        `json:"ruleId"`
	Title       string        `json:"title"`
	Description string        `json:"description"`
	Match       string        `json:"match"`
}

// ScannerPlugin is the interface all scanners implement
type ScannerPlugin interface {
	Name() string
	Scan(text string, dir Direction) []Alert
}

// Redact masks sensitive content for display
func Redact(s string) string {
	if len(s) <= 12 {
		n := len(s)
		if n > 4 {
			n = 4
		}
		return s[:n] + "****"
	}
	return s[:8] + "****" + s[len(s)-4:]
}
