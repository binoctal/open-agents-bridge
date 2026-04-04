# 🔧 Bridge 更新和重启指南

## 问题诊断

当前问题：Bridge 正在运行旧版本，没有使用新的协议系统。

### 症状
- Bridge 日志显示 `[SessionManager] Output event received: type=stdout`
- Web UI 收到 `session:output` 但没有 `chat:response`
- 协议消息没有正确转换

## 解决方案

### 1. 停止旧 Bridge

```bash
# 查找进程
ps aux | grep open-agents

# 停止进程
pkill -f "open-agents start"

# 或者手动 kill
kill <PID>
```

### 2. 重新编译

```bash
cd bridge
go build -o build/open-agents ./cmd/open-agents/
```

### 3. 启动新 Bridge

```bash
cd bridge
./build/open-agents start
```

### 4. 验证

```bash
# 检查日志，应该看到新的协议消息
tail -f ~/.open-agents/logs/bridge.log

# 应该看到类似：
# [SessionManager] Message received: type=content
# [Bridge] Forwarding protocol message: sessionId=xxx, type=content
```

## 快速重启脚本

```bash
cd bridge
./scripts/restart.sh
```

## 验证协议系统

### 检查 Bridge 日志

新版本应该显示：
```
[Protocol] Auto-detecting protocol for <command>
[Protocol] Using ACP protocol
# 或
[Protocol] ACP failed, falling back to PTY
```

### 检查消息类型

新版本应该发送：
- `chat:response` - AI 回复
- `chat:thought` - AI 思考
- `tool:call` - 工具调用
- `permission:request` - 权限请求
- `agent:status` - 状态变化

旧版本只发送：
- `session:output` - 原始输出

## 常见问题

### Q: 为什么 Web UI 没有显示 AI 回复？

A: Bridge 还在运行旧版本，需要：
1. 停止旧进程
2. 重新编译
3. 启动新版本

### Q: 如何确认使用了新版本？

A: 检查日志中是否有：
- `[Protocol]` 前缀
- `[ACP]` 或 `[PTY]` 前缀
- `chat:response` 消息类型

### Q: 重启后还是不行？

A: 检查：
1. 是否使用了正确的二进制文件（`./build/open-agents`）
2. 是否有多个 Bridge 进程在运行
3. Web UI 是否连接到正确的 WebSocket

## 自动化脚本

### 完整重启流程

```bash
#!/bin/bash
cd bridge

# 1. 停止旧进程
echo "🛑 Stopping old Bridge..."
pkill -f "open-agents start"
sleep 2

# 2. 重新编译
echo "🔨 Rebuilding..."
go build -o build/open-agents ./cmd/open-agents/

# 3. 启动新进程
echo "🚀 Starting new Bridge..."
./build/open-agents start &

# 4. 等待启动
sleep 2

# 5. 检查状态
echo "✅ Bridge restarted"
ps aux | grep '[o]pen-agents start'
```

保存为 `scripts/full-restart.sh` 并运行：
```bash
chmod +x scripts/full-restart.sh
./scripts/full-restart.sh
```
