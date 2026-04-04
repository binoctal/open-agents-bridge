# OpenAgents Bridge

[中文](README.zh-CN.md)

Local Bridge CLI that connects AI coding tools with the OpenAgents cloud platform.

## Features

- Connect AI CLIs: Claude Code, Gemini CLI, Goose, Cline, Codex, Kiro
- Real-time WebSocket communication
- End-to-end encryption
- Permission request forwarding
- Multi-session management
- **Multi-device support** — run multiple bridge instances on one machine
- **I/O logging** — record user input and AI responses for debugging
- Cross-platform: Windows, Linux, macOS

## Install

### Build from source

```bash
make build
```

### Install to system

```bash
make install
```

## Quick Start

### Pair a device

```bash
# Interactive pairing
open-agents pair

# With device name
open-agents pair --name work-pc
```

### Start the bridge

```bash
# Start a device
open-agents start --device work-pc

# With debug logging
open-agents start --device work-pc --log-level debug
```

### Manage devices

```bash
# List all devices
open-agents devices

# View device details
open-agents device work-pc
```

### System service

```bash
open-agents service install   # Install as system service
open-agents service start     # Start service
open-agents service stop      # Stop service
open-agents service uninstall # Uninstall service
```

## Configuration

Config files are stored in `~/.open-agents/`:

```
~/.open-agents/
├── config.json           # Global config
├── devices/              # Device configs
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
    "kiro": true
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
| Kiro | Supported |

## Development

```bash
make deps      # Download dependencies
make build     # Build binary
make test      # Run tests
make build-all # Build for all platforms
```

## License

GNU Affero General Public License v3.0 (AGPL-3.0). See [LICENSE](LICENSE).
