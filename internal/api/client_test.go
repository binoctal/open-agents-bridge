package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchManifestDecodesFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"deploymentId":"d1","files":[{"path":"a.txt","sha256":"` +
			"0000000000000000000000000000000000000000000000000000000000000000" +
			`","size":3}]}`))
	}))
	defer srv.Close()

	c := New("http://api.invalid", "agent", "secret")
	files, err := c.FetchManifest(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("FetchManifest: %v", err)
	}
	if len(files) != 1 || files[0].Path != "a.txt" || files[0].Size != 3 {
		t.Fatalf("unexpected files: %+v", files)
	}
}

func TestFetchManifestRejectsEmptyAndNon2xx(t *testing.T) {
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"files":[]}`))
	}))
	defer empty.Close()
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
	}))
	defer broken.Close()

	c := New("http://api.invalid", "agent", "secret")
	if _, err := c.FetchManifest(context.Background(), empty.URL); err == nil {
		t.Fatal("empty manifest must fail")
	}
	if _, err := c.FetchManifest(context.Background(), broken.URL); err == nil {
		t.Fatal("non-2xx must fail")
	}
}
