#!/bin/sh
# Runner entrypoint (add-cloud-executor 4.3). Kept a shell one-liner on
# purpose: all logic lives in runner.mjs so it is testable and the image
# surface stays minimal. No credential may be baked into this image -- the
# per-task token arrives only as container env at docker run.
set -eu
exec node /opt/runner/runner.mjs
