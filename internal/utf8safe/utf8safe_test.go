package utf8safe

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// S5 (fix-seed-audit-blockers): the live defect was a CJK rune split across
// a 4096-byte PTY read rendering as two U+FFFD. Pin the boundary shapes.
func TestDecoderNeverSplitsARune(t *testing.T) {
	cases := []struct {
		name   string
		chunks [][]byte
		want   string
	}{
		{
			name:   "3-byte rune split at the chunk boundary",
			chunks: [][]byte{[]byte("a" + "中"[:1]), []byte("中"[1:] + "b")},
			want:   "a中b",
		},
		{
			name:   "4-byte emoji split one byte at a time",
			chunks: [][]byte{{0xF0}, {0x9F}, {0x98}, {0x80}},
			want:   "😀",
		},
		{
			name:   "ascii passes through untouched",
			chunks: [][]byte{[]byte("hello"), []byte(" world")},
			want:   "hello world",
		},
		{
			name:   "whole rune in one chunk",
			chunks: [][]byte{[]byte("中文输出")},
			want:   "中文输出",
		},
		{
			name: "exact 4096 boundary like the PTY read",
			chunks: func() [][]byte {
				one := strings.Repeat("中", 1) // 3 bytes
				fill := strings.Repeat("A", 4095)
				return [][]byte{
					[]byte(fill + one[:1]), // byte 4096 lands mid-rune
					[]byte(one[1:] + "tail"),
				}
			}(),
			want: strings.Repeat("A", 4095) + "中" + "tail",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var d Decoder
			var got strings.Builder
			for _, c := range tc.chunks {
				got.WriteString(d.Push(c))
			}
			if got.String() != tc.want {
				t.Fatalf("got %q, want %q", got.String(), tc.want)
			}
		})
	}
}

// Pending bytes must not accumulate on garbage: a stream of never-valid
// start bytes flushes instead of buffering forever.
func TestDecoderFlushesGarbage(t *testing.T) {
	var d Decoder
	garbage := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	out := d.Push(garbage)
	if out == "" {
		t.Fatal("never-valid bytes must flush, not buffer indefinitely")
	}
	if d.Len() != 0 {
		t.Fatalf("no residue expected after garbage flush, got %d bytes", d.Len())
	}
}

// No U+FFFD may ever appear in reassembled output for valid UTF-8 input.
func TestDecoderNoReplacementChars(t *testing.T) {
	payload := strings.Repeat("终端输出测试🚀", 500)
	var d Decoder
	var got strings.Builder
	for i := 0; i < len(payload); i += 37 { // deliberately misaligned chunks
		end := min(i+37, len(payload))
		got.WriteString(d.Push([]byte(payload[i:end])))
	}
	if got.String() != payload {
		t.Fatal("reassembled output must equal the input")
	}
	if !utf8.ValidString(got.String()) {
		t.Fatal("output must be valid UTF-8")
	}
	if strings.ContainsRune(got.String(), 0xFFFD) {
		t.Fatal("replacement char must never appear for valid input")
	}
}
