// Package replay defines the recorded-session script format used by the
// bridge replay test suite (OpenSpec add-replay-testing). A script is a
// JSONL file: one header entry followed by frame entries. Recording and
// replay are symmetric — both sides operate on raw wire frames as the
// ACP adapter sees them on stdio, so turn-end metadata such as stopReason
// survives verbatim.
package replay

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
)

// Entry kinds appearing as the "kind" discriminator on every script line.
const (
	KindHeader = "header"
	KindFrame  = "frame"
)

// Header is the provenance record on the first line of every script. The
// replay fixtures are living artifacts: this metadata answers "which
// package upgrade should trigger a re-record" without archaeology.
type Header struct {
	Kind           string `json:"kind"`
	CLIType        string `json:"cliType"`
	AdapterVersion string `json:"adapterVersion"`
	RecordedAt     string `json:"recordedAt"`
	Recipe         string `json:"recipe,omitempty"` // e2e recipe reference, README key
}

// Direction of a recorded frame relative to the CLI process.
type Direction string

const (
	// DirectionIn is bridge -> CLI (written to the CLI's stdin).
	DirectionIn Direction = "in"
	// DirectionOut is CLI -> bridge (read from the CLI's stdout).
	DirectionOut Direction = "out"
)

// Frame is one recorded wire frame. Out frames may declare After, a gate
// naming an inbound JSON-RPC method that must be received before the
// frame is emitted during replay; this keeps the handshake order
// (initialize -> session/new -> session/prompt) stable across replays.
// AfterCount (default 1) makes the gate wait for the Nth occurrence of
// the method — mid-task interactions replay as: first prompt -> partial
// output, second prompt (the user's injected answer) -> continuation.
type Frame struct {
	Kind       string          `json:"kind"`
	Seq        int             `json:"seq"`
	Dir        Direction       `json:"dir"`
	Frame      json.RawMessage `json:"frame"`
	After      string          `json:"after,omitempty"`
	AfterCount int             `json:"afterCount,omitempty"`
}

// Script is a parsed replay script.
type Script struct {
	Header Header
	Frames []Frame
}

// LoadScript reads and validates a script file.
func LoadScript(path string) (*Script, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("replay: open script: %w", err)
	}
	defer f.Close()

	var script Script
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		var probe struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &probe); err != nil {
			return nil, fmt.Errorf("replay: line %d: not JSON: %w", line, err)
		}
		switch probe.Kind {
		case KindHeader:
			if line != 1 {
				return nil, fmt.Errorf("replay: line %d: header must be the first entry", line)
			}
			if err := json.Unmarshal(scanner.Bytes(), &script.Header); err != nil {
				return nil, fmt.Errorf("replay: line %d: bad header: %w", line, err)
			}
		case KindFrame:
			if line == 1 {
				return nil, fmt.Errorf("replay: line 1: first entry must be a header")
			}
			var fr Frame
			if err := json.Unmarshal(scanner.Bytes(), &fr); err != nil {
				return nil, fmt.Errorf("replay: line %d: bad frame: %w", line, err)
			}
			if fr.Dir != DirectionIn && fr.Dir != DirectionOut {
				return nil, fmt.Errorf("replay: line %d: frame %d: invalid direction %q", line, fr.Seq, fr.Dir)
			}
			if fr.After != "" && fr.Dir != DirectionOut {
				return nil, fmt.Errorf("replay: line %d: frame %d: after gate is only valid on out frames", line, fr.Seq)
			}
			if fr.Seq != len(script.Frames) {
				return nil, fmt.Errorf("replay: line %d: frame seq %d, want %d (must be dense and ordered)", line, fr.Seq, len(script.Frames))
			}
			script.Frames = append(script.Frames, fr)
		default:
			return nil, fmt.Errorf("replay: line %d: unknown kind %q", line, probe.Kind)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("replay: read script: %w", err)
	}
	if line == 0 {
		return nil, fmt.Errorf("replay: empty script")
	}
	if script.Header.Kind != KindHeader {
		return nil, fmt.Errorf("replay: missing header entry")
	}
	return &script, nil
}
