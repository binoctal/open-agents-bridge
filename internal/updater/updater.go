package updater

import (
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

var (
	version = "0.3.0"
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

// GetAssetForPlatform returns the download URL for current platform
func GetAssetForPlatform(release *Release) string {
	suffix := fmt.Sprintf("%s_%s", runtime.GOOS, runtime.GOARCH)
	if runtime.GOOS == "windows" {
		suffix += ".exe"
	}

	for _, asset := range release.Assets {
		if strings.Contains(asset.Name, suffix) {
			return asset.DownloadURL
		}
	}
	return ""
}

// DownloadUpdate downloads the new binary with progress
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

	tmpFile, err := os.CreateTemp("", "open-agents-bridge-update-*")
	if err != nil {
		return "", err
	}
	defer tmpFile.Close()

	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
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

	// Backup current binary
	backupPath := currentPath + ".bak"
	os.Remove(backupPath)
	if err := os.Rename(currentPath, backupPath); err != nil {
		return fmt.Errorf("backup failed: %w", err)
	}

	// Move new binary
	if err := os.Rename(newBinary, currentPath); err != nil {
		// Restore backup on failure
		os.Rename(backupPath, currentPath)
		return fmt.Errorf("replace failed: %w", err)
	}

	// Make executable
	if err := os.Chmod(currentPath, 0755); err != nil {
		return fmt.Errorf("chmod failed: %w", err)
	}

	// Remove backup
	os.Remove(backupPath)

	return nil
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
