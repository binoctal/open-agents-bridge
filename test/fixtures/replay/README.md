# Replay fixtures（G17 关键会话回放测试）

本目录是回放测试套件（`internal/bridge/replay_suite_test.go`）的固定数据：
ACP 帧回放脚本、golden 上行序列、以及供 fs 读写帧命中的工作区。

## 文件

| 文件 | 说明 |
|------|------|
| `success.script.jsonl` | 成功路径：`stopReason=end_turn` → `workflow:task_result` |
| `success.golden.jsonl` | 成功路径的 golden 上行序列（与 script 成对） |
| `failure.script.jsonl` | 失败路径：`stopReason=max_tokens` → `workflow:task_error` |
| `hang.script.jsonl` | 回合永不结束（排队路径测试用它占住进程池） |
| `e2e-*.script.jsonl` | parity e2e 六场景（上游产出/下游消费/虚报完成/中途提问/并行改动×2），配合 `scripts/e2e/shim`（add-parity-e2e-verification）；`e2e-question` 用 `afterCount:2` 把 `end_turn` 门控在用户回答注入的第二个 `session/prompt` 上 |
| `workspace/` | fixture 工作区（纯文件，无 git；任务配置 `workDir="."` 非 worktree，规避真实 CommitAll/PushBranch） |

当前已提交的脚本/golden 对是**手工编写的确定性 fixture**（脚本帧格式与真实录制
产物完全一致，golden 序列取自首次真实回放运行的归一化输出）。真实 CLI 录制需要
provider 访问（LLM 网关仅内网可达），有条件时按下述配方重录替换即可。

## 成对重录（禁止只换其一）

LLM 输出不确定，`*.script.jsonl`（ACP 帧）与 `*.golden.jsonl`（上行序列）必须是
**同一次录制的两面**。重录步骤：

1. 按本机 e2e 配方起环境（provider 已配、计划/限流/单 bridge 四道前置，见仓库
   docs 或 memory「本机编排端到端配方」）。
2. 用 `--record-replay-dir` 启动 bridge：

   ```sh
   open-agents-bridge start --record-replay-dir /tmp/replay-out
   ```

   录制产物：`/tmp/replay-out/<sessionID>.jsonl`（ACP 帧脚本）与
   `/tmp/replay-out/uplink.golden.jsonl`（合并上行序列）。
3. 从 `uplink.golden.jsonl` 摘出该任务的 `workflow:*` 窗口（心跳/`session:*` 豁免），
   去掉 payload 内时间戳字段，作为新的 golden；对应的 `<sessionID>.jsonl`
   作为新的 script。
4. 两个文件**同一次提交**落库。回放断言（`TestReplayGoldenSequenceMatches`）会用
   归一化比对（窗口 + 豁免清单都在测试 helper 里），任何单侧漂移都会红。

## 来源审查规则

- **禁止录制生产或私有仓库内容**：录制脚本会原样落盘 ACP 帧（含 prompt、文件
  内容）。只允许在一次性 fixture 任务/公开示例仓库上录制；录制产物入库前逐帧
  过目。
- 头部元数据（`cliType` / `adapterVersion` / `recipe` / `recordedAt`）回答「哪次
  升级该触发重录」——bridge 或 adapter 协议行为变更后，先重录再改断言。

## 版本腐烂排查路径

回放测试红了 ≠ 断言错，按顺序排查：

1. 看失败信息里的「events seen so far」——缺的是哪一类事件（缺失 vs 超时在
   消息里是显式区分的）。
2. golden diff 失败会打印双侧完整序列，先对比事件条数再对比字段。
3. 若 bridge 行为是有意变更：重录成对 fixture（见上），同一提交里更新
   `success.golden.jsonl` 与 `success.script.jsonl`。
4. 若 script 播放挂起：检查 `after` 门控的 method 名是否仍与 bridge 实际发送的
   JSON-RPC method 一致（协议改名是常见腐烂源）。
