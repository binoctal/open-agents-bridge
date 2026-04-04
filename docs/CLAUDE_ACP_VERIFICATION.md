# Claude Code ACP 支持验证

## ✅ 验证结果

**Claude Code 确实支持 ACP 协议！**

### 📦 包信息

- **包名**: `@zed-industries/claude-code-acp`
- **版本**: 0.16.2
- **描述**: An ACP-compatible coding agent powered by the Claude Code SDK
- **仓库**: https://github.com/zed-industries/claude-code-acp
- **协议**: Apache-2.0

### 🔧 使用方式

```bash
npx @zed-industries/claude-code-acp
```

### 📋 依赖

- `@agentclientprotocol/sdk`: 0.14.1
- `@anthropic-ai/claude-agent-sdk`: 0.2.44
- `@modelcontextprotocol/sdk`: 1.26.0

### 💻 在 Bridge 中的实现

```go
case "claude":
    // Claude Code ACP via npx
    return "npx", []string{"@zed-industries/claude-code-acp"}
```

### 🎯 协议支持

Claude Code 通过 `@zed-industries/claude-code-acp` 包支持：

1. ✅ **ACP (Agent Client Protocol)** - 主要协议
2. ✅ **MCP (Model Context Protocol)** - 工具集成
3. ✅ JSON-RPC 2.0 通信
4. ✅ 结构化消息（content, thought, tool_call, permission）

### 📚 参考

- AionUi 实现: `demo/AionUi/src/agent/acp/AcpConnection.ts`
- NPM 包: https://www.npmjs.com/package/@zed-industries/claude-code-acp
- GitHub: https://github.com/zed-industries/claude-code-acp

### ⚠️ 注意事项

1. **首次运行**: npx 会自动下载并安装包
2. **网络要求**: 需要访问 npm registry
3. **API Key**: 需要设置 `ANTHROPIC_API_KEY` 环境变量

### 🧪 测试

运行测试脚本：
```bash
cd bridge
./scripts/test-claude-acp.sh
```

### ✅ 结论

Claude Code **完全支持 ACP 协议**，通过 `npx @zed-industries/claude-code-acp` 运行。

当前 Bridge 实现是正确的！
