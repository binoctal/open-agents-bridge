package bridge

import (
	"time"

	"github.com/binoctal/open-agents-bridge/internal/logger"
)

// taskBase is the baseline a task worktree was cut from.
type taskBase struct {
	Branch string // main checkout's branch at assign time ("" when detached)
	Commit string
}

func (b *Bridge) setTaskBase(taskId string, tb taskBase) {
	b.taskBasesMu.Lock()
	defer b.taskBasesMu.Unlock()
	if b.taskBases == nil {
		b.taskBases = make(map[string]taskBase)
	}
	b.taskBases[taskId] = tb
}

// taskStartedWithBase is the dispatch-path workflow:task_started frame plus
// the baseline (baseBranch/baseCommit) the API records for the mission.
func (b *Bridge) taskStartedWithBase(jobId, taskId string) Message {
	msg := taskStartedMessage(jobId, taskId, b.config.DeviceID)
	b.taskBasesMu.Lock()
	tb, ok := b.taskBases[taskId]
	b.taskBasesMu.Unlock()
	if ok {
		if p, isMap := msg.Payload.(map[string]interface{}); isMap {
			p["baseBranch"] = tb.Branch
			p["baseCommit"] = tb.Commit
		}
	}
	return msg
}

// handleWorkflowDeliver performs an explicit mission delivery
// (mission-branch-delivery D4): push the integration branch, or fast-forward
// the user's base branch. Reports workflow:delivery_result.
func (b *Bridge) handleWorkflowDeliver(msg Message) {
	payload, ok := msg.Payload.(map[string]interface{})
	if !ok {
		return
	}
	missionId := getString(payload, "missionId")
	action := getString(payload, "action")
	b.logInfo("[%s] Workflow deliver: mission %s action %s", logger.ModWorkflow, missionId, action)

	res := b.managerFor(payload, getString(payload, "jobId")).Deliver(missionId, action,
		getString(payload, "integrationBranch"), getString(payload, "baseBranch"))

	out := map[string]interface{}{
		"missionId": missionId,
		"jobId":     getString(payload, "jobId"),
		"deviceId":  b.config.DeviceID,
		"action":    action,
		"ok":        res.OK,
	}
	if res.OK {
		out["headCommit"] = res.HeadCommit
	} else {
		out["reason"] = res.Reason
		if res.Detail != "" {
			out["error"] = res.Detail
		}
		b.logInfo("[%s] Deliver refused for mission %s: %s %s", logger.ModWorkflow, missionId, res.Reason, res.Detail)
	}
	b.sendMessage(Message{Type: "workflow:delivery_result", Payload: out, Timestamp: time.Now().UnixMilli()})
}
