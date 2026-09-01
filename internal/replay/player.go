package replay

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// Player replays the out frames of a recorded script onto a stream,
// honoring `after` gates: a gated frame is emitted only after the bridge
// has sent a JSON-RPC request with the named method. When the script is
// exhausted the player STAYS ALIVE until its context is cancelled — the
// exit decision belongs to the bridge (design D3), and a shim that exits
// on its own would hide exactly the missed-termination defects the replay
// suite exists to catch.
type Player struct {
	script *Script
	in     *bufio.Scanner
	out    *bufio.Writer

	mu       sync.Mutex
	seen     map[string]bool
	gateChan chan struct{} // closed and replaced whenever a method arrives

	// err holds the first player error; checked by RunPlayer.
	err  error
	done chan struct{}
}

// RunPlayer replays script: outbound frames to out (raw JSON lines),
// inbound frames on in consumed only for gate matching (the bridge
// generates its own requests during replay — inbound script frames are
// the recording's record of what those were). Blocks until ctx is
// cancelled, even after the script is exhausted or stdin closes.
func RunPlayer(ctx context.Context, in io.Reader, out io.Writer, script *Script) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	p := &Player{
		script:   script,
		in:       scanner,
		out:      bufio.NewWriter(out),
		seen:     make(map[string]bool),
		gateChan: make(chan struct{}),
		done:     make(chan struct{}),
	}
	go func() {
		defer close(p.done)
		p.err = p.run(ctx)
	}()
	<-p.done
	return p.err
}

func (p *Player) run(ctx context.Context) error {
	// Inbound reader: every line's method (if any) satisfies gates.
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for p.in.Scan() {
			var probe struct {
				Method string `json:"method"`
			}
			if err := json.Unmarshal(p.in.Bytes(), &probe); err != nil || probe.Method == "" {
				continue
			}
			p.mu.Lock()
			p.seen[probe.Method] = true
			close(p.gateChan)
			p.gateChan = make(chan struct{})
			p.mu.Unlock()
		}
		// stdin EOF: deliberately NOT fatal. The real agent also survives
		// the bridge going away mid-session; termination is external.
	}()

	// Outbound player: script order, gated frames wait for their method.
	for _, fr := range p.script.Frames {
		if fr.Dir != DirectionOut {
			continue
		}
		if fr.After != "" {
			if err := p.waitGate(ctx, fr.After); err != nil {
				return err
			}
		}
		if _, err := p.out.Write(append([]byte(fr.Frame), '\n')); err != nil {
			return fmt.Errorf("replay player: write frame %d: %w", fr.Seq, err)
		}
		if err := p.out.Flush(); err != nil {
			return fmt.Errorf("replay player: flush frame %d: %w", fr.Seq, err)
		}
	}

	// Script exhausted: stay alive until cancelled. The bridge decides
	// when this process dies (PromptResponse stopReason -> session stop).
	<-ctx.Done()
	return nil
}

// waitGate blocks until the named inbound method has been seen or ctx is
// cancelled. Uses the replace-closed-channel broadcast pattern.
func (p *Player) waitGate(ctx context.Context, method string) error {
	for {
		p.mu.Lock()
		if p.seen[method] {
			p.mu.Unlock()
			return nil
		}
		ch := p.gateChan
		p.mu.Unlock()

		select {
		case <-ch:
			// re-check loop
		case <-ctx.Done():
			return fmt.Errorf("replay player: cancelled waiting for gate %q", method)
		}
	}
}
