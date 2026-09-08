// Package utf8safe turns arbitrary-size read chunks into strings that never
// split a UTF-8 rune (fix-seed-audit-blockers S5: PTY output arrives in 4096
// byte reads, and a 3-byte CJK rune landing across the boundary became two
// U+FFFD replacement chars in the web terminal).
//
// A decoder holds back the trailing partial rune and prepends it to the next
// chunk. Bytes that can never start a valid rune are flushed rather than
// buffered forever, so a hostile/binary stream cannot grow the decoder
// unboundedly.
package utf8safe

import "unicode/utf8"

// Decoder keeps at most one trailing partial rune between chunks. Not safe
// for concurrent use: each read goroutine owns its own.
type Decoder struct {
	pending []byte
}

// Push appends one read chunk and returns every byte that now forms complete
// runes. An empty return means the whole chunk is a partial rune tail.
func (d *Decoder) Push(p []byte) string {
	buf := append(d.pending, p...)
	d.pending = nil

	// Try cut points from the end backwards, at most UTFMax bytes deep: cut
	// == len(buf) is the "everything is valid" fast path (a rune that just
	// completed must be checked even though its last byte is a continuation
	// byte, not a boundary).
	for cut := len(buf); ; cut-- {
		if cut < len(buf) && (len(buf)-cut) > utf8.UTFMax {
			break
		}
		if cut < len(buf) {
			if b := buf[cut]; !(b < 0x80 || b >= 0xC0) {
				continue // mid-rune position, not a cut candidate
			}
		}
		if utf8.Valid(buf[:cut]) {
			if cut < len(buf) {
				d.pending = append(d.pending[:0], buf[cut:]...)
			}
			return string(buf[:cut])
		}
		if cut == 0 {
			break
		}
	}
	// No valid prefix within reach: garbage, not a partial rune. Emit as is
	// rather than buffering indefinitely (the runtime renders invalid bytes,
	// which is the honest representation of what the PTY sent).
	return string(buf)
}

// Len reports how many bytes are held back waiting for a rune to complete
// (diagnostics; always 0 between sessions).
func (d *Decoder) Len() int { return len(d.pending) }
