package updater

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	RepoOwner     = "binoctal"
	RepoName      = "open-agents-bridge"
	CheckInterval = 24 * time.Hour

	// ChecksumsAsset is goreleaser's checksum.name_template.
	ChecksumsAsset = "checksums.txt"
	// Ceilings on what an update may download; a release archive is a few MB.
	maxArchiveBytes   = 256 << 20
	maxChecksumsBytes = 64 << 10
	maxSignatureBytes = 4 << 10
)

// Release builds get these from goreleaser's -X ldflags. The defaults are what
// a build without them reports — `go install`, or a local `go build`.
//
// The default deliberately is not a version number. It used to read "0.6.0",
// which meant a `go install` build of 0.6.2 introduced itself as 0.6.0 and,
// because CheckUpdate compares this value against the latest release, kept
// offering an update it had already installed. "dev" compares below every
// release, so a dev build is honestly told it is behind, and nothing has to
// remember to bump a string here on every release.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

var Version = version

func GetVersionInfo() string {
	return fmt.Sprintf("%s (commit: %s, built: %s)", version, commit, date)
}

// Release represents a GitHub release
type Release struct {
	TagName     string  `json:"tag_name"`
	Assets      []Asset `json:"assets"`
	PublishedAt string  `json:"published_at"`
	Body        string  `json:"body"`
}

// Asset represents a release asset
type Asset struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	DownloadURL string `json:"browser_download_url"`
}

// UpdateResult holds the result of an update check
type UpdateResult struct {
	HasUpdate      bool
	CurrentVersion string
	LatestVersion  string
	ReleaseNotes   string
	DownloadURL    string
	AssetName      string
	AssetSize      int64
	// ChecksumsURL is the same release's checksums.txt; empty means the
	// release has none and the update must be refused, not installed blind.
	ChecksumsURL string
	// SignatureURL is checksums.txt.sig of the same release (5e.2).
	SignatureURL string
}

// compareSemver compares two semver strings. Returns -1, 0, or 1.
func compareSemver(a, b string) int {
	a = strings.TrimPrefix(a, "v")
	b = strings.TrimPrefix(b, "v")

	partsA := strings.SplitN(a, "-", 2)
	partsB := strings.SplitN(b, "-", 2)

	segsA := strings.Split(partsA[0], ".")
	segsB := strings.Split(partsB[0], ".")

	for i := 0; i < 3; i++ {
		va, vb := 0, 0
		if i < len(segsA) {
			va, _ = strconv.Atoi(segsA[i])
		}
		if i < len(segsB) {
			vb, _ = strconv.Atoi(segsB[i])
		}
		if va < vb {
			return -1
		}
		if va > vb {
			return 1
		}
	}

	// Pre-release versions are lower than release
	aHasPre := len(partsA) > 1
	bHasPre := len(partsB) > 1
	if aHasPre && !bHasPre {
		return -1
	}
	if !aHasPre && bHasPre {
		return 1
	}
	return 0
}

// CheckUpdate checks for a new version
func CheckUpdate() (*UpdateResult, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", RepoOwner, RepoName)

	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", fmt.Sprintf("open-agents-bridge/%s", Version))

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub API returned %d", resp.StatusCode)
	}

	var release Release
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, err
	}

	latestVersion := strings.TrimPrefix(release.TagName, "v")
	hasUpdate := compareSemver(Version, latestVersion) < 0

	result := &UpdateResult{
		HasUpdate:      hasUpdate,
		CurrentVersion: Version,
		LatestVersion:  latestVersion,
		ReleaseNotes:   release.Body,
	}

	if hasUpdate {
		if asset := platformAsset(&release); asset != nil {
			result.DownloadURL = asset.DownloadURL
			result.AssetName = asset.Name
			result.AssetSize = asset.Size
		}
		for _, asset := range release.Assets {
			if asset.Name == ChecksumsAsset {
				result.ChecksumsURL = asset.DownloadURL
			}
			if asset.Name == SignatureAsset {
				result.SignatureURL = asset.DownloadURL
			}
		}
	}

	return result, nil
}

// GetAssetForPlatform returns the download URL for current platform.
// Release assets are archives named open-agents-bridge_<ver>_<goos>_<goarch>.tar.gz
// (or .zip on Windows), so the platform token is matched against the archive
// stem — not a bare binary suffix, which never matched any asset.
func GetAssetForPlatform(release *Release) string {
	if asset := platformAsset(release); asset != nil {
		return asset.DownloadURL
	}
	return ""
}

func platformAsset(release *Release) *Asset {
	token := fmt.Sprintf("_%s_%s.", runtime.GOOS, runtime.GOARCH)

	for i := range release.Assets {
		if strings.Contains(release.Assets[i].Name, token) {
			return &release.Assets[i]
		}
	}
	return nil
}

// DownloadVerified downloads the update described by res and installs nothing
// unless the archive matches the SHA256 that the same release's checksums.txt
// lists for it. A release without checksums.txt is refused rather than
// skipped: whoever can swap an asset can also make checksums.txt disappear.
//
// Order matters: the signature over checksums.txt is verified FIRST, then the
// archive hash against the (now authenticated) checksums. Without the first
// step an attacker who can replace release assets replaces the archive and
// checksums.txt together. Any failure leaves the installed binary untouched.
func DownloadVerified(res *UpdateResult) (string, error) {
	return downloadVerified(res, trustedKeys, RequireSignature())
}

func downloadVerified(res *UpdateResult, keys []string, requireSig bool) (string, error) {
	if res.ChecksumsURL == "" {
		return "", fmt.Errorf("release has no %s; refusing an unverified update", ChecksumsAsset)
	}
	if res.AssetName == "" || res.DownloadURL == "" {
		return "", fmt.Errorf("no release archive for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	body, err := httpGetLimited(res.ChecksumsURL, maxChecksumsBytes, 30*time.Second)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", ChecksumsAsset, err)
	}
	if res.SignatureURL == "" {
		if requireSig {
			return "", fmt.Errorf("refusing update: %w", ErrNoSignature)
		}
	} else {
		sig, err := httpGetLimited(res.SignatureURL, maxSignatureBytes, 30*time.Second)
		if err != nil {
			return "", fmt.Errorf("fetch %s: %w", SignatureAsset, err)
		}
		if err := verifySignature(body, sig, keys); err != nil {
			return "", fmt.Errorf("refusing update: %w", err)
		}
	}
	want, err := parseChecksum(body, res.AssetName)
	if err != nil {
		return "", err
	}
	return DownloadUpdate(res.DownloadURL, want)
}

// parseChecksum finds name in sha256sum-format text ("<hex>  <name>").
func parseChecksum(body []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(body))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		sum := strings.ToLower(fields[0])
		if decoded, err := hex.DecodeString(sum); err != nil || len(decoded) != sha256.Size {
			return "", fmt.Errorf("%s has a malformed entry for %s", ChecksumsAsset, name)
		}
		return sum, nil
	}
	return "", fmt.Errorf("%s has no entry for %s", ChecksumsAsset, name)
}

// DownloadUpdate downloads the release archive, refuses it unless its SHA256
// equals wantSHA256, and only then extracts the bridge binary, returning the
// path of the extracted executable. An empty wantSHA256 is an error.
func DownloadUpdate(url, wantSHA256 string) (string, error) {
	if wantSHA256 == "" {
		return "", fmt.Errorf("no expected checksum; refusing an unverified update")
	}
	data, err := httpGetLimited(url, maxArchiveBytes, 5*time.Minute)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != strings.ToLower(wantSHA256) {
		return "", fmt.Errorf("checksum mismatch for downloaded archive: got %s, want %s", got, wantSHA256)
	}

	binaryName := "open-agents-bridge"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}

	switch {
	case strings.HasSuffix(url, ".zip"):
		return extractFromZip(bytes.NewReader(data), binaryName)
	default:
		// goreleaser ships .tar.gz for non-Windows platforms
		return extractFromTarGz(bytes.NewReader(data), binaryName)
	}
}

func httpGetLimited(url string, limit int64, timeout time.Duration) ([]byte, error) {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("download returned %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("download exceeds %d bytes", limit)
	}
	return data, nil
}

func extractFromTarGz(r io.Reader, binaryName string) (string, error) {
	gzr, err := gzip.NewReader(r)
	if err != nil {
		return "", fmt.Errorf("read gzip: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return "", fmt.Errorf("binary %s not found in archive", binaryName)
		}
		if err != nil {
			return "", fmt.Errorf("read tar: %w", err)
		}
		if filepath.Base(hdr.Name) != binaryName || hdr.Typeflag != tar.TypeReg {
			continue
		}
		return writeTempExecutable(tr)
	}
}

func extractFromZip(r io.Reader, binaryName string) (string, error) {
	// archive/zip needs a ReaderAt; buffer the download first
	data, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("read zip: %w", err)
	}
	for _, f := range zr.File {
		if filepath.Base(f.Name) != binaryName || f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		path, werr := writeTempExecutable(rc)
		rc.Close()
		return path, werr
	}
	return "", fmt.Errorf("binary %s not found in archive", binaryName)
}

func writeTempExecutable(r io.Reader) (string, error) {
	tmpFile, err := os.CreateTemp("", "open-agents-bridge-update-*")
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(tmpFile, r); err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return "", err
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpFile.Name())
		return "", err
	}
	if err := os.Chmod(tmpFile.Name(), 0755); err != nil {
		os.Remove(tmpFile.Name())
		return "", err
	}
	return tmpFile.Name(), nil
}

// ApplyUpdate replaces the current binary with the new one
func ApplyUpdate(newBinary string) error {
	currentPath, err := os.Executable()
	if err != nil {
		return err
	}
	currentPath, _ = filepath.Abs(currentPath)
	return applyUpdateTo(currentPath, newBinary)
}

// applyUpdateTo swaps the binary at dst for newBinary.
func applyUpdateTo(dst, newBinary string) error {
	// Stage the new binary next to the target so the final rename never
	// crosses a filesystem boundary (e.g. /tmp tmpfs vs ~/.local on another
	// mount, where os.Rename fails with "invalid cross-machine link").
	stagePath, err := stageNextTo(dst, newBinary)
	if err != nil {
		return fmt.Errorf("stage failed: %w", err)
	}
	defer os.Remove(stagePath)

	// Backup current binary
	backupPath := dst + ".bak"
	os.Remove(backupPath)
	if err := os.Rename(dst, backupPath); err != nil {
		return fmt.Errorf("backup failed: %w", err)
	}

	// Move staged binary into place (same directory, same filesystem)
	if err := os.Rename(stagePath, dst); err != nil {
		// Restore backup on failure
		os.Rename(backupPath, dst)
		return fmt.Errorf("replace failed: %w", err)
	}

	// Make executable
	if err := os.Chmod(dst, 0755); err != nil {
		return fmt.Errorf("chmod failed: %w", err)
	}

	// Remove backup
	os.Remove(backupPath)

	return nil
}

// stageNextTo copies src into a temp file in dst's directory, marked
// executable, so renaming it over dst stays on one filesystem.
func stageNextTo(dst, src string) (string, error) {
	srcFile, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer srcFile.Close()

	stage, err := os.CreateTemp(filepath.Dir(dst), ".open-agents-bridge-stage-*")
	if err != nil {
		return "", err
	}
	stageName := stage.Name()

	if _, err := io.Copy(stage, srcFile); err != nil {
		stage.Close()
		os.Remove(stageName)
		return "", err
	}
	if err := stage.Close(); err != nil {
		os.Remove(stageName)
		return "", err
	}
	if err := os.Chmod(stageName, 0755); err != nil {
		os.Remove(stageName)
		return "", err
	}
	return stageName, nil
}

// ShouldCheck returns true if enough time has passed since last check
func ShouldCheck(lastCheckFile string) bool {
	info, err := os.Stat(lastCheckFile)
	if err != nil {
		return true // File doesn't exist, should check
	}
	return time.Since(info.ModTime()) > CheckInterval
}

// MarkChecked updates the last check timestamp
func MarkChecked(lastCheckFile string) error {
	dir := filepath.Dir(lastCheckFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(lastCheckFile, []byte(time.Now().Format(time.RFC3339)), 0644)
}

// AutoCheck performs a background update check on startup
func AutoCheck(configDir string) *UpdateResult {
	checkFile := filepath.Join(configDir, ".last-update-check")
	if !ShouldCheck(checkFile) {
		return nil
	}

	result, err := CheckUpdate()
	if err != nil {
		return nil
	}

	MarkChecked(checkFile)
	if result.HasUpdate {
		return result
	}
	return nil
}
