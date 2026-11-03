package compose

import (
	"strings"
	"testing"

	"github.com/binoctal/open-agents/hosting-agent/internal/api"
)

// specFor builds the rewrite spec for the canonical fixture: web is the
// ingress, worker an active profile-gated sidecar (the platform activated
// "back"), gpu an inactive-profile service.
func specFor() api.ComposeSpec {
	return api.ComposeSpec{
		ComposePath:      "docker-compose.yml",
		IngressService:   "web",
		IngressPort:      3000,
		ActivateProfiles: []string{"back"},
		HostPort:         30001,
		Services: []api.ServiceLimit{
			{Name: "web", MemoryMiB: 512, CPUs: 1},
			{Name: "worker", MemoryMiB: 256, CPUs: 0.5},
		},
	}
}

const canonicalYAML = `
version: "3.8"
services:
  web:
    image: ghcr.io/example/web:1.2.3
    ports:
      - "8080:3000"
    environment:
      - FOO=bar
    deploy:
      resources:
        limits:
          memory: 999m
  worker:
    image: ghcr.io/example/worker:1.2.3
    profiles: ["back"]
    ports:
      - "9090:9090"
    volumes:
      - data:/var/lib/data
  gpu:
    image: nvidia/cuda:12
    profiles: ["gpu"]
    ports:
      - "4000:4000"
volumes:
  data: {}
`

func TestRewrite_ActivatesStripsPortsInjectsLimits(t *testing.T) {
	out, err := Rewrite([]byte(canonicalYAML), specFor())
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)

	// Activation: the two active services lose their profiles key; the
	// inactive one keeps it so plain `docker compose up -d` never starts it.
	if strings.Contains(got, "back") {
		t.Errorf("active service's profiles key must be stripped:\n%s", got)
	}
	if !strings.Contains(got, "gpu:") {
		t.Errorf("inactive service must stay in the file:\n%s", got)
	}
	if !strings.Contains(got, "profiles") {
		t.Errorf("inactive service must keep its profiles key:\n%s", got)
	}

	// Ingress port rewrite: loopback-bound negotiated slot only.
	if !strings.Contains(got, `"127.0.0.1:30001:3000"`) && !strings.Contains(got, "127.0.0.1:30001:3000") {
		t.Errorf("ingress ports must be the negotiated loopback slot:\n%s", got)
	}
	if strings.Contains(got, "8080") {
		t.Errorf("original ingress host port must be gone:\n%s", got)
	}
	// Non-ingress active service loses host ports entirely.
	if strings.Contains(got, "9090") {
		t.Errorf("non-ingress host ports must be stripped:\n%s", got)
	}
	// Inactive service ports are left alone (never runs).
	if !strings.Contains(got, "4000") {
		t.Errorf("inactive service must keep its own ports:\n%s", got)
	}

	// Limits: authoritative post-min values replace the user's 999m.
	if !strings.Contains(got, "cpus: 1") {
		t.Errorf("web cpus limit not injected:\n%s", got)
	}
	if !strings.Contains(got, "cpus: 0.5") {
		t.Errorf("worker cpus limit not injected:\n%s", got)
	}
	if !strings.Contains(got, `memory: "536870912"`) {
		t.Errorf("web memory limit (512MiB in bytes) not injected:\n%s", got)
	}
	if !strings.Contains(got, `memory: "268435456"`) {
		t.Errorf("worker memory limit (256MiB in bytes) not injected:\n%s", got)
	}
	if strings.Contains(got, "999") {
		t.Errorf("user-declared memory limit must be overwritten:\n%s", got)
	}

	// Named volume and top-level structure survive.
	if !strings.Contains(got, "data") {
		t.Errorf("declared volume must survive the rewrite:\n%s", got)
	}
}

func TestRewrite_IsDeterministic(t *testing.T) {
	a, err := Rewrite([]byte(canonicalYAML), specFor())
	if err != nil {
		t.Fatal(err)
	}
	b, err := Rewrite([]byte(canonicalYAML), specFor())
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Error("same input must rewrite to identical bytes (deterministic commit)")
	}
}

func TestRewrite_MergesLimitsIntoExistingDeployTree(t *testing.T) {
	// The user declared a sibling limit key (pids): legal (the table only
	// reads cpus/memory), preserved; the rewrite merges into the same map.
	// deploy.resources.reservations is NOT legal per the table -- that case
	// lives in the rejection matrix below.
	src := `
services:
  web:
    image: nginx
    deploy:
      resources:
        limits:
          pids: 64
`
	out, err := Rewrite([]byte(src), func() api.ComposeSpec {
		s := specFor()
		s.Services = []api.ServiceLimit{{Name: "web", MemoryMiB: 512, CPUs: 1}}
		return s
	}())
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, "pids: 64") {
		t.Errorf("sibling limit key must be preserved:\n%s", got)
	}
	if !strings.Contains(got, `memory: "536870912"`) {
		t.Errorf("memory must be injected into the existing limits map:\n%s", got)
	}
}

func TestRewrite_AliasAnchorsDoNotCrossContaminate(t *testing.T) {
	// worker: *web makes both keys decode to the same config. The rewrite
	// must treat them as independent services with independent limits.
	src := `
services:
  web: &base
    image: nginx
    ports: ["8080:3000"]
  worker: *base
`
	spec := specFor()
	spec.Services = []api.ServiceLimit{
		{Name: "web", MemoryMiB: 512, CPUs: 1},
		{Name: "worker", MemoryMiB: 4096, CPUs: 2},
	}
	out, err := Rewrite([]byte(src), spec)
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.Contains(got, `memory: "536870912"`) {
		t.Errorf("web must keep its own limit:\n%s", got)
	}
	if !strings.Contains(got, `memory: "4294967296"`) {
		t.Errorf("worker must get its own limit, not web's:\n%s", got)
	}
	// web is the ingress (ports rewritten); worker must have NO ports.
	webBlock := got[strings.Index(got, "web:"):strings.Index(got, "worker:")]
	if !strings.Contains(webBlock, "127.0.0.1:30001:3000") {
		t.Errorf("ingress service must get the negotiated slot:\n%s", webBlock)
	}
	if strings.Contains(got[strings.Index(got, "worker:"):], "ports") {
		t.Errorf("non-ingress alias must have its ports stripped:\n%s", got)
	}
}

func TestRewrite_RejectsDriftedOrDangerousDocuments(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"privileged", `
services:
  web: {image: nginx, privileged: true}
`, "privileged mode"},
		{"host network", `
services:
  web: {image: nginx, network_mode: host}
`, "network_mode"},
		{"host bind short", `
services:
  web: {image: nginx, volumes: ["/host/data:/data"]}
`, "host path bind mounts"},
		{"host bind relative", `
services:
  web: {image: nginx, volumes: ["./data:/data"]}
`, "host path bind mounts"},
		{"undeclared volume", `
services:
  web: {image: nginx, volumes: ["ghost:/data"]}
`, "is not declared in top-level volumes"},
		{"anonymous volume", `
services:
  web: {image: nginx, volumes: ["/data"]}
`, "anonymous volumes"},
		{"external volume", `
services:
  web: {image: nginx, volumes: ["data:/data"]}
volumes:
  data: {external: true}
`, "external volumes"},
		{"volume driver", `
services:
  web: {image: nginx, volumes: ["data:/data"]}
volumes:
  data: {driver: nfs}
`, `driver "nfs" is not allowed`},
		{"gpu reservation", `
services:
  web:
    image: nginx
    deploy:
      resources:
        reservations:
          devices: [{driver: nvidia}]
`, "device (GPU) reservations"},
		{"plain reservations", `
services:
  web:
    image: nginx
    deploy:
      resources:
        reservations:
          cpus: 0.25
`, "only deploy.resources.limits is supported"},
		{"placement key", `
services:
  web:
    image: nginx
    deploy:
      placement: {constraints: [node.role == manager]}
`, "only deploy.resources.limits is supported"},
		{"unsupported service key", `
services:
  web: {image: nginx, ulimits: {nproc: 65535}}
`, `unsupported service key "ulimits"`},
		{"unsupported top key", `
x-common: &c {image: nginx}
services:
  web: *c
`, `unsupported top-level key "x-common"`},
		{"missing active service", `
services:
  web: {image: nginx}
`, `active service "worker" missing`},
		{"activation drift (ingress gated off)", `
services:
  web: {image: nginx, profiles: [never]}
`, `activation drift: service "web" is in the spec but not activated by the file`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Rewrite([]byte(tc.src), specFor())
			if err == nil {
				t.Fatalf("must reject, got success")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q must mention %q", err, tc.want)
			}
		})
	}
}

func TestRewrite_RejectsUnparseableDocuments(t *testing.T) {
	if _, err := Rewrite([]byte("services: [oops"), specFor()); err == nil {
		t.Error("unparseable yaml must fail, not rewrite")
	}
	if _, err := Rewrite([]byte("- a\n- b\n"), specFor()); err == nil {
		t.Error("a sequence document must fail")
	}
	if _, err := Rewrite([]byte(""), specFor()); err == nil {
		t.Error("an empty document must fail")
	}
}
