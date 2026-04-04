package test

import (
	"testing"

	"github.com/open-agents/open-agents-bridge/internal/updater"
)

func TestVersion(t *testing.T) {
	// Version constant should be set
	if updater.Version == "" {
		t.Error("Version is empty")
	}
}

func TestRepoConstants(t *testing.T) {
	if updater.RepoOwner == "" {
		t.Error("RepoOwner is empty")
	}
	if updater.RepoName == "" {
		t.Error("RepoName is empty")
	}
}

func TestCheckUpdate(t *testing.T) {
	// This test verifies the function doesn't panic
	// Actual update check requires network access
	release, hasUpdate, err := updater.CheckUpdate()

	// Either succeeds or fails gracefully (network may not be available)
	if err != nil {
		t.Logf("CheckUpdate returned error (expected without network): %v", err)
		return
	}

	if release != nil {
		t.Logf("Latest release: %s, hasUpdate: %v", release.TagName, hasUpdate)
	}
}

func TestGetAssetForPlatform(t *testing.T) {
	release := &updater.Release{
		TagName: "v1.0.0",
		Assets: []updater.Asset{
			{Name: "open-agents-linux-amd64", DownloadURL: "https://example.com/linux"},
			{Name: "open-agents-darwin-amd64", DownloadURL: "https://example.com/darwin"},
			{Name: "open-agents-windows-amd64.exe", DownloadURL: "https://example.com/windows"},
		},
	}

	url := updater.GetAssetForPlatform(release)
	// Should return a URL for current platform (or empty if not found)
	t.Logf("Asset URL for current platform: %s", url)
}

func TestGetAssetForPlatformEmpty(t *testing.T) {
	release := &updater.Release{
		TagName: "v1.0.0",
		Assets:  []updater.Asset{},
	}

	url := updater.GetAssetForPlatform(release)
	if url != "" {
		t.Errorf("Expected empty URL for empty assets, got %s", url)
	}
}
