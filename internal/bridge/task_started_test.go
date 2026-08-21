package bridge

import (
	"encoding/json"
	"testing"
)

// Known-issue #7: on the orchestrator→bridge dispatch path the only start
// signal was workflow:task_progress {progress:0, step:"started"}; the
// workflow:task_started emitters were echoes of web-origin commands. The
// dispatch path must emit task_started too, with the same payload fields the
// dispatch family carries (jobId, taskId, deviceId).
func TestTaskStartedMessage(t *testing.T) {
	msg := taskStartedMessage("job_1", "task_2", "dev_3")
	if msg.Type != "workflow:task_started" {
		t.Fatalf("type = %q, want workflow:task_started", msg.Type)
	}
	var p map[string]interface{}
	if err := json.Unmarshal(mustJSON(t, msg.Payload), &p); err != nil {
		t.Fatalf("payload not JSON object: %v", err)
	}
	for _, k := range []string{"jobId", "taskId", "deviceId"} {
		if _, ok := p[k]; !ok {
			t.Fatalf("payload missing %q: %v", k, p)
		}
	}
	if msg.Timestamp <= 0 {
		t.Fatalf("timestamp not set: %d", msg.Timestamp)
	}
}

func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
