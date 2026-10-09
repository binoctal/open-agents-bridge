> **DRAFT - pending the owner's review. Not yet in force. 草案，待所有者审阅，尚未生效。**
> The CLA text below should be reviewed by a lawyer before external contributions are accepted.

# Contributing / 贡献指南

## English

Thanks for helping. Before opening a pull request:

1. Open an issue (or comment on an existing one) for anything larger than a small fix.
2. Keep changes focused; add or update Go tests for behavior changes. `go vet ./... && go test ./...` must pass.
3. Commit messages in English. Do not add `Co-Authored-By` trailers.
4. Do not include secrets, private keys or personal data.

### Contributor License Agreement (CLA)

By submitting a contribution (code, documentation, or other material) to this
repository you agree that:

1. You wrote the contribution or have the right to submit it, and it does not
   knowingly infringe anyone's rights. If your employer has rights in it, you
   have their permission.
2. You keep the copyright in your contribution, and you grant the maintainer
   (binoctal) a perpetual, worldwide, non-exclusive, royalty-free, irrevocable
   license to use, reproduce, modify, sublicense, distribute and **relicense**
   your contribution under any license terms, including proprietary or
   dual-licensing terms, in addition to the project's current license (AGPL-3.0).
3. You grant the maintainer a patent license for any patents you own that your
   contribution necessarily infringes.
4. The Marks described in TRADEMARK.md are not licensed to you by contributing.

**How to sign:** post a comment on your pull request that says exactly:

```
I have read the CLA in CONTRIBUTING.md and I agree to it. CLA
```

You only need to sign once; later pull requests from the same GitHub account
and commit email are covered.

### How the CLA is enforced (manual)

Third-party GitHub Actions are blocked by org policy, so there is no CLA bot.
The maintainer enforces it by hand before merging:

1. Check the PR for the `CLA` comment from the PR author.
2. Add the author's commit identity to `.github/CLA-SIGNERS` (one `Name <email>`
   per line, with a `# PR link` comment).
3. Run `scripts/cla-check.sh origin/main HEAD` on the PR branch. It lists every
   commit author in the range who is not in `.github/CLA-SIGNERS` and exits
   non-zero if there is any. Do not merge while it fails.

## 中文

感谢参与。提交 PR 前请：

1. 较大的改动先开 issue 讨论。
2. 保持改动聚焦；行为变更须补充 Go 测试，`go vet ./... && go test ./...` 必须通过。
3. 提交信息使用英文，不要添加 `Co-Authored-By`。
4. 不要提交密钥、私钥或个人数据。

### 贡献者许可协议（CLA）

向本仓库提交贡献（代码、文档或其他材料）即表示你同意：

1. 贡献由你本人创作或你有权提交，且不明知地侵犯他人权利；若雇主对其享有权利，你已获其许可。
2. 你保留贡献的版权，并授予维护者（binoctal）永久、全球范围、非独占、免版税、不可撤销的许可：
   可在项目现行许可证（AGPL-3.0）之外，以任何许可条款（包括闭源或双重授权）使用、复制、修改、
   再许可、分发并**重新授权**你的贡献。
3. 你对自己拥有的、贡献必然侵犯的专利，授予维护者专利许可。
4. 提交贡献不意味着授予你 TRADEMARK.md 所述商标的任何权利。

**签署方式：** 在你的 PR 下评论如下内容：

```
I have read the CLA in CONTRIBUTING.md and I agree to it. CLA
```

只需签署一次；同一 GitHub 账号与提交邮箱的后续 PR 视为已覆盖。

### CLA 的执行方式（人工）

组织策略禁用了第三方 GitHub Action，因此没有 CLA 机器人，由维护者合并前人工执行：

1. 确认 PR 作者已发表 `CLA` 评论；
2. 将作者的提交身份写入 `.github/CLA-SIGNERS`（每行 `Name <email>`，附 `# PR 链接` 注释）；
3. 在 PR 分支运行 `scripts/cla-check.sh origin/main HEAD`，它列出范围内不在 `.github/CLA-SIGNERS`
   的提交作者，存在则以非零退出。未通过不得合并。
