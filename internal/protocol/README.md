# Protocol System

Open Agents Bridge 的多协议通信系统。

## 架构

```
Protocol Manager
├── ACP Adapter (优先)
│   ├── JSON-RPC 2.0
│   ├── stdio 通信
│   └── 支持: Claude Code, Qwen Code, Goose, Gemini CLI
└── PTY Adapter (兜底)
    ├── 伪终端
    ├── 原始输出
    └── 支持: 所有 CLI 工具
```

## 使用方法

### 基本用法

```go
import "github.com/open-agents/bridge/internal/protocol"

// 创建管理器
manager := protocol.NewManager()

// 订阅消息
manager.Subscribe(func(msg protocol.Message) {
    switch msg.Type {
    case protocol.MessageTypeContent:
        fmt.Println("Content:", msg.Content)
    case protocol.MessageTypePermission:
        // 处理权限请求
    }
})

// 连接（自动检测协议）
config := protocol.AdapterConfig{
    WorkDir: ".",
    Command: "claude",
    Args:    []string{"--experimental-acp"},
}
manager.Connect(config)

// 发送消息
manager.SendMessage(protocol.Message{
    Type:    protocol.MessageTypeContent,
    Content: "Hello!",
})
```

### 协议检测

Manager 会自动尝试以下顺序：

1. **ACP** - 发送 `initialize` 请求，等待 3 秒
2. **PTY** - 如果 ACP 失败，使用 PTY

### 消息类型

| 类型 | 说明 | ACP | PTY |
|------|------|-----|-----|
| `content` | AI 回复文本 | ✅ | ✅ |
| `thought` | AI 思考过程 | ✅ | ❌ |
| `tool_call` | 工具调用 | ✅ | ❌ |
| `permission` | 权限请求 | ✅ | ❌ |
| `status` | 状态变化 | ✅ | ✅ |
| `error` | 错误消息 | ✅ | ❌ |

### 支持的 CLI 工具

#### ACP 协议

- Claude Code: `npx @zed-industries/claude-code-acp`
- Qwen Code: `qwen-code --experimental-acp`
- Goose: `goose acp`
- Gemini CLI: `gemini-cli --acp`

#### PTY 协议（兜底）

- Kiro CLI
- Cline
- Codex
- 所有其他 CLI 工具

## 开发

### 运行测试

```bash
# 单元测试
go test ./internal/protocol/

# 集成测试
go test -v ./internal/protocol/
```

### 运行演示

```bash
cd bridge
go run cmd/demo/protocol_demo.go
```

## 扩展

### 添加新协议

1. 实现 `protocol.Adapter` 接口
2. 在 `Manager.Connect()` 中添加检测逻辑
3. 添加测试

示例：

```go
type MyAdapter struct {
    // ...
}

func (a *MyAdapter) Name() string { return "my-protocol" }
func (a *MyAdapter) Connect(config AdapterConfig) error { /* ... */ }
// ... 实现其他方法
```

## 参考

- [ACP 协议规范](../../.kiro/specs/acp-protocol-integration/requirements.md)
- [AionUi ACP 实现](../../demo/AionUi/src/agent/acp/)
