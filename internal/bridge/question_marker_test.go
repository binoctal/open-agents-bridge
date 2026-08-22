package bridge

import "testing"

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
			name: "echoed prompt instruction does not match",
			content: "Fix the login bug\n\n--- Instruction ---\n" +
				"If you need to ask the user a question during execution, output a line starting with [QUESTION] followed by your question. Example: [QUESTION] Should I use JWT or session-based authentication?",
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
