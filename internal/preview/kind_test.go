package preview

import (
	"fmt"
	"testing"
)

// writePkgJSON writes a minimal package.json; scripts controls the build
// script body ("" omits the script entirely).
func writePkgJSON(t *testing.T, dir, build string) {
	t.Helper()
	scripts := "null"
	if build != "" {
		scripts = fmt.Sprintf(`{"build":%q}`, build)
	}
	writeFile(t, dir, "package.json", fmt.Sprintf(`{"name":"x","scripts":%s}`, scripts))
}

// The D3 cascade (add-deployment-previews task 2.3): five rules, first hit
// wins, each case exercised against a real directory tree.
func TestDetectArtifactKind_Cascade(t *testing.T) {
	cases := []struct {
		name string
		prep func(t *testing.T, dir string)
		want string
	}{
		{
			// R1: root index.html, no framework config.
			name: "plain static site -> static",
			prep: func(t *testing.T, dir string) {
				writeFile(t, dir, "index.html", "<html></html>")
			},
			want: KindStatic,
		},
		{
			// R2 beats R1: a Next SSR shell can emit a root index.html that
			// must NOT misclassify the site as static.
			name: "next.config without output:'export' -> runtime even with root index.html",
			prep: func(t *testing.T, dir string) {
				writeFile(t, dir, "index.html", "<html></html>")
				writeFile(t, dir, "next.config.js", "module.exports = {}")
			},
			want: KindRuntime,
		},
		{
			// R2 excluded: output:'export' makes Next static, and the
			// exported out/ tree resolves.
			name: "next.config with output:'export' + out/index.html -> static",
			prep: func(t *testing.T, dir string) {
				writeFile(t, dir, "next.config.mjs", "export default { output: 'export' }")
				writeFile(t, dir, "out/index.html", "<html></html>")
			},
			want: KindStatic,
		},
		{
			// R3: Nuxt with SSR on builds a Nitro server bundle.
			name: "nuxt.config ssr on -> runtime",
			prep: func(t *testing.T, dir string) {
				writeFile(t, dir, "nuxt.config.ts", "export default defineNuxtConfig({})")
				writePkgJSON(t, dir, "nuxt build")
			},
			want: KindRuntime,
		},
		{
			// R3 excluded by ssr:false: static dist resolvable.
			name: "nuxt.config ssr:false + dist/index.html -> static",
			prep: func(t *testing.T, dir string) {
				writeFile(t, dir, "nuxt.config.ts", "export default defineNuxtConfig({ ssr: false })")
				writeFile(t, dir, "dist/index.html", "<html></html>")
			},
			want: KindStatic,
		},
		{
			// R3 excluded by a generate script even with SSR on.
			name: "nuxt generate script + dist/index.html -> static",
			prep: func(t *testing.T, dir string) {
				writeFile(t, dir, "nuxt.config.ts", "export default defineNuxtConfig({})")
				writePkgJSON(t, dir, "nuxt generate")
				writeFile(t, dir, "dist/index.html", "<html></html>")
			},
			want: KindStatic,
		},
		{
			// R4: no static output anywhere, but a Next server bundle exists.
			name: "next.config + .next/ server bundle, no static output -> runtime",
			prep: func(t *testing.T, dir string) {
				writeFile(t, dir, "next.config.js", "module.exports = {}")
				writeFile(t, dir, ".next/server/app/page.js", "/* server */")
			},
			want: KindRuntime,
		},
		{
			// R5: nothing recognizable — build nothing, keep the status quo.
			name: "bare repo with no signals -> empty (no preview)",
			prep: func(t *testing.T, dir string) {
				writeFile(t, dir, "README.md", "hello")
			},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.prep(t, dir)
			if got := DetectArtifactKind(dir); got != tc.want {
				t.Fatalf("DetectArtifactKind = %q, want %q", got, tc.want)
			}
		})
	}
}

// A runtime Next build produces no preview row; DetectAndReport is the only
// thing that still tells the platform about it.
func TestDetectAndReport_ReportsDetectedKind(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "index.html", "<html></html>")
	writeFile(t, dir, "next.config.js", "module.exports = {}") // runtime

	fake := &fakeUploader{}
	var logs []string
	logf := func(f string, a ...interface{}) { logs = append(logs, fmt.Sprintf(f, a...)) }

	kind := DetectAndReport(fake, "mission-1", dir, logf)
	if kind != KindRuntime {
		t.Fatalf("DetectAndReport = %q, want %q", kind, KindRuntime)
	}
	if len(fake.reportedKinds) != 1 || fake.reportedKinds[0] != KindRuntime {
		t.Fatalf("expected one runtime report, got %v", fake.reportedKinds)
	}
	if len(logs) != 0 {
		t.Fatalf("successful report must not log, got %v", logs)
	}
}

// Rule 5 must not report at all: "" means "keep the status quo", and an
// empty upsert on the platform would clobber a real detection.
func TestDetectAndReport_EmptyKindReportsNothing(t *testing.T) {
	dir := t.TempDir() // bare

	fake := &fakeUploader{}
	if got := DetectAndReport(fake, "mission-1", dir, nil); got != "" {
		t.Fatalf("expected empty kind, got %q", got)
	}
	if len(fake.reportedKinds) != 0 {
		t.Fatalf("empty kind must not be reported, got %v", fake.reportedKinds)
	}
}

// A failed report is logged and swallowed — the same best-effort contract
// as every preview path — and the detected kind is still returned so it can
// ride the complete call.
func TestDetectAndReport_ReportFailureIsBestEffort(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "index.html", "<html></html>")

	fake := &fakeUploader{kindErr: fmt.Errorf("503")}
	var logs []string
	logf := func(f string, a ...interface{}) { logs = append(logs, fmt.Sprintf(f, a...)) }

	kind := DetectAndReport(fake, "mission-1", dir, logf)
	if kind != KindStatic {
		t.Fatalf("kind = %q, want static even when the report fails", kind)
	}
	if len(fake.reportedKinds) != 1 {
		t.Fatalf("expected exactly one report attempt, got %v", fake.reportedKinds)
	}
	if len(logs) != 1 {
		t.Fatalf("expected exactly one failure log, got %v", logs)
	}
}
