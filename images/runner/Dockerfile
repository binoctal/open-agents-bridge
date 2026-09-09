# Cloud-sandbox runner image (add-cloud-executor 4.3, design D8).
#
# Reproducible by construction: every input that could change the result --
# the base image and each CLI engine version -- arrives as a build arg from
# versions.lock (digest-pinned base, exact versions). build.sh refuses to
# build without the lock, so the intent tags in versions.env never reach a
# build. `latest` is forbidden anywhere in the chain.
#
# Credential discipline: the ONLY secret this image may ever see is the
# per-task token injected as container env at docker run. No control-plane
# credential may appear below as ARG/ENV/COPY -- scan-secrets.sh fails the
# build on any hit, and the guard test (internal/agent/image_guard_test.go)
# pins this statically.
ARG NODE_IMAGE
ARG NODE_DIGEST
FROM ${NODE_IMAGE}@${NODE_DIGEST}
ARG CLAUDE_CODE_VERSION
RUN npm install -g --no-audit --no-fund @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} \
 && npm cache clean --force
WORKDIR /workspace
COPY runner.mjs /opt/runner/runner.mjs
COPY entrypoint.sh /opt/runner/entrypoint.sh
RUN chmod 0555 /opt/runner/runner.mjs /opt/runner/entrypoint.sh
ENTRYPOINT ["/opt/runner/entrypoint.sh"]
