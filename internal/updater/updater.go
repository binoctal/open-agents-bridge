package updater

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
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
	AssetSize      int64
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
		downloadURL := GetAssetForPlatform(&release)
		result.DownloadURL = downloadURL
		for _, asset := range release.Assets {
			if asset.DownloadURL == downloadURL {
				result.AssetSize = asset.Size
				break
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
	token := fmt.Sprintf("_%s_%s.", runtime.GOOS, runtime.GOARCH)

	for _, asset := range release.Assets {
		if strings.Contains(asset.Name, token) {
			return asset.DownloadURL
		}
	}
	return ""
}

// DownloadUpdate downloads the release archive and extracts the bridge
// binary from it, returning the path of the extracted executable.
func DownloadUpdate(url string) (string, error) {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download returned %d", resp.StatusCode)
	}

	binaryName := "open-agents-bridge"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}

	switch {
	case strings.HasSuffix(url, ".zip"):
		return extractFromZip(resp.Body, binaryName)
	default:
		// goreleaser ships .tar.gz for non-Windows platforms
		return extractFromTarGz(resp.Body, binaryName)
	}
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
	// mount, where os.Rename fails with "invalid cross-device link").
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
