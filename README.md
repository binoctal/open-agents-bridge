# Open Agents Bridge

Local Bridge 程序，连接 AI CLI 工具与 Open Agents 云端服务。

## 功能

- 连接 Kiro、Claude、Cline、Codex、Gemini 等 AI CLI
- WebSocket 实时通信
- 端到端加密
- 权限请求转发
- 多会话管理
- **多设备支持** - 一台机器可运行多个 bridge 实例
- **I/O 日志** - 记录用户输入和 AI 响应用于调试和审计
- 跨平台支持 (Windows, Linux, macOS)

## 安装

### 从源码构建

```bash
cd bridge
make build
```

### 安装到系统

```bash
make install
```

## 使用

### 配对设备

```bash
# 交互式配对
open-agents pair

# 指定设备名称
open-agents pair --name work-pc
open-agents pair --name personal-laptop
open-agents pair --name testing
```

### 管理设备

```bash
# 列出所有设备
open-agents devices

# 切换当前设备
open-agents use work-pc

# 查看设备详情
open-agents device work-pc
```

### 启动 Bridge

```bash
# 启动当前设备
open-agents start

# 启动指定设备
open-agents start --device work-pc

# 启动多个设备（不同终端）
open-agents start --device work-pc
open-agents start --device personal-laptop

# 使用环境变量
export OPEN_AGENTS_DEVICE=work-pc
open-agents start

# 启动时指定日志级别
open-agents start --log-level debug
```

### 查看状态

```bash
# 查看状态
open-agents status

# 查看日志
open-agents logs -f

# 查看特定设备日志
open-agents logs --device work-pc
```

### 安装为系统服务

```bash
# 安装服务
open-agents service install

# 启动服务
open-agents service start

# 停止服务
open-agents service stop

# 卸载服务
open-agents service uninstall
```

## 配置文件

### 目录结构

```
~/.open-agents/
├── config.json           # 全局配置
├── devices/              # 设备配置目录
│   ├── work-pc.json
│   ├── personal-laptop.json
│   └── testing.json
├── logs/
│   ├── work-pc-2026-03-14.log
│   └── personal-laptop-2026-03-14.log
└── sessions/             # 会话数据
```

### 文件位置

| 平台 | 配置目录 | 日志目录 |
|------|----------|----------|
| Linux | `~/.open-agents/` | `~/.open-agents/logs/` |
| macOS | `~/.open-agents/` | `~/Library/Logs/open-agents/` |
| Windows | `%APPDATA%\open-agents\` | `%APPDATA%\open-agents\logs\` |

### 全局配置示例

```json
{
  "serverUrl": "wss://open-agents-api.binoctal.workers.dev",
  "environment": "staging",
  "logLevel": "info",
  "cliEnabled": {
    "claude": true,
    "cline": true,
    "codex": true,
    "gemini": true,
    "kiro": true
  },
  "permissions": {
    "fs_read": true,
    "fs_write": true,
    "execute_bash": true,
    "network": false
  }
}
```

### 设备配置示例

```json
{
  "deviceName": "work-pc",
  "deviceId": "device_xxx",
  "deviceToken": "token_xxx",
  "userId": "user_xxx",
  "publicKey": "...",
  "privateKey": "..."
}
```

### 环境自动检测

| ServerURL 包含 | 检测结果 |
|----------------|---------|
| `staging`, `preview`, `-staging` | `staging` |
| `localhost`, `127.0.0.1` | `development` |
| 其他 | `production` |

## 多设备场景

### 场景 1: 工作与个人分离

```bash
# 配对两个设备
open-agents pair --name work-pc      # 连接生产环境
open-agents pair --name personal     # 连接测试环境

# 同时运行两个 bridge
open-agents start --device work-pc &
open-agents start --device personal &
```

### 场景 2: 开发与测试

```bash
# 配对设备
open-agents pair --name dev          # 日常开发
open-agents pair --name testing      # 运行测试

# 切换设备
open-agents use dev
open-agents start
```

## 支持的 CLI 工具

| CLI | 状态 |
|-----|------|
| Kiro | ✅ 支持 |
| Cline | ✅ 支持 |
| Claude | ✅ 支持 |
| Codex | ✅ 支持 |
| Gemini | ✅ 支持 |

## I/O 日志

Bridge 支持 I/O 日志功能，用于记录用户输入和 AI 响应，方便调试和审计。

### 启用 I/O 日志

在 `config.json` 中添加 `ioLogging` 配置：

```json
{
  "ioLogging": {
    "enabled": true,
    "types": ["prompt", "agent_message"],
    "maxSizeMB": 50,
    "maxBackups": 7
  }
}
```

### 配置项说明

| 字段 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `enabled` | bool | false | 是否启用 I/O 日志 |
| `types` | []string | ["prompt", "agent_message"] | 要记录的消息类型 |
| `maxSizeMB` | int | 50 | 单个日志文件最大大小 (MB) |
| `maxBackups` | int | 7 | 保留的日志文件数量 |

### 支持的消息类型

| 类型 | 说明 |
|------|------|
| `prompt` | 用户发送的输入 |
| `agent_message` | AI 的文本响应 |
| `agent_thought` | AI 的思考过程 |
| `tool_call` | 工具调用请求 |

### 日志格式

日志采用 JSON Lines 格式，每行一个 JSON 对象：

```json
{"timestamp":"2026-03-23T22:00:00Z","sessionId":"session-123","direction":"input","type":"prompt","content":"Hello, AI!"}
{"timestamp":"2026-03-23T22:00:05Z","sessionId":"session-123","direction":"output","type":"agent_message","content":"Hello, human!"}
```

### 日志文件位置

| 平台 | 日志路径 |
|------|----------|
| Linux | `~/.open-agents/logs/io-YYYY-MM-DD.log` |
| macOS | `~/Library/Logs/open-agents/io-YYYY-MM-DD.log` |
| Windows | `%APPDATA%\open-agents\logs\io-YYYY-MM-DD.log` |

### 隐私注意

⚠️ I/O 日志可能包含敏感信息（如代码、API 密钥等）。默认禁用，请根据需要启用。日志文件存储在本地，不会上传到云端。

## 开发

```bash
# 下载依赖
make deps

# 构建
make build

# 运行测试
make test

# 构建所有平台
make build-all
```

## 项目结构

```
bridge/
├── cmd/open-agents/     # CLI 入口
│   ├── main.go
│   ├── cmd_pair.go
│   ├── cmd_start.go
│   ├── cmd_devices.go
│   └── cmd_use.go
├── internal/
│   ├── adapter/         # CLI 适配器
│   ├── bridge/          # 核心 Bridge 逻辑
│   ├── config/          # 配置管理
│   ├── logger/          # 日志系统
│   └── session/         # 会话管理
├── go.mod
├── Makefile
├── README.md
└── config.example.json
```

## License

GNU Affero General Public License v3.0 (AGPL-3.0). See [LICENSE](LICENSE) for details.
