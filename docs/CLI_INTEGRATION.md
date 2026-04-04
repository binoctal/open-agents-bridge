# CLI Integration Methods

Open Agents Bridge supports three methods for integrating with CLI tools:

## 1. Wrapper Script (Universal)

**Best for**: Any CLI tool without modification

**How it works**:
- Intercepts CLI commands before execution
- Sends permission requests via Unix socket
- Waits for approval before executing

**Setup**:
```bash
cd bridge/scripts
./install-kiro-wrapper.sh
```

**Usage**:
```bash
kiro-cli chat "your prompt"  # Automatically intercepted
```

**Pros**:
- ✅ Works with any CLI
- ✅ No source code modification needed
- ✅ Simple to install

**Cons**:
- ⚠️ Command-level interception only
- ⚠️ Limited visibility into tool calls

---

## 2. Hook/Plugin (Event-based)

**Best for**: CLIs that support hooks/plugins (like Claude CLI)

**How it works**:
- CLI calls hooks on specific events
- Bridge receives notifications via HTTP
- Full visibility into sessions and tool calls

**Setup**:
```bash
# Bridge automatically starts hook server
# CLI configured with hook settings
```

**Configuration** (`~/.kiro/hooks.json`):
```json
{
  "hooks": {
    "SessionStart": {
      "command": "curl",
      "args": [
        "-X", "POST",
        "http://127.0.0.1:PORT/hook/session-start",
        "-H", "Content-Type: application/json",
        "-d", "@-"
      ]
    },
    "ToolCall": {
      "command": "curl",
      "args": [
        "-X", "POST",
        "http://127.0.0.1:PORT/hook/tool-call",
        "-H", "Content-Type: application/json",
        "-d", "@-"
      ]
    }
  }
}
```

**Pros**:
- ✅ Event-level visibility
- ✅ No source modification
- ✅ Rich context

**Cons**:
- ⚠️ CLI must support hooks
- ⚠️ Requires configuration

---

## 3. ACP Protocol (Standard)

**Best for**: CLIs built with ACP support

**How it works**:
- JSON-RPC communication via stdin/stdout
- Standardized protocol like LSP
- Full bidirectional control

**Setup**:
```bash
# Bridge automatically detects ACP support
# No additional configuration needed
```

**Protocol Example**:

**Initialize**:
```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "initialize",
  "params": {
    "clientInfo": {
      "name": "open-agents-bridge",
      "version": "1.0.0"
    }
  }
}
```

**Tool Call Notification**:
```json
{
  "jsonrpc": "2.0",
  "method": "tool/call",
  "params": {
    "tool": "fs_write",
    "input": {
      "path": "/tmp/test.txt",
      "content": "Hello"
    }
  }
}
```

**Pros**:
- ✅ Standardized protocol
- ✅ Full control
- ✅ Bidirectional communication
- ✅ Auto-detection

**Cons**:
- ⚠️ CLI must implement ACP
- ⚠️ More complex to implement

---

## Comparison

| Feature | Wrapper | Hook | ACP |
|---------|---------|------|-----|
| **No source modification** | ✅ | ✅ | ❌ |
| **Works with any CLI** | ✅ | ❌ | ❌ |
| **Event visibility** | ⚠️ Limited | ✅ Full | ✅ Full |
| **Bidirectional control** | ❌ | ⚠️ Limited | ✅ Full |
| **Setup complexity** | Low | Medium | Low |
| **Performance** | Good | Good | Excellent |

---

## Auto-Detection

Bridge automatically selects the best method:

1. **Check for ACP support** → Use ACP
2. **Check for hook support** → Use Hooks
3. **Fallback** → Use Wrapper

**Configuration** (`~/.open-agents/config.json`):
```json
{
  "clis": {
    "kiro": {
      "command": "kiro-cli",
      "supportsACP": false,
      "supportsHooks": false,
      "adapter": "wrapper"
    },
    "claude": {
      "command": "claude",
      "supportsHooks": true,
      "adapter": "hook"
    },
    "custom-cli": {
      "command": "./my-cli",
      "supportsACP": true,
      "adapter": "acp"
    }
  }
}
```

---

## Implementing ACP in Your CLI

If you're building a CLI tool, implement ACP for best integration:

### 1. Accept JSON-RPC on stdin

```javascript
process.stdin.on('data', (data) => {
  const message = JSON.parse(data);
  handleACPMessage(message);
});
```

### 2. Send notifications on stdout

```javascript
function notifyToolCall(tool, input) {
  const message = {
    jsonrpc: "2.0",
    method: "tool/call",
    params: { tool, input }
  };
  console.log(JSON.stringify(message));
}
```

### 3. Implement required methods

- `initialize` - Handshake
- `chat/send` - Send message
- `tool/call` - Tool call notification (from CLI)
- `tool/response` - Tool response (from Bridge)

See `bridge/docs/ACP_SPEC.md` for full specification.

---

## Testing

```bash
# Test wrapper
kiro-cli chat "test"

# Test hooks
claude --settings ~/.open-agents/hooks.json

# Test ACP
./my-acp-cli --acp-mode
```

---

## Troubleshooting

### Wrapper not working
- Check if wrapper is in PATH
- Verify Unix socket exists: `ls /tmp/open-agents.sock`

### Hooks not firing
- Check hook server port: `netstat -an | grep <PORT>`
- Verify CLI hook configuration

### ACP connection failed
- Check CLI supports ACP
- Verify JSON-RPC format
- Check stdin/stdout not used for other output
