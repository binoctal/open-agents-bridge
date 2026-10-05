package fingerprint

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

func TestComputeIsStableAndServerAcceptable(t *testing.T) {
	a, b := Compute(), Compute()
	if a != b {
		t.Fatalf("fingerprint not stable: %q vs %q", a, b)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`).MatchString(a) {
		t.Fatalf("fingerprint %q would be rejected by the server", a)
	}
}

func TestHashDiffersPerInputAndHidesRawValues(t *testing.T) {
	h1 := hash("host-a", "linux", "amd64", "id1")
	if h1 == hash("host-b", "linux", "amd64", "id1") || h1 == hash("host-a", "linux", "amd64", "id2") {
		t.Fatal("different machines must hash differently")
	}
	if regexp.MustCompile(`host-a|id1`).MatchString(h1) {
		t.Fatal("raw input leaked into the fingerprint")
	}
}

func TestReadMachineIDSkipsMissingAndEmpty(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	good := filepath.Join(dir, "good")
	_ = os.WriteFile(empty, []byte("  \n"), 0o600)
	_ = os.WriteFile(good, []byte("abc123\n"), 0o600)
	if got := readMachineID([]string{filepath.Join(dir, "missing"), empty, good}); got != "abc123" {
		t.Fatalf("got %q", got)
	}
	if got := readMachineID([]string{filepath.Join(dir, "missing")}); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}
