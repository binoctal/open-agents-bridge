package updater

import (
	"regexp"
	"testing"
)

// The build-time default must not look like a release version.
//
// It used to be "0.6.0". Nothing updated it when 0.6.1 and 0.6.2 shipped,
// because release builds override it via ldflags and never exercise it — the
// only builds that read it are `go install` and local `go build`, which no
// release process looks at. So the string sat there going stale, and a
// go-installed binary claimed to be a specific older release rather than an
// unversioned build.
func TestVersionDefaultIsNotAReleaseNumber(t *testing.T) {
	if regexp.MustCompile(`^v?\d+\.\d+`).MatchString(version) {
		t.Fatalf("default version %q looks like a release; use a non-version sentinel so it cannot go stale", version)
	}
}

// A dev build must sort below any real release, so CheckUpdate offers the
// upgrade instead of concluding the dev build is current.
func TestDevVersionSortsBelowReleases(t *testing.T) {
	for _, release := range []string{"0.6.2", "v1.0.0", "0.0.1"} {
		if got := compareSemver(version, release); got != -1 {
			t.Errorf("compareSemver(%q, %q) = %d, want -1", version, release, got)
		}
	}
}
