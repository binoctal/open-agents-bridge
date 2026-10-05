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
ARG BRIDGE_VERSION
ARG ACP_ADAPTER_VERSION
# The bridge serves cloud interactive sessions (entrypoint.sh: SESSION_TOKEN).
# Exact version only; its postinstall pins and verifies the release archive.
# The bridge launches the claude engine as `npx @agentclientprotocol/claude-agent-acp`;
# the sandbox has no npm egress, so the adapter must be preinstalled (npx finds
# a global install) or the session falls back to a PTY that cannot answer.
RUN npm install -g --no-audit --no-fund @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} \
    @binoctal/open-agents-bridge@${BRIDGE_VERSION} \
    @agentclientprotocol/claude-agent-acp@${ACP_ADAPTER_VERSION} \
 && npm cache clean --force
WORKDIR /workspace
COPY runner.mjs /opt/runner/runner.mjs
COPY entrypoint.sh /opt/runner/entrypoint.sh
RUN chmod 0555 /opt/runner/runner.mjs /opt/runner/entrypoint.sh
# Run as the unprivileged `node` user (uid 1000, built into the official
# node base): the claude CLI refuses --dangerously-skip-permissions under
# root/sudo, so a root runner dies on every task (live e2e 2026-09-09).
# HOME points at the container's noexec tmpfs -- the CLI's state files land
# there and nothing persists past the container.
RUN chown node:node /workspace
USER node
ENV HOME=/tmp
ENTRYPOINT ["/opt/runner/entrypoint.sh"]
