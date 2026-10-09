package bridge

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestAddTelemetryParamsNames(t *testing.T) {
	q := url.Values{}
	addTelemetryParams(q, "0.13.3", "ab")
	if q.Get("v") != "0.13.3" || q.Get("sha") != "ab" {
		t.Fatalf("unexpected params: %v", q)
	}
}

func TestAddTelemetryParamsOmitsEmptySHA(t *testing.T) {
	q := url.Values{}
	addTelemetryParams(q, "dev", "")
	if _, ok := q["sha"]; ok {
		t.Fatal("sha must be omitted when the digest is unavailable")
	}
	if q.Get("v") != "dev" {
		t.Fatal("version must still be reported")
	}
}

func TestFileSHA256(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	// sha256("abc")
	want := "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := fileSHA256(p); got != want {
		t.Fatalf("got %s", got)
	}
	if got := fileSHA256(filepath.Join(t.TempDir(), "missing")); got != "" {
		t.Fatalf("missing file must yield empty digest, got %q", got)
	}
}

func TestSelfSHA256Format(t *testing.T) {
	got := selfSHA256()
	if got == "" {
		t.Skip("executable unreadable in this environment")
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(got) {
		t.Fatalf("not 64 lowercase hex: %q", got)
	}
}
