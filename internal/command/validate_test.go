package command

import (
	"testing"
)

func TestHasShellMetacharacters(t *testing.T) {
	tests := []struct {
		name      string
		cmd       string
		want      bool
		wantName  string
	}{
		{"simple command", "git status", false, ""},
		{"pipe", "ls | grep foo", true, "pipe"},
		{"semicolon", "ls; rm -rf /", true, "semicolon"},
		{"ampersand", "sleep 10 &", true, "ampersand"},
		{"command substitution", "echo $(cat /etc/passwd)", true, "command_substitution"},
		{"backtick", "echo `cat /etc/passwd`", true, "backtick"},
		{"redirect write", "echo foo > bar", true, "redirect_write"},
		{"redirect append", "echo foo >> bar", true, "redirect_append"},
		{"redirect read", "sort < input.txt", true, "redirect_read"},
		{"quoted semicolon ignored", `echo "hello; world"`, false, ""},
		{"quoted pipe ignored", `echo "a | b"`, false, ""},
		{"single quoted pipe ignored", `echo 'a | b'`, false, ""},
		{"mixed quotes", `echo "hello" | grep h`, true, "pipe"},
		{"escaped pipe", `echo hello \| grep`, false, ""}, // backslash-escaped pipes are literal
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, gotName := HasShellMetacharacters(tt.cmd)
			if got != tt.want {
				t.Errorf("HasShellMetacharacters(%q) = %v, want %v", tt.cmd, got, tt.want)
			}
			if got && gotName != tt.wantName {
				t.Errorf("metacharacter name = %q, want %q", gotName, tt.wantName)
			}
		})
	}
}

func TestExtractBaseCommand(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want string
	}{
		{"simple", "git status", "git"},
		{"with args", "git commit -m \"fix bug\"", "git"},
		{"absolute path", "/usr/bin/git status", "git"},
		{"relative path", "./bin/node server.js", "node"},
		{"with flags", "npm install --save-dev", "npm"},
		{"leading spaces", "  ls -la", "ls"},
		{"empty", "", ""},
		{"spaces only", "   ", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractBaseCommand(tt.cmd)
			if got != tt.want {
				t.Errorf("ExtractBaseCommand(%q) = %q, want %q", tt.cmd, got, tt.want)
			}
		})
	}
}

func TestIsAllowed(t *testing.T) {
	tests := []struct {
		cmd  string
		want bool
	}{
		{"git", true},
		{"npm", true},
		{"node", true},
		{"go", true},
		{"rm", false},
		{"sudo", false},
		{"chmod", false},
		{"bash", false},
		{"sh", false},
		{"python3", true},
		{"curl", true},
		{"docker", true},
	}

	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) {
			if got := IsAllowed(tt.cmd); got != tt.want {
				t.Errorf("IsAllowed(%q) = %v, want %v", tt.cmd, got, tt.want)
			}
		})
	}
}

func TestValidateCommand(t *testing.T) {
	tests := []struct {
		name    string
		cmd     string
		wantErr bool
	}{
		{"allowed command", "git status", false},
		{"allowed with args", "git commit -m \"fix\"", false},
		{"dangerous command", "rm -rf /tmp/test", true},
		{"pipe attack", "ls | grep foo", true},
		{"semicolon chaining", "ls; cat /etc/passwd", true},
		{"command substitution", "echo $(whoami)", true},
		{"not in whitelist", "sudo apt install foo", true},
		{"empty command", "", true},
		{"allowed node", "node server.js", false},
		{"allowed npm", "npm install", false},
		{"allowed python", "python3 main.py", false},
		{"allowed docker", "docker ps", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateCommand(tt.cmd)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateCommand(%q) error = %v, wantErr %v", tt.cmd, err, tt.wantErr)
			}
		})
	}
}
