package agent

// Dual-home routing and degradation tests (dual-home-hosting-agent tasks
// 2.2/2.4): an item polled from endpoint A reports and presigns through A --
// never through B -- and a 401 retires exactly the endpoint that was rejected
// while the rest keep serving; every endpoint retired means process exit.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/binoctal/open-agents/hosting-agent/internal/api"
	"github.com/binoctal/open-agents/hosting-agent/internal/githttp"
	"github.com/binoctal/open-agents/hosting-agent/internal/store"
)

// fakeIngress is the route-wiring seam recording upserts (register_route path).
type fakeIngress struct {
	mu    sync.Mutex
	added map[string]string
}

func (f *fakeIngress) Upsert(ctx context.Context, deploymentID, containerAddr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.added == nil {
		f.added = map[string]string{}
	}
	f.added[deploymentID] = containerAddr
	return nil
}

func (f *fakeIngress) Remove(ctx context.Context, deploymentID string) error { return nil }

// epServer is one fake API environment: it counts requests per path and can
// be told to reject everything with 401 (the rotated-secret scenario).
type epServer struct {
	mu      sync.Mutex
	counts  map[string]int
	unauth  bool
	handler http.HandlerFunc // optional per-path custom answers (work items)
}

func (s *epServer) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[path]
}

func (s *epServer) requests() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.counts {
		n += c
	}
	return n
}

// newEnvServer spins one environment: srv.URL is the endpoint BaseURL.
func newEnvServer(t *testing.T, custom http.HandlerFunc) (*epServer, *httptest.Server) {
	t.Helper()
	s := &epServer{counts: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.counts[r.URL.Path]++
		unauth := s.unauth
		s.mu.Unlock()
		if unauth {
			w.WriteHeader(401)
			return
		}
		if custom != nil {
			custom(w, r)
			return
		}
		w.WriteHeader(200)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	return s, srv
}

// TestPollWorkRoutesReportsAndPresignsToOfferingEndpoint locks D2 end to end:
// endpoint A offers a materialize item; the manifest fetch, the presign
// exchange, the object download chain and the materialized report must all
// land on A, and B -- served in the same poll -- must see nothing but its own
// empty work poll.
func TestPollWorkRoutesReportsAndPresignsToOfferingEndpoint(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()

	// One tiny tree: a single index.html object.
	content := []byte("hello dual-home")
	dig := sha256.Sum256(content)
	sum := hex.EncodeToString(dig[:])

	objects := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/"+sum {
			_, _ = w.Write(content)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(objects.Close)

	// srvA must pre-exist: the handler closure reads its URL at request time
	// (after assignment), which := scoping would forbid.
	var (
		sa   *epServer
		srvA *httptest.Server
	)
	sa, srvA = newEnvServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/internal/hosting-agent/work":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []api.WorkItem{{
				DeploymentID: "dep-a",
				Action:       api.ActionMaterialize,
				ManifestURL:  srvA.URL + "/manifests/dep-a",
				SourcePrefix: "deploys/dep-a",
			}}})
		case "/manifests/dep-a":
			_, _ = w.Write([]byte(fmt.Sprintf(`{"files":[{"path":"index.html","sha256":%q,"size":%d}]}`, sum, len(content))))
		case "/internal/hosting-agent/presign-source":
			_ = json.NewEncoder(w).Encode(api.PresignResponse{URLs: []api.PresignedObject{{
				Key: "deploys/dep-a/" + sum,
				URL: objects.URL + "/" + sum,
			}}})
		case "/internal/hosting-agent/report":
			w.WriteHeader(200)
		default:
			http.NotFound(w, r)
		}
	})
	sb, srvB := newEnvServer(t, nil)

	a := &Agent{
		Endpoints: []APIEndpoint{
			{Client: api.New(srvA.URL, "t", "sec-a"), BaseURL: srvA.URL, CoolifyProjectUUID: "proj-a"},
			{Client: api.New(srvB.URL, "t", "sec-b"), BaseURL: srvB.URL, CoolifyProjectUUID: "proj-b"},
		},
		Store:   store.New(dir, nil),
		GitHTTP: githttp.New(dir+"/repos", "127.0.0.1", 34200, 34299),
	}
	if err := a.pollWork(context.Background()); err != nil {
		t.Fatalf("pollWork: %v", err)
	}

	if got := sa.count("/internal/hosting-agent/presign-source"); got == 0 {
		t.Fatal("presign must go through the offering endpoint A")
	}
	if got := sa.count("/internal/hosting-agent/report"); got == 0 {
		t.Fatal("the materialized report must land on A")
	}
	if got := sb.count("/internal/hosting-agent/report"); got != 0 {
		t.Fatalf("endpoint B must never see A's reports, got %d", got)
	}
	if got := sb.count("/internal/hosting-agent/presign-source"); got != 0 {
		t.Fatalf("endpoint B must never see A's presigns, got %d", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "repos", "dep-a.git")); err != nil {
		t.Fatalf("materialized repo missing: %v", err)
	}
}

// TestEndpoint401RetiresOnlyThatEndpoint locks D3: a rejected credential on B
// takes B out (its request count stops growing) while A keeps serving work and
// watermark fanout.
func TestEndpoint401RetiresOnlyThatEndpoint(t *testing.T) {
	sa, srvA := newEnvServer(t, nil)
	sb, srvB := newEnvServer(t, nil)

	var logs []string
	var logMu sync.Mutex
	a := &Agent{
		Endpoints: []APIEndpoint{
			{Client: api.New(srvA.URL, "t", "sec-a"), BaseURL: srvA.URL},
			{Client: api.New(srvB.URL, "t", "sec-b"), BaseURL: srvB.URL},
		},
		Logf: func(format string, args ...any) {
			logMu.Lock()
			defer logMu.Unlock()
			logs = append(logs, fmt.Sprintf(format, args...))
		},
	}

	// First poll: both healthy.
	if err := a.pollWork(context.Background()); err != nil {
		t.Fatalf("pollWork: %v", err)
	}

	// B's secret rotates server-side; A keeps serving. The poll that
	// discovers the 401 does reach B once -- retirement starts after it.
	sb.mu.Lock()
	sb.unauth = true
	sb.mu.Unlock()
	if err := a.pollWork(context.Background()); err != nil {
		t.Fatalf("one retired endpoint must not abort the agent: %v", err)
	}
	bBefore := sb.requests()
	logMu.Lock()
	announced := false
	for _, l := range logs {
		if len(l) > 0 && l == fmt.Sprintf("endpoint %s disabled: credential rejected (401); restart the agent with a fresh secret to re-enable", srvB.URL) {
			announced = true
		}
	}
	logMu.Unlock()
	if !announced {
		t.Fatalf("the retirement must be announced in the log, got %v", logs)
	}

	// Subsequent ticks skip B entirely (no more requests), A keeps polling.
	if err := a.pollWork(context.Background()); err != nil {
		t.Fatalf("pollWork after retirement: %v", err)
	}
	if got := sb.requests(); got != bBefore {
		t.Fatalf("a retired endpoint must not be polled again: %d -> %d requests", bBefore, got)
	}
	if sa.count("/internal/hosting-agent/work") < 3 {
		t.Fatalf("endpoint A must keep serving, got %d work polls", sa.count("/internal/hosting-agent/work"))
	}

	// Watermark fanout (D4) still reaches A after B's retirement.
	if err := a.reportWatermark(context.Background()); err != nil {
		t.Fatalf("reportWatermark: %v", err)
	}
	if got := sa.count("/internal/hosting-agent/watermark"); got != 1 {
		t.Fatalf("watermark must fan out to the surviving endpoint, got %d posts", got)
	}
}

// TestAllEndpointsDisabledExits locks D3's exit condition: with every endpoint
// retired the agent can serve nothing, so Run must exit (systemd restart).
func TestAllEndpointsDisabledExits(t *testing.T) {
	sa, srvA := newEnvServer(t, nil)
	sb, srvB := newEnvServer(t, nil)
	sa.mu.Lock()
	sa.unauth = true
	sa.mu.Unlock()
	sb.mu.Lock()
	sb.unauth = true
	sb.mu.Unlock()

	a := &Agent{
		Endpoints: []APIEndpoint{
			{Client: api.New(srvA.URL, "t", "sec-a"), BaseURL: srvA.URL},
			{Client: api.New(srvB.URL, "t", "sec-b"), BaseURL: srvB.URL},
		},
	}
	err := a.pollWork(context.Background())
	if err == nil {
		t.Fatal("every endpoint rejected must return an error (exit 1)")
	}
	if want := "all API endpoints disabled"; !strings.Contains(err.Error(), want) {
		t.Fatalf("error must name the condition, got %q", err.Error())
	}
}

// TestWatermarkFansOutToAllEndpoints locks D4: the one node-level watermark
// reaches every active endpoint so each environment's allocator sees the full
// port occupancy.
func TestWatermarkFansOutToAllEndpoints(t *testing.T) {
	sa, srvA := newEnvServer(t, nil)
	sb, srvB := newEnvServer(t, nil)
	a := &Agent{
		Endpoints: []APIEndpoint{
			{Client: api.New(srvA.URL, "t", "sec-a"), BaseURL: srvA.URL},
			{Client: api.New(srvB.URL, "t", "sec-b"), BaseURL: srvB.URL},
		},
	}
	if err := a.reportWatermark(context.Background()); err != nil {
		t.Fatalf("reportWatermark: %v", err)
	}
	for name, s := range map[string]*epServer{"A": sa, "B": sb} {
		if got := s.count("/internal/hosting-agent/watermark"); got != 1 {
			t.Fatalf("endpoint %s must receive the watermark exactly once, got %d", name, got)
		}
	}
}
