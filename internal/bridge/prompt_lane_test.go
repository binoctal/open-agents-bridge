package bridge

import (
	"sync"
	"testing"
	"time"
)

func laneMsg(sessionID string, n int) Message {
	return Message{Type: "chat:send", Payload: map[string]interface{}{"sessionId": sessionID, "n": n}}
}

// A prompt waiting on a busy turn must not stall the caller (the shared
// messageWorker) nor another session's prompts.
func TestEnqueuePromptDoesNotBlockCallerOrOtherSessions(t *testing.T) {
	b := &Bridge{}
	release := make(chan struct{})
	otherRan := make(chan struct{})

	returned := make(chan struct{})
	go func() {
		b.enqueuePrompt(laneMsg("s1", 1), func(Message) { <-release })
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("enqueuePrompt blocked the caller while the prompt handler was busy")
	}

	b.enqueuePrompt(laneMsg("s2", 1), func(Message) { close(otherRan) })
	select {
	case <-otherRan:
	case <-time.After(time.Second):
		t.Fatal("a busy session's prompt blocked another session")
	}
	close(release)
}

func TestEnqueuePromptKeepsPerSessionOrder(t *testing.T) {
	b := &Bridge{}
	var mu sync.Mutex
	var got []int
	var wg sync.WaitGroup
	gate := make(chan struct{})

	const n = 50
	wg.Add(n)
	for i := 0; i < n; i++ {
		b.enqueuePrompt(laneMsg("s1", i), func(m Message) {
			<-gate
			mu.Lock()
			got = append(got, m.Payload.(map[string]interface{})["n"].(int))
			mu.Unlock()
			wg.Done()
		})
	}
	close(gate)
	wg.Wait()

	for i, v := range got {
		if v != i {
			t.Fatalf("prompt order broken at %d: got %v", i, got)
		}
	}

	// Drained lanes are released so idle sessions do not accumulate.
	deadline := time.Now().Add(time.Second)
	for {
		b.promptLanesMu.Lock()
		left := len(b.promptLanes)
		b.promptLanesMu.Unlock()
		if left == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lane not released after drain: %d left", left)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestEnqueuePromptSurvivesHandlerPanic(t *testing.T) {
	b := &Bridge{}
	done := make(chan struct{})
	b.enqueuePrompt(laneMsg("s1", 1), func(Message) { panic("boom") })
	b.enqueuePrompt(laneMsg("s1", 2), func(Message) { close(done) })
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("lane died after a handler panic")
	}
}
