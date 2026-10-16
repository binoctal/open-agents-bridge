package bridge

import "github.com/binoctal/open-agents-bridge/internal/logger"

// promptLane serialises one session's prompts off the shared messageWorker.
//
// Sending a prompt may block for up to staleTurnWait (60s) while the
// previous turn winds down. Run on the shared worker, that wait stalled every
// other web→bridge message: machine:listDir timed out on the web side (30s),
// and a permission:response the running turn was waiting on sat behind the
// prompt until the stale-turn cancel fired. Prompts of one session keep
// their arrival order; nothing else waits on them.
type promptLane struct {
	pending []promptJob
	running bool
}

type promptJob struct {
	msg    Message
	handle func(Message)
}

func (b *Bridge) enqueuePrompt(msg Message, handle func(Message)) {
	sessionID := ""
	if payload, ok := msg.Payload.(map[string]interface{}); ok {
		sessionID, _ = payload["sessionId"].(string)
	}

	b.promptLanesMu.Lock()
	if b.promptLanes == nil {
		b.promptLanes = map[string]*promptLane{}
	}
	lane, ok := b.promptLanes[sessionID]
	if !ok {
		lane = &promptLane{}
		b.promptLanes[sessionID] = lane
	}
	lane.pending = append(lane.pending, promptJob{msg: msg, handle: handle})
	start := !lane.running
	lane.running = true
	b.promptLanesMu.Unlock()

	if start {
		go b.drainPromptLane(sessionID, lane)
	}
}

func (b *Bridge) drainPromptLane(sessionID string, lane *promptLane) {
	for {
		b.promptLanesMu.Lock()
		if len(lane.pending) == 0 {
			lane.running = false
			delete(b.promptLanes, sessionID)
			b.promptLanesMu.Unlock()
			return
		}
		job := lane.pending[0]
		lane.pending = lane.pending[1:]
		b.promptLanesMu.Unlock()

		b.runPromptJob(job)
	}
}

// runPromptJob isolates a handler panic to the one prompt, mirroring the
// messageWorker's recover so a bad prompt cannot kill the lane silently.
func (b *Bridge) runPromptJob(job promptJob) {
	defer func() {
		if r := recover(); r != nil {
			b.logError("[%s] prompt handler panic (type=%s): %v", logger.ModBridge, job.msg.Type, r)
		}
	}()
	job.handle(job.msg)
}
