package bridge

import "testing"

// Known-issue #9: the task prompt itself contains "[QUESTION]" mid-sentence
// (buildTaskPrompt's instruction line), and the PTY fallback path echoes that
// prompt back as CLI output. Marker detection must require the marker to OPEN
// the line — the exact contract the prompt states — or every PTY task fires a
// spurious workflow:task_question on harness-generated text.
func TestExtractQuestion(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
		found   bool
	}{
		{
			name:    "genuine agent question",
			content: "Working...\n[QUESTION] Should I use JWT or session-based auth?",
			want:    "Should I use JWT or session-based auth?",
			found:   true,
		},
		{
			name:    "leading whitespace and CR tolerated",
			content: "  \r\n  [QUESTION] Proceed with migration?\r",
			want:    "Proceed with migration?",
			found:   true,
		},
		{
			name: "echoed prompt instruction does not match",
			content: "Fix the login bug\n\n--- Instruction ---\n" +
				"If you need to ask the user a question during execution, output a line starting with [QUESTION] followed by your question. Example: [QUESTION] Should I use JWT or session-based authentication?",
			want:  "",
			found: false,
		},
		{
			name:    "mid-line mention does not match",
			content: "The docs say to use [QUESTION] markers when stuck.",
			want:    "",
			found:   false,
		},
		{
			name:    "marker with empty question is ignored",
			content: "[QUESTION]",
			want:    "",
			found:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, found := extractQuestion(tc.content)
			if found != tc.found {
				t.Fatalf("found = %v, want %v (content: %q)", found, tc.found, tc.content)
			}
			if got != tc.want {
				t.Fatalf("question = %q, want %q", got, tc.want)
			}
		})
	}
}
