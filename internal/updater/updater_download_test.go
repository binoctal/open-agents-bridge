package updater

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

	path, err := DownloadUpdate(srv.URL+"/open-agents-bridge_0.11.2_linux_amd64.tar.gz", sha256Hex(buf.Bytes()))
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

	path, err := DownloadUpdate(srv.URL+"/open-agents-bridge_0.11.2_windows_amd64.zip", sha256Hex(buf.Bytes()))
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

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func tarGzArchive(t *testing.T, content []byte) []byte {
	t.Helper()
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
	return buf.Bytes()
}

// 5e.3: a tampered archive (or a wrong/absent checksum) must never be
// extracted, let alone installed.
func TestDownloadUpdateRefusesChecksumMismatch(t *testing.T) {
	archive := tarGzArchive(t, []byte("tampered payload"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive)
	}))
	defer srv.Close()

	url := srv.URL + "/open-agents-bridge_0.11.2_linux_amd64.tar.gz"
	if _, err := DownloadUpdate(url, sha256Hex([]byte("the published archive"))); err == nil {
		t.Fatal("tampered archive accepted")
	}
	if _, err := DownloadUpdate(url, ""); err == nil {
		t.Fatal("download without an expected checksum accepted")
	}
}

func TestDownloadVerifiedEndToEnd(t *testing.T) {
	content := []byte("published bridge binary")
	archive := tarGzArchive(t, content)
	name := fmt.Sprintf("open-agents-bridge_0.99.0_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)

	var checksums string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/checksums.txt":
			w.Write([]byte(checksums))
		default:
			w.Write(archive)
		}
	}))
	defer srv.Close()

	res := &UpdateResult{
		DownloadURL:  srv.URL + "/" + name,
		AssetName:    name,
		ChecksumsURL: srv.URL + "/checksums.txt",
	}

	checksums = sha256Hex([]byte("other")) + "  open-agents-bridge_0.99.0_other_arch.tar.gz\n" +
		sha256Hex(archive) + "  " + name + "\n"
	path, err := DownloadVerified(res)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	if got, _ := os.ReadFile(path); !bytes.Equal(got, content) {
		t.Fatalf("extracted content mismatch: %q", got)
	}

	checksums = sha256Hex([]byte("swapped")) + "  " + name + "\n"
	if _, err := DownloadVerified(res); err == nil {
		t.Fatal("archive not matching checksums.txt accepted")
	}

	checksums = sha256Hex(archive) + "  some-other-file.tar.gz\n"
	if _, err := DownloadVerified(res); err == nil {
		t.Fatal("archive absent from checksums.txt accepted")
	}

	noSums := *res
	noSums.ChecksumsURL = ""
	if _, err := DownloadVerified(&noSums); err == nil {
		t.Fatal("release without checksums.txt accepted")
	}
}

func TestPlatformAssetNeverPicksChecksums(t *testing.T) {
	release := &Release{Assets: []Asset{
		{Name: "checksums.txt", DownloadURL: "https://x/checksums.txt"},
	}}
	if a := platformAsset(release); a != nil {
		t.Fatalf("checksums.txt matched as a platform archive: %+v", a)
	}
}
