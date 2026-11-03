package coolify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeCoolify is a minimal Coolify REST API: keys by name, apps by uuid with
// 404 on unknown, deployments by uuid. Requests are recorded so field shapes
// are assertable (they are load-bearing 4.3.17 workarounds).
type fakeCoolify struct {
	keys        map[string]string // name -> uuid
	apps        map[string]map[string]any
	deployments map[string]map[string]any
	lastCreate  map[string]any
	lastPatch   map[string]any
}

func newFakeCoolify() *fakeCoolify {
	return &fakeCoolify{
		keys:        map[string]string{},
		apps:        map[string]map[string]any{},
		deployments: map[string]map[string]any{},
	}
}

func (f *fakeCoolify) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	send := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case r.URL.Path == "/api/v1/security/keys" && r.Method == http.MethodGet:
		var out []map[string]string
		for name, uuid := range f.keys {
			out = append(out, map[string]string{"name": name, "uuid": uuid})
		}
		send(out)
	case r.URL.Path == "/api/v1/security/keys" && r.Method == http.MethodPost:
		name, _ := body["name"].(string)
		if _, ok := f.keys[name]; ok {
			w.WriteHeader(http.StatusConflict)
			return
		}
		uuid := "key-" + name
		f.keys[name] = uuid
		send(map[string]string{"uuid": uuid})
	case r.URL.Path == "/api/v1/applications/private-deploy-key" && r.Method == http.MethodPost:
		f.lastCreate = body
		uuid := fmt.Sprintf("app-%d", len(f.apps)+1)
		f.apps[uuid] = map[string]any{"uuid": uuid, "name": "derived"}
		send(map[string]string{"uuid": uuid})
	case strings.HasPrefix(r.URL.Path, "/api/v1/applications/") && r.Method == http.MethodPatch:
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/applications/"), "")
		if _, ok := f.apps[id]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.lastPatch = body
		send(map[string]any{})
	case r.URL.Path == "/api/v1/applications" && r.Method == http.MethodGet:
		var out []map[string]any
		for _, a := range f.apps {
			out = append(out, a)
		}
		send(out)
	case strings.HasPrefix(r.URL.Path, "/api/v1/applications/") && r.Method == http.MethodGet:
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/applications/")
		a, ok := f.apps[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		send(a)
	case r.URL.Path == "/api/v1/deploy" && r.Method == http.MethodPost:
		uuid, _ := body["uuid"].(string)
		if _, ok := f.apps[uuid]; !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		depUUID := "dep-1"
		f.deployments[depUUID] = map[string]any{"uuid": depUUID, "status": "queued"}
		send(map[string]any{"deployments": []map[string]string{{"deployment_uuid": depUUID}}})
	case strings.HasPrefix(r.URL.Path, "/api/v1/deployments/") && r.Method == http.MethodGet:
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/deployments/")
		d, ok := f.deployments[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound) // queue rows age out
			return
		}
		send(d)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newTestClient(t *testing.T) (*Client, *fakeCoolify) {
	t.Helper()
	fake := newFakeCoolify()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	return New(srv.URL, "tok"), fake
}

func TestCreateApplicationUsesHostnameFormThenPatchesGatewayURL(t *testing.T) {
	c, fake := newTestClient(t)
	ctx := context.Background()

	uuid, err := c.CreateApplication(ctx, ApplicationInput{
		ProjectUUID:     "proj",
		ServerUUID:      "srv",
		EnvironmentName: "production",
		GitRepository:   "http://172.17.0.1:24123/oa-preview-m1.git",
		GitBranch:       "main",
		BuildPack:       "dockerfile",
		PortsExposes:    32100,
		Domains:         "https://abc.deploy.openagents.top",
		LimitsMemory:    "128m",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// The create-time URL must dodge ValidGitRepositoryUrl (hostname form)...
	if got := fake.lastCreate["git_repository"]; got != "http://host.docker.internal:24123/oa-preview-m1.git" {
		t.Fatalf("create git_repository must be hostname form, got %v", got)
	}
	if got := fake.lastCreate["build_pack"]; got != "dockerfile" {
		t.Fatalf("runtime preview app must build from the tree's Dockerfile, got %v", got)
	}
	if got := fake.lastCreate["instant_deploy"]; got != false {
		t.Fatalf("instant_deploy must be false, got %v", got)
	}
	// ...and the PATCH must restore the real docker0-gateway URL plus the
	// port mapping (ports_exposes alone publishes nothing to the host).
	if got := fake.lastPatch["git_repository"]; got != "http://172.17.0.1:24123/oa-preview-m1.git" {
		t.Fatalf("patch git_repository must be the gateway URL, got %v", got)
	}
	if got := fake.lastPatch["ports_mappings"]; got != "32100:32100" {
		t.Fatalf("port mapping must publish the host slot, got %v", got)
	}
	if got := fake.lastPatch["limits_memory"]; got != "128m" {
		t.Fatalf("memory limit must ride the PATCH, got %v", got)
	}
	if got := fake.lastPatch["is_auto_deploy_enabled"]; got != false {
		t.Fatalf("auto deploy must be off, got %v", got)
	}
	if uuid != "app-1" {
		t.Fatalf("unexpected uuid %q", uuid)
	}
}

func TestCreateApplicationIsIdempotentOnPlaceholderKey(t *testing.T) {
	c, fake := newTestClient(t)
	ctx := context.Background()
	in := ApplicationInput{
		ProjectUUID: "proj", ServerUUID: "srv", EnvironmentName: "production",
		GitRepository: "http://172.17.0.1:24123/x.git", BuildPack: "dockerfile",
		PortsExposes: 1, LimitsMemory: "128m",
	}
	if _, err := c.CreateApplication(ctx, in); err != nil {
		t.Fatal(err)
	}
	// Second create must REUSE the key created by the first (no 409 path).
	if _, err := c.CreateApplication(ctx, in); err != nil {
		t.Fatalf("second create must reuse the placeholder key: %v", err)
	}
	if len(fake.keys) != 1 {
		t.Fatalf("exactly one placeholder key must exist, got %d", len(fake.keys))
	}
}

func TestGetDeploymentReturnsNilOn404(t *testing.T) {
	c, _ := newTestClient(t)
	ctx := context.Background()
	d, err := c.GetDeployment(ctx, "gone")
	if err != nil || d != nil {
		t.Fatalf("aged-out queue row must be (nil, nil), got (%v, %v)", d, err)
	}
}

func TestGetApplicationReturnsNilOn404(t *testing.T) {
	c, _ := newTestClient(t)
	ctx := context.Background()
	a, err := c.GetApplication(ctx, "nope")
	if err != nil || a != nil {
		t.Fatalf("unknown app must be (nil, nil), got (%v, %v)", a, err)
	}
}

func TestDeployReturnsDeploymentUUID(t *testing.T) {
	c, fake := newTestClient(t)
	ctx := context.Background()
	uuid, err := c.CreateApplication(ctx, ApplicationInput{
		ProjectUUID: "proj", ServerUUID: "srv", EnvironmentName: "production",
		GitRepository: "http://172.17.0.1:1/x.git", BuildPack: "dockerfile",
		PortsExposes: 1, LimitsMemory: "128m",
	})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := c.Deploy(ctx, uuid, true)
	if err != nil {
		t.Fatal(err)
	}
	if dep != "dep-1" {
		t.Fatalf("unexpected deployment uuid %q", dep)
	}
	d, err := c.GetDeployment(ctx, dep)
	if err != nil || d == nil || d.Status != "queued" {
		t.Fatalf("poll must see the queued row, got (%v, %v)", d, err)
	}
	_ = fake
}
