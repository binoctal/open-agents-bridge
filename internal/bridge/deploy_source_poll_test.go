package bridge

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/binoctal/open-agents-bridge/internal/api"
	"github.com/binoctal/open-agents-bridge/internal/config"
)

// fix-deploy-source-dual-claim: the pending endpoint advertises a
// source_upload feature only to non-executor machines. When the features
// array is present WITHOUT it, this registration is an executor form (a
// hosting-node runner) and must stop polling for source packs — its tree is
// a materialization copy, not the mission workspace, and packing it is how
// the 2026-10-30 vnivni dual-claimant incident produced a stale-tree row.
// A features-less response (old API) must keep polling.

func newDeploySourceBridge(t *testing.T, pendingBody string) *Bridge {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/missions/internal/hosted-deployments/pending" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(pendingBody))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	cfg := &config.Config{
		ServerURL:    "ws" + strings.TrimPrefix(srv.URL, "http"),
		MachineID:    "machine-ds",
		MachineToken: "token-ds",
	}
	return &Bridge{apiClient: api.NewClient(cfg)}
}

func TestPollDeploySourcesStopsWhenSourceUploadWithheld(t *testing.T) {
	b := newDeploySourceBridge(t,
		`{"deploys":[],"features":["chunked_declare"]}`)

	if b.pollDeploySources() {
		t.Fatal("poll with features lacking source_upload must report stop (executor form)")
	}
}

func TestPollDeploySourcesContinuesWhenFeatureGranted(t *testing.T) {
	b := newDeploySourceBridge(t,
		`{"deploys":[],"features":["chunked_declare","source_upload"]}`)

	if !b.pollDeploySources() {
		t.Fatal("poll with source_upload granted must continue")
	}
}

func TestPollDeploySourcesContinuesWithoutFeaturesField(t *testing.T) {
	// Old API: no features array at all — treat as capable (rolling period).
	b := newDeploySourceBridge(t, `{"deploys":[]}`)

	if !b.pollDeploySources() {
		t.Fatal("poll without a features array must continue (old API compatibility)")
	}
}
