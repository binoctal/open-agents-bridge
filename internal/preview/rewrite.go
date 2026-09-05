package preview

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// htmlRefPattern matches src/href attribute values in either quote style.
// RE2 has no backreferences, so the quoted value is captured whole and the
// quote character recovered from its first byte in the replacer.
var htmlRefPattern = regexp.MustCompile(`(src|href)(\s*=\s*)("([^"]*)"|'([^']*)')`)

// RewriteHTMLAbsolutePaths rewrites root-absolute src/href references in
// every .html file under outputDir to directory-relative paths, but ONLY
// when the target exists in the build output — the "in-manifest" test. The
// rewrite runs before BuildManifest, so manifest membership is approximated
// by file existence under outputDir, which is equivalent (design D4).
//
// This fixes the white-screen served under /a/{label}/ hosting, where a
// build with base:'/' emits references that point outside the preview
// prefix. External references survive untouched: protocol-relative (//cdn),
// absolute URLs (https://...), and any /path that is not a file in the
// output. No <base> tag is injected (history-routing footgun); the rewritten
// bytes are what gets hashed into the manifest. Returns the number of
// attribute rewrites applied.
func RewriteHTMLAbsolutePaths(outputDir string) (int, error) {
	total := 0

	err := filepath.Walk(outputDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".html") {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		depth := htmlDepth(outputDir, path)
		prefix := "./"
		if depth > 0 {
			prefix = strings.Repeat("../", depth)
		}

		rewrites := 0
		out := htmlRefPattern.ReplaceAllStringFunc(string(data), func(m string) string {
			sub := htmlRefPattern.FindStringSubmatch(m)
			quoted := sub[3]
			inner := quoted[1 : len(quoted)-1]

			// Only root-absolute references; "//" is protocol-relative
			// (external CDN), not a path into the preview.
			if !strings.HasPrefix(inner, "/") || strings.HasPrefix(inner, "//") {
				return m
			}

			rest := inner[1:]
			cut := len(rest)
			if i := strings.IndexAny(rest, "?#"); i >= 0 {
				cut = i
			}
			pathPart, suffix := rest[:cut], rest[cut:]
			if pathPart == "" {
				// A bare "/" self-link: it points at the root index.
				pathPart = "index.html"
			}

			// In-manifest check: the target must exist in the build output.
			if !fileExists(filepath.Join(outputDir, filepath.FromSlash(pathPart))) {
				return m
			}

			rewrites++
			return sub[1] + sub[2] + string(quoted[0]) + prefix + pathPart + suffix + string(quoted[0])
		})

		if rewrites == 0 {
			return nil
		}
		total += rewrites
		return os.WriteFile(path, []byte(out), info.Mode().Perm())
	})
	if err != nil {
		return total, err
	}
	return total, nil
}

// htmlDepth is how many directories below outputDir the file sits — the
// number of ../ segments a root-absolute reference needs from there.
func htmlDepth(outputDir, path string) int {
	rel, err := filepath.Rel(outputDir, path)
	if err != nil {
		return 0
	}
	return strings.Count(filepath.ToSlash(rel), "/")
}
