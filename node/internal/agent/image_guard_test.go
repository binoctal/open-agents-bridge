package agent

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Static supply-chain guard over the runner image sources (add-cloud-executor
// 4.3, spec sandbox-security-baseline: 镜像供应链 / 凭据纪律). The image files
// live in the repo, so a repo-side test can pin the invariants that must hold
// BEFORE any docker build runs: digest-pinned base, no floating tags, exact
// CLI versions, and zero control-plane credential material. scan-secrets.sh
// covers the BUILT image (config env + layer history); this test covers the
// sources so a bad change fails in CI, not at node-build time.

const imageDir = "../../images/runner"

// Control-plane credential names; TASK_TOKEN is deliberately absent -- the
// per-task token is the one credential the sandbox legitimately holds, and it
// only arrives as container env at docker run.
var secretPattern = regexp.MustCompile(
	`COOLIFY_API_TOKEN|INTERNAL_SECRET|JWT_SECRET|KEY_ENCRYPTION_KEY|RESEND_API_KEY|CLOUDFLARE_API_TOKEN|ANTHROPIC_API_KEY|OPENAI_API_KEY|API_TOKEN_PROD|GITHUB_TOKEN`)

var keyMaterialPattern = regexp.MustCompile(`sk-[A-Za-z0-9]{16,}|ghp_[A-Za-z0-9]{20,}`)

func readImageFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(imageDir, name))
	if err != nil {
		t.Fatalf("runner image file %s unreadable: %v (build it under hosting-agent/images/runner/)", name, err)
	}
	return string(b)
}

// Dockerfile lines with the comment stripped: a comment SAYING "latest is
// forbidden" must not trip the guard that enforces exactly that.
func strippedComments(s string) string {
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// The Dockerfile must build from the digest-pinned base passed via build
// args -- a bare tag FROM (which could move under the same name) is exactly
// the supply-chain hole the pin exists to close. And no floating tags.
func TestRunnerDockerfilePinsBase(t *testing.T) {
	df := strippedComments(readImageFile(t, "Dockerfile"))
	from := regexp.MustCompile(`(?m)^FROM\s+(\S+)`).FindAllStringSubmatch(df, -1)
	if len(from) != 1 {
		t.Fatalf("want exactly one FROM stage, got %d", len(from))
	}
	ref := from[0][1]
	if !strings.HasPrefix(ref, "${NODE_IMAGE}") {
		t.Errorf("FROM must use the ${NODE_IMAGE} build arg, got %q", ref)
	}
	// The digest must come from the lock arg, not be hardcoded (build.sh
	// controls it); either way the ARG wiring must exist.
	if !strings.Contains(df, "ARG NODE_DIGEST") {
		t.Error("ARG NODE_DIGEST missing -- the base digest pin cannot reach FROM")
	}
	if strings.Contains(df, "latest") {
		t.Errorf("floating tag 'latest' in Dockerfile")
	}
	if secretPattern.MatchString(df) {
		t.Errorf("control-plane credential name in Dockerfile:\n%s", secretPattern.FindString(df))
	}
	if keyMaterialPattern.MatchString(df) {
		t.Errorf("hardcoded key material in Dockerfile")
	}
}

// versions.lock is the ONLY build input: it must pin a full 64-hex digest and
// an exact CLI version. versions.env (intent) must not be readable by the
// build -- the Dockerfile only ARGs what the lock provides, enforced above.
func TestRunnerVersionsLockIsPinned(t *testing.T) {
	lock := readImageFile(t, "versions.lock")
	digest := regexp.MustCompile(`(?m)^NODE_DIGEST=(sha256:[0-9a-f]{64})$`).FindStringSubmatch(lock)
	if digest == nil {
		t.Errorf("NODE_DIGEST must be a full 64-hex sha256 pin, got lock:\n%s", lock)
	}
	ver := regexp.MustCompile(`(?m)^CLAUDE_CODE_VERSION=([0-9][0-9A-Za-z.\-+]*)$`).FindStringSubmatch(lock)
	if ver == nil {
		t.Errorf("CLAUDE_CODE_VERSION must be an exact version (no latest/float), got lock:\n%s", lock)
	}
	if secretPattern.MatchString(lock) {
		t.Errorf("control-plane credential name in versions.lock")
	}
}

// The runner entrypoint and script must not hardcode any credential -- the
// task token is read from env only, and must not be echoed into artifacts.
func TestRunnerEntrypointNoSecrets(t *testing.T) {
	for _, name := range []string{"entrypoint.sh", "runner.mjs"} {
		src := readImageFile(t, name)
		if secretPattern.MatchString(src) {
			t.Errorf("%s: control-plane credential name found: %s", name, secretPattern.FindString(src))
		}
		if keyMaterialPattern.MatchString(src) {
			t.Errorf("%s: hardcoded key material found", name)
		}
	}
	mjs := readImageFile(t, "runner.mjs")
	if !strings.Contains(mjs, "process.env") && !strings.Contains(mjs, "env.TASK_TOKEN") {
		t.Errorf("runner.mjs must read its credential from env, not from a file or literal")
	}
	// TASK_PROMPT must actually REACH the CLI: validated-as-set but never
	// passed was a live e2e failure (2026-09-09) where every task died with
	// "Input must be provided either through stdin or as a prompt argument".
	if !strings.Contains(mjs, "input: env.TASK_PROMPT") {
		t.Errorf("runner.mjs must feed TASK_PROMPT to the CLI via spawnSync stdin")
	}
	// fix-runner-etimedout-long-turns: the CLI kill time must be derived from
	// the authoritative sandbox window (SANDBOX_DEADLINE_EPOCH_MS handed down
	// by the node), with a fallback for the older no-env path -- a hardcoded
	// 25min decoupled from the window truncated long single-turn tasks.
	if !strings.Contains(mjs, "SANDBOX_DEADLINE_EPOCH_MS") {
		t.Errorf("runner.mjs must read SANDBOX_DEADLINE_EPOCH_MS")
	}
	// Anchor the CALL SITE, not the identifier: a cliTimeoutMs definition
	// sitting uncalled (spawnSync back on a literal) is exactly the
	// regression this guard exists for, and the bare identifier matches it.
	if !strings.Contains(mjs, "windowMs = cliTimeoutMs()") || !strings.Contains(mjs, "timeout: windowMs") {
		t.Errorf("runner.mjs spawnSync must consume the deadline-derived window (windowMs = cliTimeoutMs(); timeout: windowMs)")
	}
	// A spawnSync timeout surfaces as cli.error (ETIMEDOUT), and a CLI that
	// ran the whole window before being killed is NOT a spawn failure:
	// mislabeling it "runner_spawn_error"/"failed to start" sent diagnosis
	// down the wrong path for months.
	if !strings.Contains(mjs, "cli.error.code === 'ETIMEDOUT'") {
		t.Errorf("runner.mjs must branch on cli.error.code === 'ETIMEDOUT' to report timeouts honestly")
	}
}

// The scanner itself must exist and scan the built image's env + history --
// the spec's assertion guard is a SHALL, so its absence is a failure, not a
// style issue.
func TestRunnerScanSecretsScriptPresent(t *testing.T) {
	scan := readImageFile(t, "scan-secrets.sh")
	for _, want := range []string{"image inspect", "history --no-trunc", "FAIL"} {
		if !strings.Contains(scan, want) {
			t.Errorf("scan-secrets.sh lost its %q check -- the image-content assertion guard is incomplete", want)
		}
	}
}
