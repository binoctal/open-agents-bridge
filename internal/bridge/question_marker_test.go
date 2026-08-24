package bridge

import (
	"strings"
	"testing"
)

// Known-issue #9: the task prompt itself contains "[QUESTION]" mid-sentence
// (buildTaskPrompt's instruction line), and the PTY fallback path echoes that
// prompt back as CLI output. Marker detection must require the marker to OPEN
// the line — the exact contract the prompt states — or every PTY task fires a
// spurious workflow:task_question on harness-generated text. Chunks that begin
// mid-line (PTY read boundaries are arbitrary) must not count their first
// fragment as a line start.
func TestExtractQuestion(t *testing.T) {
	cases := []struct {
		name                string
		content             string
		firstLineIsBoundary bool
		want                string
		found               bool
	}{
		{
			name:                "genuine agent question",
			content:             "Working...\n[QUESTION] Should I use JWT or session-based auth?",
			firstLineIsBoundary: true,
			want:                "Should I use JWT or session-based auth?",
			found:               true,
		},
		{
			name:                "leading whitespace and CR tolerated",
			content:             "  \r\n  [QUESTION] Proceed with migration?\r",
			firstLineIsBoundary: true,
			want:                "Proceed with migration?",
			found:               true,
		},
		{
			name:                "genuine marker as the very first line of a chunk",
			content:             "[QUESTION] Proceed with migration?",
			firstLineIsBoundary: true,
			want:                "Proceed with migration?",
			found:               true,
		},
		{
			name:                "echoed prompt instruction does not match",
			content:             buildTaskPrompt("Fix the login bug", "Locate and fix it", ""),
			firstLineIsBoundary: true,
			want:                "",
			found:               false,
		},
		{
			name:                "mid-line mention does not match",
			content:             "The docs say to use [QUESTION] markers when stuck.",
			firstLineIsBoundary:  true,
			want:                 "",
			found:                false,
		},
		{
			// Dogfood 2026-08-22: a PTY read split the echoed instruction
			// exactly before the Example marker — the chunk then STARTS with
			// "[QUESTION]" but is not at a line start.
			name:                "chunk beginning mid-line at the marker does not match",
			content:             "[QUESTION] Should I use JWT or session-based authentication?",
			firstLineIsBoundary:  false,
			want:                 "",
			found:                false,
		},
		{
			name:                "later lines are true line starts even when the chunk begins mid-line",
			content:             "Should I use JWT or session-based authentication?\n[QUESTION] Actually, wait?",
			firstLineIsBoundary:  false,
			want:                 "Actually, wait?",
			found:                true,
		},
		{
			name:                "marker with empty question is ignored",
			content:             "[QUESTION]",
			firstLineIsBoundary:  true,
			want:                 "",
			found:                false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, found := extractQuestion(tc.content, tc.firstLineIsBoundary)
			if found != tc.found {
				t.Fatalf("found = %v, want %v (content: %q)", found, tc.found, tc.content)
			}
			if got != tc.want {
				t.Fatalf("question = %q, want %q", got, tc.want)
			}
		})
	}
}

// Dogfood 2026-08-24 (run 13): PTY terminals hard-wrap echoed text at the
// column width, and a wrap can fall immediately before ANY "[QUESTION]"
// occurrence inside the echoed instruction — live logs show both the example
// sentence ("Should I use JW", truncated mid-word at the wrap) and the
// instruction sentence itself ("followed by your question. Example: …")
// landing at genuine "\n" line starts. Line-prefix detection cannot
// distinguish that from a real question, so the prompt must not contain the
// literal marker at all: buildTaskPrompt describes the format without
// spelling the marker out.
func TestBuildTaskPromptOmitsLiteralQuestionMarker(t *testing.T) {
	for _, tc := range []struct {
		name, title, desc, context string
	}{
		{"minimal", "Fix the login bug", "Locate and fix the authentication failure", ""},
		{"with upstream context", "Fix the login bug", "Locate and fix the authentication failure", "upstream task output"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if strings.Contains(buildTaskPrompt(tc.title, tc.desc, tc.context), "[QUESTION]") {
				t.Fatal("task prompt must not contain the literal [QUESTION] marker: " +
					"PTY echo hard-wrap can place it at a true line start and fire a spurious task_question")
			}
		})
	}
}

// wrapAtWidth simulates a terminal hard-wrapping echoed text at the given
// column count — the failure mode live-observed in dogfood (extraction fired
// on "Should I use JW", the example sentence split mid-word).
func wrapAtWidth(s string, width int) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && i%width == 0 {
			b.WriteByte('\n')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func TestWrappedPromptEchoNeverFiresQuestionMarker(t *testing.T) {
	prompt := buildTaskPrompt("Fix the login bug", "Locate and fix the authentication failure", "upstream task output")
	for w := 10; w <= 160; w++ {
		if _, found := extractQuestion(wrapAtWidth(prompt, w), true); found {
			t.Fatalf("width %d: a hard-wrapped echo of the task prompt must not fire question detection", w)
		}
	}
}

// chunkStartsAtLineBoundary returns the PREVIOUS chunk's tail state (was it
// newline-terminated); the first chunk of a session is line-aligned by
// assumption. State is per session.
func TestChunkStartsAtLineBoundary(t *testing.T) {
	b := &Bridge{lineBoundary: make(map[string]bool)}

	if !b.chunkStartsAtLineBoundary("s1", "first chunk without newline") {
		t.Fatal("first chunk must be treated as line-aligned")
	}
	if b.chunkStartsAtLineBoundary("s1", "continues the same line") {
		t.Fatal("chunk after a newline-less chunk is mid-line")
	}
	if b.chunkStartsAtLineBoundary("s1", "still no newline") {
		t.Fatal("still mid-line")
	}
	// This chunk itself still begins mid-line (previous had no newline) but
	// leaves the stream at a boundary — the NEXT chunk is line-aligned.
	if b.chunkStartsAtLineBoundary("s1", "line ends here\n") {
		t.Fatal("chunk after a newline-less chunk is mid-line even if it ends with one")
	}
	if !b.chunkStartsAtLineBoundary("s1", "aligned start") {
		t.Fatal("chunk after a newline-terminated chunk must be line-aligned")
	}
	if b.chunkStartsAtLineBoundary("s1", "mid-line again") {
		t.Fatal("chunk after a newline-less chunk is mid-line")
	}
	if !b.chunkStartsAtLineBoundary("s2", "independent session starts aligned") {
		t.Fatal("session state must be independent")
	}
}
