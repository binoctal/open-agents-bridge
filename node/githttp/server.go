// Package githttp serves bare repos over git's own http-backend CGI, one
// listener per deployment (design D1 / task 2.3).
//
// Listeners bind the docker0 gateway (default 172.17.0.1), NOT loopback:
// Coolify's git fetch runs inside the coolify-helper container, which can
// neither resolve host.docker.internal nor reach the host's 127.0.0.1 -- the
// gateway IP is the only address shared between the helper's network and the
// host (S3 spike). Off-host the ports are unroutable and the node firewall
// admits only the Coolify container subnet to the git port range
// (spec: 回环外不可达, enforced at the firewall layer instead).
// Ports come from a configured range and are recycled on
// release: the allocator always scans from the range start, so a freed port
// is the first one handed out again.
package githttp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Manager owns the per-deployment listeners.
type Manager struct {
	RepoRoot string
	// BindHost is the address every listener binds (see the package comment:
	// the docker0 gateway in production, 127.0.0.1 in tests/local dev).
	BindHost string
	PortMin  int
	PortMax  int

	mu      sync.Mutex
	serving map[string]*served
}

type served struct {
	port int
	srv  *http.Server
	ln   net.Listener
}

// New builds a manager serving {RepoRoot}/{id}.git from bindHost over the
// port range.
func New(repoRoot, bindHost string, portMin, portMax int) *Manager {
	return &Manager{
		RepoRoot: repoRoot,
		BindHost: bindHost,
		PortMin:  portMin,
		PortMax:  portMax,
		serving:  make(map[string]*served),
	}
}

// Serve starts (or returns the already-running) listener for a
// deployment's repo. Idempotent: serving an id twice returns the same port.
func (m *Manager) Serve(deploymentID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.serving[deploymentID]; ok {
		return s.port, nil
	}
	if deploymentID == "" || strings.ContainsAny(deploymentID, `/\.`) {
		return 0, fmt.Errorf("githttp: invalid deployment id %q", deploymentID)
	}
	ln, port, err := m.listenFree()
	if err != nil {
		return 0, err
	}
	srv := &http.Server{Handler: m.handler()}
	go func() { _ = srv.Serve(ln) }()
	m.serving[deploymentID] = &served{port: port, srv: srv, ln: ln}
	return port, nil
}

// Release stops the listener and recycles the port (absence is success).
func (m *Manager) Release(deploymentID string) error {
	m.mu.Lock()
	s, ok := m.serving[deploymentID]
	delete(m.serving, deploymentID)
	m.mu.Unlock()
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	// Close the raw listener too: if Shutdown races the serve goroutine's
	// first registration (Serve not started yet), Shutdown closes nothing and
	// the port would stay bound until the goroutine wakes up. When Shutdown
	// loses that race the other way (listener already registered, then we
	// closed it), its poll reports the very close we caused -- that benign
	// error is success here, not a failure.
	_ = s.ln.Close()
	if err := s.srv.Shutdown(ctx); err != nil &&
		!errors.Is(err, http.ErrServerClosed) &&
		!strings.Contains(err.Error(), "use of closed network connection") {
		return err
	}
	return nil
}

// Port reports the port a deployment is served on, if any.
func (m *Manager) Port(deploymentID string) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.serving[deploymentID]
	if !ok {
		return 0, false
	}
	return s.port, true
}

const closeTimeout = 5 * time.Second

// listenFree scans the range from the start and takes the first port that
// binds -- which is what makes released ports recycle immediately.
func (m *Manager) listenFree() (net.Listener, int, error) {
	for port := m.PortMin; port <= m.PortMax; port++ {
		ln, err := net.Listen("tcp", net.JoinHostPort(m.BindHost, strconv.Itoa(port)))
		if err == nil {
			return ln, port, nil
		}
	}
	return nil, 0, fmt.Errorf("githttp: no free port in [%d,%d]", m.PortMin, m.PortMax)
}

// handler adapts HTTP requests to one `git http-backend` CGI invocation.
// GIT_PROJECT_ROOT + PATH_INFO make http-backend resolve the repo, and
// GIT_HTTP_EXPORT_ALL bypasses per-repo daemons-permission files.
func (m *Manager) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only ever this agent's repos; no path can escape RepoRoot because
		// http-backend resolves GIT_PROJECT_ROOT+PATH_INFO itself and we
		// reject anything that is not a plain /{id}.git/... shape.
		if !validGitPath(r.URL.Path) {
			http.NotFound(w, r)
			return
		}

		cmd := exec.CommandContext(r.Context(), "git", "http-backend")
		cmd.Env = []string{
			"GIT_PROJECT_ROOT=" + m.RepoRoot,
			"GIT_HTTP_EXPORT_ALL=1",
			"REQUEST_METHOD=" + r.Method,
			"PATH_INFO=" + r.URL.Path,
			"QUERY_STRING=" + r.URL.RawQuery,
			"REMOTE_ADDR=" + r.RemoteAddr,
			"REMOTE_USER=hosting-agent",
			"CONTENT_TYPE=" + r.Header.Get("Content-Type"),
			"PATH=" + os.Getenv("PATH"),
		}
		if r.Method == http.MethodPost {
			cmd.Env = append(cmd.Env, "CONTENT_LENGTH="+strconv.FormatInt(r.ContentLength, 10))
			cmd.Stdin = r.Body
		}

		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			http.Error(w, "git http-backend failed", http.StatusInternalServerError)
			return
		}

		// Parse the CGI response: header lines until the first blank line,
		// then the body. "Status:" is CGI's status field; every other header
		// maps straight through.
		br := bufio.NewReader(&out)
		status := http.StatusOK
		header := w.Header()
		for {
			line, err := br.ReadString('\n')
			if err != nil || strings.TrimSpace(line) == "" {
				break
			}
			name, val, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			name, val = strings.TrimSpace(name), strings.TrimSpace(val)
			if strings.EqualFold(name, "Status") {
				if code, _, found := strings.Cut(val, " "); found {
					if n, err := strconv.Atoi(code); err == nil {
						status = n
					}
				}
				continue
			}
			header.Set(name, val)
		}
		w.WriteHeader(status)
		_, _ = io.Copy(w, br)
	})
}

// validGitPath accepts only /{id}.git and subpaths below it.
func validGitPath(p string) bool {
	p = strings.TrimPrefix(p, "/")
	id, rest, ok := strings.Cut(p, "/")
	if !ok {
		// "/{id}.git" with no subpath: http-backend never sees this shape from
		// a real git client; reject.
		return false
	}
	if id == "" || !strings.HasSuffix(id, ".git") {
		return false
	}
	base := strings.TrimSuffix(id, ".git")
	if base == "" || strings.ContainsAny(base, "/.\\") || strings.Contains(base, "..") {
		return false
	}
	_ = rest
	return true
}
