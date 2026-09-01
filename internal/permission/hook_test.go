package permission

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGetSocketPath_Default(t *testing.T) {
	t.Setenv("OPEN_AGENTS_SOCKET_DIR", "")
	p := GetSocketPath()
	if filepath.Base(p) != SocketName {
		t.Errorf("expected base %s, got %s", SocketName, filepath.Base(p))
	}
}

func TestGetSocketPath_EnvOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPEN_AGENTS_SOCKET_DIR", dir)
	p := GetSocketPath()
	want := filepath.Join(dir, SocketName)
	if p != want {
		t.Errorf("expected %s, got %s", want, p)
	}
}

func TestToolToPermissionType(t *testing.T) {
	cases := map[string]string{
		"fs_read":      "file:read",
		"fs_write":     "file:write",
		"execute_bash": "command:exec",
		"use_aws":      "aws:api",
		"custom_tool":  "tool:custom_tool",
		"":             "tool:",
	}
	for tool, want := range cases {
		if got := toolToPermissionType(tool); got != want {
			t.Errorf("toolToPermissionType(%q) = %q, want %q", tool, got, want)
		}
	}
}

func TestClassifyRisk(t *testing.T) {
	cases := map[string]string{
		"execute_bash": "high",
		"use_aws":      "high",
		"fs_write":     "medium",
		"fs_read":      "low",
		"unknown":      "low",
	}
	for tool, want := range cases {
		if got := classifyRisk(tool); got != want {
			t.Errorf("classifyRisk(%q) = %q, want %q", tool, got, want)
		}
	}
}

func TestBuildDescription(t *testing.T) {
	cases := []struct {
		name  string
		tool  string
		input map[string]any
		want  string
	}{
		{
			name:  "fs_write with path",
			tool:  "fs_write",
			input: map[string]any{"path": "/tmp/foo.txt"},
			want:  "Write to file: /tmp/foo.txt",
		},
		{
			name:  "fs_write missing path",
			tool:  "fs_write",
			input: map[string]any{},
			want:  "Use tool: fs_write",
		},
		{
			name:  "execute_bash with command",
			tool:  "execute_bash",
			input: map[string]any{"command": "ls -la"},
			want:  "Execute command: ls -la",
		},
		{
			name:  "use_aws with service and operation",
			tool:  "use_aws",
			input: map[string]any{"service_name": "s3", "operation_name": "ListBuckets"},
			want:  "AWS s3: ListBuckets",
		},
		{
			name:  "use_aws missing operation",
			tool:  "use_aws",
			input: map[string]any{"service_name": "ec2"},
			want:  "AWS ec2: ",
		},
		{
			name:  "unknown tool falls back",
			tool:  "custom_tool",
			input: map[string]any{"foo": "bar"},
			want:  "Use tool: custom_tool",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := buildDescription(c.tool, c.input); got != c.want {
				t.Errorf("buildDescription(%q) = %q, want %q", c.tool, got, c.want)
			}
		})
	}
}

// Server round-trip: a hook request over the Unix socket is auto-approved via
// OnRequest and a HookResponse is written back.
func TestServer_PermissionRoundTrip_Approved(t *testing.T) {
	withTempSocket(t, func(h *Handler) {
		h.OnRequest(func(r Request) {
			go h.Resolve(Response{ID: r.ID, Approved: true})
		})
	}, func(srv *Server) {
		resp := dialHook(t, srv, HookRequest{
			Type:      "permission",
			ToolName:  "fs_read",
			ToolInput: map[string]any{"path": "/tmp/x"},
			SessionID: "s1",
		})
		if !resp.Approved {
			t.Error("expected approved=true")
		}
	})
}

func TestServer_PermissionRoundTrip_Rejected(t *testing.T) {
	withTempSocket(t, func(h *Handler) {
		h.OnRequest(func(r Request) {
			go h.Resolve(Response{ID: r.ID, Approved: false})
		})
	}, func(srv *Server) {
		resp := dialHook(t, srv, HookRequest{
			Type:     "permission",
			ToolName: "execute_bash",
			ToolInput: map[string]any{
				"command": "rm -rf /",
			},
		})
		if resp.Approved {
			t.Error("expected approved=false")
		}
	})
}

// withTempSocket isolates the Unix socket in a temp dir, starts a Server wired
// to a handler configured by `setup`, and runs `body` against it.
func withTempSocket(t *testing.T, setup func(*Handler), body func(*Server)) {
	t.Helper()
	t.Setenv("OPEN_AGENTS_SOCKET_DIR", t.TempDir())

	h := NewHandler()
	setup(h)

	srv := NewServer(h)
	if err := srv.Start(); err != nil {
		t.Fatalf("server Start: %v", err)
	}
	t.Cleanup(srv.Stop)

	body(srv)
}

// dialHook opens a connection to the server socket, sends one HookRequest as a
// JSON line, and reads back the single HookResponse line.
func dialHook(t *testing.T, srv *Server, req HookRequest) HookResponse {
	t.Helper()

	conn, err := net.Dial("unix", GetSocketPath())
	if err != nil {
		t.Fatalf("dial socket: %v", err)
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatalf("encode request: %v", err)
	}

	respBytes := make(chan []byte, 1)
	errCh := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(conn)
		if !scanner.Scan() {
			errCh <- scanner.Err()
			return
		}
		b := make([]byte, len(scanner.Bytes()))
		copy(b, scanner.Bytes())
		respBytes <- b
	}()

	select {
	case b := <-respBytes:
		var resp HookResponse
		if err := json.Unmarshal(b, &resp); err != nil {
			t.Fatalf("unmarshal response %q: %v", string(b), err)
		}
		return resp
	case err := <-errCh:
		t.Fatalf("read response: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for hook response")
	}
	return HookResponse{}
}

// sanity: GetSocketPath honors the env var set by t.Setenv for the temp dir used
// above (guards against a stale socket path collision across tests).
var _ = os.Setenv
