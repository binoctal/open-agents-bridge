package bridge

import "testing"

// An ACP agent does not exit when its turn ends, so a workflow task run over
// ACP never reached the exit_code branch that stops PTY sessions: the exit
// callback never fired, no task_result was ever sent, and the task sat in
// `running` until the execution deadline swept it (live-observed in the local
// end-to-end on 2026-08-31 — the file was edited, stopReason=end_turn
// arrived, nothing was reported). The turn boundary must end a TASK session,
// and only a task session.
func TestTaskTurnExitCode(t *testing.T) {
	cases := []struct {
		name     string
		meta     map[string]interface{}
		isTask   bool
		wantCode int
		wantEnd  bool
	}{
		{"finished turn ends the task", map[string]interface{}{"stopReason": "end_turn"}, true, 0, true},
		{"truncated turn fails the task", map[string]interface{}{"stopReason": "max_tokens"}, true, 1, true},
		{"turn limit fails the task", map[string]interface{}{"stopReason": "max_turn_requests"}, true, 1, true},
		{"cancelled is not a finish", map[string]interface{}{"stopReason": "cancelled"}, true, 0, false},
		{"unknown reason is not a finish", map[string]interface{}{"stopReason": "whatever"}, true, 0, false},
		{"no stopReason at all", map[string]interface{}{"exit_code": 0}, true, 0, false},
		{"nil meta", nil, true, 0, false},
		// An interactive session must survive its turns — stopping it here
		// would kill the user's agent after every single reply.
		{"interactive session survives end_turn", map[string]interface{}{"stopReason": "end_turn"}, false, 0, false},
		{"interactive session survives max_tokens", map[string]interface{}{"stopReason": "max_tokens"}, false, 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, end := taskTurnExitCode(tc.meta, tc.isTask)
			if end != tc.wantEnd {
				t.Fatalf("terminal = %v, want %v", end, tc.wantEnd)
			}
			if end && code != tc.wantCode {
				t.Fatalf("exit code = %d, want %d", code, tc.wantCode)
			}
		})
	}
}
