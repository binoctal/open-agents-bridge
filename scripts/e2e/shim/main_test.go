package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeState(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveScriptSingleMode(t *testing.T) {
	t.Setenv("OA_REPLAY_SCRIPT", "/tmp/single.jsonl")
	t.Setenv("OA_REPLAY_SCRIPT_SEQ", "")
	t.Setenv("OA_REPLAY_STATE", "")

	got, err := resolveScript()
	if err != nil {
		t.Fatalf("resolveScript: %v", err)
	}
	if got != "/tmp/single.jsonl" {
		t.Fatalf("got %q, want /tmp/single.jsonl", got)
	}
}

func TestResolveScriptRequiresEnv(t *testing.T) {
	t.Setenv("OA_REPLAY_SCRIPT", "")
	t.Setenv("OA_REPLAY_SCRIPT_SEQ", "")

	if _, err := resolveScript(); err == nil {
		t.Fatal("expected error when no script env is set")
	}
}

func TestResolveScriptSeqWalksListAndWraps(t *testing.T) {
	state := writeState(t, "0")
	t.Setenv("OA_REPLAY_SCRIPT", "")
	t.Setenv("OA_REPLAY_SCRIPT_SEQ", "/a.jsonl,/b.jsonl")
	t.Setenv("OA_REPLAY_STATE", state)

	want := []string{"/a.jsonl", "/b.jsonl", "/a.jsonl"}
	for _, w := range want {
		got, err := resolveScript()
		if err != nil {
			t.Fatalf("resolveScript: %v", err)
		}
		if got != w {
			t.Fatalf("got %q, want %q", got, w)
		}
	}
	// The counter advanced exactly len(want) times.
	data, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "3" {
		t.Fatalf("state file = %q, want \"3\"", data)
	}
}

func TestResolveScriptSeqRequiresState(t *testing.T) {
	t.Setenv("OA_REPLAY_SCRIPT", "")
	t.Setenv("OA_REPLAY_SCRIPT_SEQ", "/a.jsonl")
	t.Setenv("OA_REPLAY_STATE", "")

	if _, err := resolveScript(); err == nil {
		t.Fatal("expected error when SEQ is set but STATE is missing")
	}
}

func TestResolveScriptSeqRejectsMissingStateFile(t *testing.T) {
	// A missing state file must be a hard error, not a silent restart at 0:
	// mid-mission the sequence position IS the fixture selection.
	t.Setenv("OA_REPLAY_SCRIPT", "")
	t.Setenv("OA_REPLAY_SCRIPT_SEQ", "/a.jsonl,/b.jsonl")
	t.Setenv("OA_REPLAY_STATE", filepath.Join(t.TempDir(), "does-not-exist"))

	if _, err := resolveScript(); err == nil {
		t.Fatal("expected error when the state file does not exist")
	}
}

func TestResolveScriptSeqRejectsGarbageCounter(t *testing.T) {
	state := writeState(t, "not-a-number")
	t.Setenv("OA_REPLAY_SCRIPT", "")
	t.Setenv("OA_REPLAY_SCRIPT_SEQ", "/a.jsonl")
	t.Setenv("OA_REPLAY_STATE", state)

	if _, err := resolveScript(); err == nil {
		t.Fatal("expected error for unparseable counter")
	}
}
