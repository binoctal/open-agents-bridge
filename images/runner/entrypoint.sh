#!/bin/sh
# Runner entrypoint (add-cloud-executor 4.3). Kept a shell one-liner on
# purpose: all logic lives in runner.mjs so it is testable and the image
# surface stays minimal. No credential may be baked into this image -- the
# per-task token arrives only as container env at docker run.
#
# Cloud interactive session (cloud-session-shadow-device): SESSION_TOKEN (sst_)
# in the env means this container serves one session; the bridge builds its
# whole identity from the env, so there is nothing to pair or mount.
set -eu
if [ -n "${SESSION_TOKEN:-}" ]; then
  command -v open-agents-bridge >/dev/null 2>&1 || {
    echo "open-agents-bridge is not installed in this image" >&2
    exit 127
  }
  exec open-agents-bridge start
fi
exec node /opt/runner/runner.mjs
