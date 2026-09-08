package preview_test

import (
	"reflect"
	"regexp"
	"testing"

	"github.com/binoctal/open-agents-bridge/internal/deploysource"
	"github.com/binoctal/open-agents-bridge/internal/preview"
)

// The S5 no-build fallback duplicated the deploy source packer's exclusion
// lists into preview (deploysource imports preview, so preview cannot import
// them back). This guard keeps the two copies from drifting apart silently:
// preview's fallback manifest must strip exactly what the source packer
// strips.
func TestStaticTreeExclusionsMirrorDeploysource(t *testing.T) {
	if !reflect.DeepEqual(preview.StaticTreeExcludedDirs, deploysource.ExcludedDirs) {
		t.Errorf("excluded dirs drifted:\npreview:     %v\ndeploysource: %v",
			preview.StaticTreeExcludedDirs, deploysource.ExcludedDirs)
	}

	toPatterns := func(rs []*regexp.Regexp) []string {
		out := make([]string, len(rs))
		for i, re := range rs {
			out[i] = re.String()
		}
		return out
	}
	got := toPatterns(preview.StaticTreeSensitivePatterns)
	want := toPatterns(deploysource.SensitivePatterns)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sensitive patterns drifted:\npreview:     %v\ndeploysource: %v", got, want)
	}
}
