// Package markdown provides lightweight markdown format boundary detection
// for streaming content splitting. It identifies safe cut points that avoid
// breaking structural elements (code blocks, tables, HTML tags).
package markdown

import "strings"

// FindSafeCut returns the byte offset of the last safe cut point in content.
// A safe cut point is at the end of a line that is not inside a multi-line
// structure (code block, table sequence, or HTML block tag).
// Returns 0 if no safe cut point exists (entire content is inside a structure).
// Returns len(content) if the entire content is safe to send.
func FindSafeCut(content string) int {
	if content == "" {
		return 0
	}

	inCodeBlock := false
	inTable := false
	inHTMLBlock := false
	lastSafeOffset := 0

	lines := splitLines(content)
	offset := 0

	for _, line := range lines {
		lineStart := offset
		lineEnd := offset + len(line)
		trimmed := strings.TrimSpace(line)

		// Code block fence detection (opening or closing)
		if isCodeFence(trimmed) {
			if inCodeBlock {
				// Closing fence — code block ends at this line
				inCodeBlock = false
				// Line end after closing fence is a safe cut point
				lastSafeOffset = lineEnd
			} else {
				// Opening fence — everything before this line is safe
				if lineStart > 0 {
					lastSafeOffset = lineStart
				}
				inCodeBlock = true
			}
			offset = lineEnd
			continue
		}

		if inCodeBlock {
			// Inside code block, not safe to cut
			offset = lineEnd
			continue
		}

		// Table row detection: lines starting and ending with |
		if isTableRow(trimmed) {
			if !inTable {
				// Table starts — cut point before this line
				if lineStart > 0 {
					lastSafeOffset = lineStart
				}
				inTable = true
			}
			// Check if next line would continue the table — we can't
			// know until we see the next line, so don't mark as safe yet
			offset = lineEnd
			continue
		}

		// Non-table line encountered
		if inTable {
			// Table ended at previous line
			inTable = false
			// Re-evaluate current line for other structures
		}

		// HTML block tag detection
		if startsHTMLBlock(trimmed) {
			if !inHTMLBlock && lineStart > 0 {
				lastSafeOffset = lineStart
			}
			inHTMLBlock = true
			// Check for immediate close on same line
			if endsHTMLBlock(trimmed) {
				inHTMLBlock = false
				lastSafeOffset = lineEnd
			}
			offset = lineEnd
			continue
		}

		if inHTMLBlock {
			if endsHTMLBlock(trimmed) {
				inHTMLBlock = false
				lastSafeOffset = lineEnd
			}
			offset = lineEnd
			continue
		}

		// Regular line — end of line is safe
		lastSafeOffset = lineEnd
		offset = lineEnd
	}

	return lastSafeOffset
}

// splitLines splits content into lines preserving line endings.
func splitLines(content string) []string {
	var lines []string
	for {
		idx := strings.IndexByte(content, '\n')
		if idx == -1 {
			if content != "" {
				lines = append(lines, content)
			}
			break
		}
		lines = append(lines, content[:idx+1])
		content = content[idx+1:]
	}
	return lines
}

// isCodeFence returns true if the line is a markdown code fence (``` or ~~~).
func isCodeFence(trimmed string) bool {
	if len(trimmed) < 3 {
		return false
	}
	char := trimmed[0]
	if char != '`' && char != '~' {
		return false
	}
	// Must be at least 3 consecutive same characters
	for i := 1; i < len(trimmed) && i < 10; i++ {
		if trimmed[i] != char {
			// After the fence chars, there can be an info string (e.g. ```python)
			return i >= 3
		}
	}
	return len(trimmed) >= 3
}

// isTableRow returns true if the line looks like a markdown table row.
func isTableRow(trimmed string) bool {
	if len(trimmed) < 3 {
		return false
	}
	// Must start and end with |
	if trimmed[0] != '|' || trimmed[len(trimmed)-1] != '|' {
		return false
	}
	inner := trimmed[1 : len(trimmed)-1]
	// Separator row: |---|---|
	if isTableSeparator(inner) {
		return true
	}
	// Data row: inner content is non-empty and contains only cell content
	// Single-column rows like | 1 | have no inner pipe — still valid
	return len(strings.TrimSpace(inner)) > 0
}

// isTableSeparator checks if the inner content of a table row is a separator.
func isTableSeparator(inner string) bool {
	for _, part := range strings.Split(inner, "|") {
		col := strings.TrimSpace(part)
		if col == "" {
			continue
		}
		for _, ch := range col {
			if ch != '-' && ch != ':' {
				return false
			}
		}
	}
	return true
}

// startsHTMLBlock returns true if the line opens an HTML block element.
func startsHTMLBlock(trimmed string) bool {
	if len(trimmed) < 2 || trimmed[0] != '<' {
		return false
	}
	blockTags := []string{"<div", "<table", "<pre", "<blockquote", "<ul", "<ol", "<dl", "<figure", "<section", "<article", "<aside", "<header", "<footer", "<nav", "<main", "<form", "<fieldset", "<details"}
	lower := strings.ToLower(trimmed)
	for _, tag := range blockTags {
		if strings.HasPrefix(lower, tag) {
			return true
		}
	}
	return false
}

// endsHTMLBlock returns true if the line closes an HTML block element.
func endsHTMLBlock(trimmed string) bool {
	if len(trimmed) < 4 {
		return false
	}
	lower := strings.ToLower(trimmed)
	return strings.Contains(lower, "</div>") ||
		strings.Contains(lower, "</table>") ||
		strings.Contains(lower, "</pre>") ||
		strings.Contains(lower, "</blockquote>") ||
		strings.Contains(lower, "</ul>") ||
		strings.Contains(lower, "</ol>") ||
		strings.Contains(lower, "</dl>") ||
		strings.Contains(lower, "</figure>") ||
		strings.Contains(lower, "</section>") ||
		strings.Contains(lower, "</article>") ||
		strings.Contains(lower, "</aside>") ||
		strings.Contains(lower, "</header>") ||
		strings.Contains(lower, "</footer>") ||
		strings.Contains(lower, "</nav>") ||
		strings.Contains(lower, "</main>") ||
		strings.Contains(lower, "</form>") ||
		strings.Contains(lower, "</fieldset>") ||
		strings.Contains(lower, "</details>")
}
