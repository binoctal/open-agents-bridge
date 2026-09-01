package replay

import (
	"bufio"
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// lineChan adapts a reader into a channel of lines with no artificial
// timeout: a missing line must surface as the caller's own deadline firing,
// not as a read error.
func lineChan(r io.Reader) <-chan string {
	ch := make(chan string)
	go func() {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			ch <- sc.Text()
		}
	}()
	return ch
}

func loadOrFatal(t *testing.T, lines ...string) *Script {
	t.Helper()
	s, err := LoadScript(writeScript(t, lines...))
	if err != nil {
		t.Fatalf("LoadScript: %v", err)
	}
	return s
}

// Spec scenario "Handshake order is gated": the initialize response plays
// immediately, but the session/new and session/prompt responses play only
// after the bridge actually sends those requests — handshake order stays
// stable across replays.
func TestPlayerGatesOutboundFramesOnInboundMethods(t *testing.T) {
	script := loadOrFatal(t,
		`{"kind":"header","cliType":"claude"}`,
		`{"kind":"frame","seq":0,"dir":"in","frame":{"id":"bridge_1","method":"initialize"}}`,
		`{"kind":"frame","seq":1,"dir":"out","frame":{"id":"bridge_1","result":{"protocolVersion":1,"agentInfo":{"name":"t","version":"1"}}}}`,
		`{"kind":"frame","seq":2,"dir":"in","frame":{"id":"bridge_2","method":"session/new"}}`,
		`{"kind":"frame","seq":3,"dir":"out","after":"session/new","frame":{"id":"bridge_2","result":{"sessionId":"s1"}}}`,
		`{"kind":"frame","seq":4,"dir":"in","frame":{"id":"bridge_3","method":"session/prompt"}}`,
		`{"kind":"frame","seq":5,"dir":"out","after":"session/prompt","frame":{"id":"bridge_3","result":{"stopReason":"end_turn"}}}`,
	)

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	defer stdinW.Close()
	defer stdoutW.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { RunPlayer(ctx, stdinR, stdoutW, script) }()

	lines := lineChan(stdoutR)

	// Ungated initialize response plays without any bridge input.
	select {
	case l := <-lines:
		if !strings.Contains(l, "agentInfo") {
			t.Fatalf("first out frame = %s, want initialize response", l)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("initialize response never played")
	}

	// session/new response must NOT arrive before its request.
	select {
	case l := <-lines:
		t.Fatalf("session/new response leaked before the request: %s", l)
	case <-time.After(150 * time.Millisecond):
	}

	stdinW.Write([]byte(`{"id":"bridge_2","method":"session/new"}` + "\n"))
	select {
	case l := <-lines:
		if !strings.Contains(l, "sessionId") {
			t.Fatalf("second out frame = %s, want session/new response", l)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session/new response never played after its gate was satisfied")
	}

	stdinW.Write([]byte(`{"id":"bridge_3","method":"session/prompt"}` + "\n"))
	select {
	case l := <-lines:
		if !strings.Contains(l, "end_turn") {
			t.Fatalf("third out frame = %s, want session/prompt response", l)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("session/prompt response never played after its gate was satisfied")
	}
}

// Spec scenario "Shim waits for bridge-driven termination": with the script
// exhausted AND stdin at EOF the player must not return on its own — the
// exit decision belongs to the bridge (design D3). A player that exits here
// would let the death-reporting path mask missed-termination defects.
func TestPlayerStaysAliveAfterScriptExhausted(t *testing.T) {
	script := loadOrFatal(t,
		`{"kind":"header","cliType":"claude"}`,
		`{"kind":"frame","seq":0,"dir":"out","frame":{"id":"bridge_1","result":{"stopReason":"end_turn"}}}`,
	)

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	stdinW.Close() // EOF immediately: the script's only frame is ungated.
	defer stdoutW.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunPlayer(ctx, stdinR, stdoutW, script) }()

	select {
	case l := <-lineChan(stdoutR):
		if !strings.Contains(l, "end_turn") {
			t.Fatalf("out frame = %s", l)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("out frame never played")
	}

	// Both script exhausted and stdin EOF: still alive.
	select {
	case err := <-done:
		t.Fatalf("player returned on its own (%v); termination must be external", err)
	case <-time.After(300 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("player ignored cancellation")
	}
}
