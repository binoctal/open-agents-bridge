package node

import (
	"archive/tar"
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
	"strings"
	"testing"
)

func TestChecksumFor(t *testing.T) {
	sums := "aaaabbbbccccdddd  caddy_2.11.7_linux_amd64.tar.gz\n1111222233334444  caddy_2.11.7_linux_arm64.tar.gz\n"
	if got, ok := checksumFor(sums, "caddy_2.11.7_linux_arm64.tar.gz"); !ok || got != "1111222233334444" {
		t.Fatalf("checksumFor arm64 = %q, %v", got, ok)
	}
	if _, ok := checksumFor(sums, "missing.tar.gz"); ok {
		t.Fatal("checksumFor found an entry that does not exist")
	}
}

func TestExtractSingleFile(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	payload := []byte("#!/bin/sh\necho fake caddy\n")
	_ = tw.WriteHeader(&tar.Header{Name: "README", Typeflag: tar.TypeReg, Size: int64(len("readme"))})
	_, _ = tw.Write([]byte("readme"))
	_ = tw.WriteHeader(&tar.Header{Name: "caddy", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(payload))})
	_, _ = tw.Write(payload)
	_ = tw.Close()
	_ = gz.Close()

	got, err := extractSingleFile(buf.Bytes())
	if err != nil {
		t.Fatalf("extractSingleFile: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("extracted %q, want %q", got, payload)
	}
}

func TestDownloadCaddyVerifiesChecksum(t *testing.T) {
	name := "caddy_" + caddyVersion + "_linux_" + runtime.GOARCH + ".tar.gz"
	var tarball []byte
	{
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		_ = tw.WriteHeader(&tar.Header{Name: "caddy", Typeflag: tar.TypeReg, Mode: 0o755, Size: 3})
		_, _ = tw.Write([]byte("bin"))
		_ = tw.Close()
		_ = gz.Close()
		tarball = buf.Bytes()
	}
	goodSum := sha256.Sum256(tarball)
	badSum := sha256.Sum256([]byte("other"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.TrimPrefix(r.URL.Path, "/v"+caddyVersion+"/") {
		case name:
			_, _ = w.Write(tarball)
		case "checksums.txt":
			_, _ = fmt.Fprintf(w, "%s  %s\n%s  other.tar.gz\n", hex.EncodeToString(badSum[:]), name, hex.EncodeToString(goodSum[:]))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	orig := caddyDownloadBase
	caddyDownloadBase = srv.URL + "/v" + caddyVersion
	defer func() { caddyDownloadBase = orig }()

	dest := filepath.Join(t.TempDir(), "bin", "caddy")
	if err := downloadCaddy(dest); err == nil {
		t.Fatal("downloadCaddy succeeded despite a sha256 mismatch")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("mismatched tarball must not land on disk: %v", err)
	}
}

func TestEnsureCaddyPrefersPath(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "caddy")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	got, managed, err := ensureCaddy(t.TempDir(), func(string, ...any) {})
	if err != nil {
		t.Fatalf("ensureCaddy: %v", err)
	}
	if managed {
		t.Fatal("PATH caddy must not be daemon-managed")
	}
	if got != fake {
		t.Fatalf("ensureCaddy returned %q, want %q", got, fake)
	}
}

