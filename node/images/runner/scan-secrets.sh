#!/usr/bin/env bash
# Supply-chain secret guard (add-cloud-executor 4.3, spec: 镜像与容器无控制面
# secret). Scans a BUILT image for control-plane credentials -- config env,
# layer history (docker history records ARG/ENV/COPY values baked at build
# time) -- and the image sources for hardcoded credential material. Any hit
# fails the build; this script is wired into build.sh so a secret bakes
# nothing. Also runnable standalone against any built runner tag.
#
# TASK_TOKEN is deliberately NOT in the pattern: the per-task token is
# injected at docker run and is the only credential the sandbox legitimately
# holds. This script scans the IMAGE, which must be credential-free.
set -euo pipefail
cd "$(dirname "$0")"

IMAGE=${1:?usage: scan-secrets.sh <image-tag>}
PATTERN='COOLIFY_API_TOKEN|INTERNAL_SECRET|JWT_SECRET|KEY_ENCRYPTION_KEY|RESEND_API_KEY|CLOUDFLARE_API_TOKEN|ANTHROPIC_API_KEY|OPENAI_API_KEY|API_TOKEN_PROD|GITHUB_TOKEN'

status=0

echo "== image config env =="
if docker image inspect --format '{{json .Config.Env}}' "$IMAGE" | grep -En "$PATTERN"; then
  echo "FAIL: control-plane credential found in image config env" >&2
  status=1
fi

echo "== image layer history =="
if docker history --no-trunc "$IMAGE" | grep -En "$PATTERN"; then
  echo "FAIL: control-plane credential found in layer history (baked ENV/ARG/COPY)" >&2
  status=1
fi

echo "== image sources =="
if grep -RnE "$PATTERN" Dockerfile entrypoint.sh runner.mjs versions.env versions.lock; then
  echo "FAIL: control-plane credential hardcoded in image sources" >&2
  status=1
fi

# A baked generic key material shape (sk-... / ghp_...) in sources is also a
# fail -- it does not need to be a KNOWN name to be a secret.
if grep -RnE '(sk-[A-Za-z0-9]{16,}|ghp_[A-Za-z0-9]{20,})' Dockerfile entrypoint.sh runner.mjs versions.env versions.lock; then
  echo "FAIL: hardcoded key material found in image sources" >&2
  status=1
fi

if [ "$status" -eq 0 ]; then
  echo "scan-secrets: clean"
else
  echo "scan-secrets: FAILED" >&2
fi
exit "$status"
