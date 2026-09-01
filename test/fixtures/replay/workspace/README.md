# Replay fixture workspace

Pristine workspace used by the G17 replay suite (see `test/fixtures/replay/README.md`).

A recorded task prompt asks the agent to read `src/hello.txt` and write
`src/greeting.txt`. `fs/read` and `fs/write` server requests from the ACP
frames hit this workspace, so replay is deterministic across machines.

Tests copy this directory to a temp dir before running — a replay's
`fs/write` frames must never dirty the committed fixture.
