package preview

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/binoctal/open-agents-bridge/internal/logger"
)

// Artifact kinds (add-deployment-previews D3). KindStatic and KindRuntime
// mirror the platform's preview_deployments.kind values; the empty string is
// the cascade's rule-5 fall-through: keep the status quo, build no preview.
const (
	KindStatic  = "static"
	KindRuntime = "runtime"

	// TaskIDMerge is the complete-body sentinel for the merge-final build:
	// that artifact is not any single task's product but the merged tree's
	// final state, and the platform renders its snapshot as 合并终态.
	TaskIDMerge = "merge"
)

// Framework config candidates, in no particular order — only presence and
// one exclusion pattern each matter to the cascade.
var (
	nextConfigCandidates = []string{"next.config.js", "next.config.mjs", "next.config.ts", "next.config.cjs"}
	nuxtConfigCandidates = []string{"nuxt.config.ts", "nuxt.config.js", "nuxt.config.mjs"}

	outputExportPattern = regexp.MustCompile(`output\s*:\s*['"]export['"]`)
	ssrFalsePattern     = regexp.MustCompile(`ssr\s*:\s*false`)
)

// configSignal reports whether any of candidates exists in repoRoot and, if
// so, whether its content matches the exclusion pattern (next: output
// 'export'; nuxt: ssr false). An unreadable config counts as present without
// exclusion — the cascade then errs toward runtime, which in stage 1 only
// changes a banner, never the static pipeline.
func configSignal(repoRoot string, candidates []string, exclude *regexp.Regexp) (exists, excluded bool) {
	for _, name := range candidates {
		path := filepath.Join(repoRoot, name)
		if !fileExists(path) {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return true, false
		}
		return true, exclude.Match(data)
	}
	return false, false
}

// buildScriptRunsGenerate reports whether the build script invokes a
// generator (nuxt generate et al.), which produces a static dist rather than
// a server bundle.
func buildScriptRunsGenerate(repoRoot string) bool {
	data, err := os.ReadFile(filepath.Join(repoRoot, "package.json"))
	if err != nil {
		return false
	}
	var pkg packageJSON
	if err := json.Unmarshal(data, &pkg); err != nil {
		return false
	}
	return strings.Contains(pkg.Scripts["build"], "generate")
}

// serverBundleExists reports whether a known server-bundle directory from a
// framework build is present (Next .next/, Nuxt/Nitro .output/).
func serverBundleExists(repoRoot string) bool {
	for _, dir := range []string{".next", ".output"} {
		if fi, err := os.Stat(filepath.Join(repoRoot, dir)); err == nil && fi.IsDir() {
			return true
		}
	}
	return false
}

// DetectArtifactKind runs the D3 static cascade against repoRoot. Detection
// deliberately reads framework CONFIG, not just build output: an SSR shell
// can emit an index.html that would otherwise misclassify it as static
// (design risk row). First hit wins:
//
//  1. root index.html and no framework server signal  -> static
//  2. next.config.* without output:'export'           -> runtime
//  3. nuxt.config.* neither ssr:false nor generate    -> runtime
//  4. no root index.html, no resolvable static output,
//     but a server bundle dir present                 -> runtime
//  5. anything else                                   -> "" (no preview)
//
// Static export paths (next export, nuxt generate, Vite, CRA) all leave an
// index.html under dist|build|out and land static via ResolveOutputDir.
func DetectArtifactKind(repoRoot string) string {
	nextCfg, nextExport := configSignal(repoRoot, nextConfigCandidates, outputExportPattern)
	nuxtCfg, nuxtSSRFalse := configSignal(repoRoot, nuxtConfigCandidates, ssrFalsePattern)

	rootIndex := fileExists(filepath.Join(repoRoot, "index.html"))

	// R1: a plain static site — root index.html and no SSR framework config.
	if rootIndex && !nextCfg && !nuxtCfg {
		return KindStatic
	}
	// R2: Next without output:'export' builds a server bundle, whatever
	// index.html it also emits — the shell outranks the artifact.
	if nextCfg && !nextExport {
		return KindRuntime
	}
	// R3: Nuxt with SSR on (or not explicitly off) and not running generate.
	if nuxtCfg && !nuxtSSRFalse && !buildScriptRunsGenerate(repoRoot) {
		return KindRuntime
	}
	// Static build output resolvable — Vite/CRA/next-export/nuxt-generate.
	if _, ok := ResolveOutputDir(repoRoot); ok {
		return KindStatic
	}
	// R4: server-only build output (Next non-export never resolves a static
	// dir; .next/ is the tell).
	if !rootIndex && serverBundleExists(repoRoot) {
		return KindRuntime
	}
	return ""
}

// DetectAndReport classifies repoRoot and reports the mission-level artifact
// kind to the platform. It runs after the build, preview or not: a Next
// non-export build produces no preview row at all, and the runtime
// classification is exactly what must still reach the mission row so
// PreviewCard can render the deploy-tier teaser. A report failure is logged
// and swallowed — same best-effort contract as every preview path. Returns
// the detected kind ("" = rule 5, nothing reported).
func DetectAndReport(client Uploader, jobID, repoRoot string, logf Logf) string {
	if logf == nil {
		logf = noopLogf
	}
	kind := DetectArtifactKind(repoRoot)
	if kind == "" {
		return ""
	}
	if err := client.ReportArtifactKind(jobID, kind); err != nil {
		logf("[%s] preview: artifact-kind report failed for mission %s: %v", logger.ModPreview, jobID, err)
	}
	return kind
}
