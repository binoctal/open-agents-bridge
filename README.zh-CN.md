# OpenAgents Bridge

[English](README.md)

本地 Bridge CLI，连接 AI 编程工具与 OpenAgents 云平台。

## 功能

- 连接 AI CLI：Claude Code、Gemini CLI、Goose、Cline、Codex、Kiro
- WebSocket 实时通信
- 端到端加密
- 权限请求转发
- 多会话管理
- **多设备支持** — 一台机器可运行多个 bridge 实例
- **I/O 日志** — 记录用户输入和 AI 响应用于调试和审计
- 跨平台：Windows、Linux、macOS

## 安装

### 从源码构建

```bash
make build
```

### 安装到系统

```bash
make install
```

## 快速开始

### 配对设备

```bash
# 交互式配对
open-agents pair

# 指定设备名称
open-agents pair --name work-pc
```

### 启动 Bridge

```bash
# 启动设备
open-agents start --device work-pc

# 指定日志级别
open-agents start --device work-pc --log-level debug
```

### 管理设备

```bash
# 列出所有设备
open-agents devices

# 查看设备详情
open-agents device work-pc
```

### 安装为系统服务

```bash
open-agents service install   # 安装服务
open-agents service start     # 启动服务
open-agents service stop      # 停止服务
open-agents service uninstall # 卸载服务
```

## 配置文件

配置文件存储在 `~/.open-agents/`：

```
~/.open-agents/
├── config.json           # 全局配置
├── devices/              # 设备配置
│   ├── work-pc.json
│   └── laptop.json
├── logs/                 # 日志文件
└── sessions/             # 会话数据
```

### 全局配置示例

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

## 支持的 CLI 工具

| CLI | 状态 |
|-----|------|
| Claude Code | 支持 |
| Gemini CLI | 支持 |
| Goose | 支持 |
| Cline | 支持 |
| Codex | 支持 |
| Kiro | 支持 |

## 开发

```bash
make deps      # 下载依赖
make build     # 构建
make test      # 运行测试
make build-all # 构建所有平台
```

## 许可证

GNU Affero General Public License v3.0 (AGPL-3.0)。详见 [LICENSE](LICENSE)。
