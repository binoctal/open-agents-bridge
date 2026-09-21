package updater

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestGetAssetForPlatformMatchesArchiveName(t *testing.T) {
	release := &Release{Assets: []Asset{
		{Name: "checksums.txt", DownloadURL: "https://x/checksums.txt"},
		{Name: "open-agents-bridge_0.11.2_darwin_arm64.tar.gz", DownloadURL: "https://x/darwin"},
		{Name: "open-agents-bridge_0.11.2_linux_amd64.tar.gz", DownloadURL: "https://x/linux"},
		{Name: "open-agents-bridge_0.11.2_windows_amd64.zip", DownloadURL: "https://x/windows"},
	}}

	// Force-match the linux asset regardless of host platform by rewriting
	// the expected token from the actual runtime values.
	token := "_" + runtime.GOOS + "_" + runtime.GOARCH + "."
	want := ""
	for _, a := range release.Assets {
		if bytes.Contains([]byte(a.Name), []byte(token)) {
			want = a.DownloadURL
		}
	}
	if want == "" {
		t.Skip("no asset for this platform in fixture")
	}
	if got := GetAssetForPlatform(release); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}

	// A bare ".exe" suffix must never be required: windows assets are .zip.
	if runtime.GOOS == "windows" && GetAssetForPlatform(release) == "" {
		t.Fatal("windows asset not matched")
	}
}

func TestDownloadUpdateExtractsTarGz(t *testing.T) {
	content := []byte("fake bridge binary payload")

	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	if err := tw.WriteHeader(&tar.Header{Name: "open-agents-bridge", Mode: 0755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gzw.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(buf.Bytes())
	}))
	defer srv.Close()

	path, err := DownloadUpdate(srv.URL + "/open-agents-bridge_0.11.2_linux_amd64.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("extracted content mismatch: %q", got)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0755 {
		t.Fatalf("extracted file not executable: %v %v", info, err)
	}
	if filepath.Base(path) == "open-agents-bridge_0.11.2_linux_amd64.tar.gz" {
		t.Fatal("archive installed as-is instead of extracted")
	}
}

func TestDownloadUpdateExtractsZip(t *testing.T) {
	content := []byte("fake zipped bridge binary")
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// Name matches what DownloadUpdate looks for on the host platform
	// ("open-agents-bridge.exe" only on Windows).
	f, err := zw.Create("open-agents-bridge")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(content); err != nil {
		t.Fatal(err)
	}
	zw.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(buf.Bytes())
	}))
	defer srv.Close()

	path, err := DownloadUpdate(srv.URL + "/open-agents-bridge_0.11.2_windows_amd64.zip")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("extracted content mismatch: %q", got)
	}
}
