package ingress

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeAdmin is a minimal Caddy admin API: it records routes by @id, PATCH
// replaces in place (404 when new), PUT inserts at index 0, DELETE removes
// (404 when absent). It counts method usage so idempotency is assertable.
type fakeAdmin struct {
	mu     sync.Mutex
	routes map[string]map[string]any
	order  []string
	calls  map[string]int
}

func newFakeAdmin() *fakeAdmin {
	return &fakeAdmin{routes: map[string]map[string]any{}, calls: map[string]int{}}
}

func (f *fakeAdmin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[r.Method]++

	body := map[string]any{}
	if r.Body != nil {
		raw, _ := io.ReadAll(r.Body)
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &body)
		}
	}

	// Path shapes the client uses: /id/{id} and /config/.../routes/0.
	rest := strings.TrimPrefix(r.URL.Path, "/id/")
	switch {
	case strings.HasPrefix(r.URL.Path, "/id/"):
		id := rest
		switch r.Method {
		case http.MethodPatch:
			if _, ok := f.routes[id]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			f.routes[id] = body
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			if _, ok := f.routes[id]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			delete(f.routes, id)
			for i, x := range f.order {
				if x == id {
					f.order = append(f.order[:i], f.order[i+1:]...)
					break
				}
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	case strings.HasPrefix(r.URL.Path, "/config/"):
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		id, _ := body["@id"].(string)
		if _, ok := f.routes[id]; !ok {
			f.order = append([]string{id}, f.order...) // insert at index 0
		}
		f.routes[id] = body
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeAdmin) method(n string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[n]
}

func (f *fakeAdmin) has(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.routes[id]
	return ok
}

func newTestCaddy(t *testing.T) (*Caddy, *fakeAdmin) {
	t.Helper()
	admin := newFakeAdmin()
	srv := httptest.NewServer(admin)
	t.Cleanup(srv.Close)
	return NewCaddy(strings.TrimPrefix(srv.URL, "http://"), "deploy.openagents.top"), admin
}

func TestUpsertNewRouteInsertsThenPatches(t *testing.T) {
	c, admin := newTestCaddy(t)
	ctx := context.Background()

	if err := c.Upsert(ctx, "dep1", "127.0.0.1:3000"); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if !admin.has("oa-dep1") {
		t.Fatal("route must exist after first upsert")
	}
	if got := admin.method(http.MethodPut); got != 1 {
		t.Fatalf("new route must be inserted with exactly one PUT, got %d", got)
	}

	// Second upsert of the same deployment must take the PATCH-in-place path.
	if err := c.Upsert(ctx, "dep1", "127.0.0.1:3001"); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if got := admin.method(http.MethodPut); got != 1 {
		t.Fatalf("idempotent upsert must not PUT again, got %d PUTs", got)
	}
	// The first upsert's 404 PATCH probe + this successful PATCH = 2.
	if got := admin.method(http.MethodPatch); got != 2 {
		t.Fatalf("existing route must be PATCHed in place, got %d PATCHes", got)
	}
}

func TestRemoveIsIdempotent(t *testing.T) {
	c, admin := newTestCaddy(t)
	ctx := context.Background()

	if err := c.Upsert(ctx, "dep2", "127.0.0.1:3002"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := c.Remove(ctx, "dep2"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if admin.has("oa-dep2") {
		t.Fatal("route must be gone after remove")
	}
	// Removing an absent route is success (absence = the desired state).
	if err := c.Remove(ctx, "dep2"); err != nil {
		t.Fatalf("remove of absent route must succeed, got %v", err)
	}
}

func TestRemoveDoesNotTouchNeighborRoutes(t *testing.T) {
	c, admin := newTestCaddy(t)
	ctx := context.Background()
	if err := c.Upsert(ctx, "keep", "127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Upsert(ctx, "drop", "127.0.0.1:2"); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(ctx, "drop"); err != nil {
		t.Fatal(err)
	}
	if !admin.has("oa-keep") {
		t.Fatal("removing one route must not affect its neighbors")
	}
}
