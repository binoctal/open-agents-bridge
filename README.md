# OpenAgents Bridge

[中文](README.zh-CN.md)

Local Bridge CLI that connects AI coding tools with the OpenAgents cloud platform.

## Features

- Connect AI CLIs: Claude Code, Gemini CLI, Goose, Cline, Codex
- Real-time WebSocket communication
- End-to-end encryption
- Permission request forwarding
- Multi-session management
- **Multi-machine support** — run multiple bridge instances on one machine
- **I/O logging** — record user input and AI responses for debugging
- Cross-platform: Windows, Linux, macOS

## Install

```bash
npm i -g @binoctal/open-agents-bridge
```

### Build from source

```bash
make build
```

### Install to system

```bash
make install
```

## Quick Start

### Pair a machine

```bash
# Interactive pairing
open-agents-bridge pair

# With machine name
open-agents-bridge pair --name work-pc
```

### Start the bridge

```bash
# Start a machine
open-agents-bridge start --machine work-pc

# With debug logging
open-agents-bridge start --machine work-pc --log-level debug
```

### Manage machines

```bash
# List all machines
open-agents-bridge machines

# View machine details
open-agents-bridge machine work-pc
```

### System service

```bash
open-agents-bridge service install   # Install as system service
open-agents-bridge service start     # Start service
open-agents-bridge service stop      # Stop service
open-agents-bridge service uninstall # Uninstall service
```

## Configuration

Config files are stored in `~/.open-agents-bridge/`:

```
~/.open-agents-bridge/
├── config.json           # Global config
├── machines/              # Machine configs
│   ├── work-pc.json
│   └── laptop.json
├── logs/                 # Log files
└── sessions/             # Session data
```

### Global config example

```json
{
  "serverUrl": "wss://api.openagents.top",
  "logLevel": "info",
  "cliEnabled": {
    "claude": true,
    "cline": true,
    "codex": true,
    "gemini": true,
  }
}
```

## Supported CLI Tools

| CLI | Status |
|-----|--------|
| Claude Code | Supported |
| Gemini CLI | Supported |
| Goose | Supported |
| Cline | Supported |
| Codex | Supported |

## Development

```bash
make deps      # Download dependencies
make build     # Build binary
make test      # Run tests
make build-all # Build for all platforms
```

## License

GNU Affero General Public License v3.0 (AGPL-3.0). See [LICENSE](LICENSE).

## Trademark and contributing

The Open Agents name and logo are covered by the [Trademark Policy](TRADEMARK.md) (draft). Contributions require agreeing to the CLA described in [CONTRIBUTING.md](CONTRIBUTING.md) (draft). Release signing is described in [docs/release-signing.md](docs/release-signing.md).
