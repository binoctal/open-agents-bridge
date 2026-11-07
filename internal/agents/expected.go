// Package agents owns the provisioning contract for ACP agent wrappers:
// which npm package each CLI type needs, at which exact version, and where it
// is installed on disk. See openspec change acp-startup-hardening.
//
// Versions are pinned EXACT on purpose. A bare `npx --yes <pkg>` re-resolves
// registry "latest" on every spawn; combined with a corrupted npx cache this
// pushed cold start past the ACP handshake timeout on 2026-11-07 and produced
// zombie sessions. Bump these only as part of a deliberate bridge release.
package agents

// ClaudeAgentPackage is the npm package providing the Claude Code ACP server
// (renamed from @zed-industries/claude-code-acp).
const ClaudeAgentPackage = "@agentclientprotocol/claude-agent-acp"

// ClaudeAgentVersion is the exact version the bridge expects to run.
const ClaudeAgentVersion = "0.101.0"

// ClaudeAgentSpec returns the pinned package spec for npx fallback invocation.
func ClaudeAgentSpec() string {
	return ClaudeAgentPackage + "@" + ClaudeAgentVersion
}
