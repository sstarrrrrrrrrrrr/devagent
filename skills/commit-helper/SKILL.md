---
name: commit-helper
description: 规范化 git 提交流程。当用户要求提交代码、写 commit message、commit 或 push 时使用。
---

# 规范化 Git 提交

你的任务是把这个项目的改动按规范提交到 git。**每一步都基于真实命令输出，禁止凭记忆或猜测描述改动内容。**

## 流程（严格按顺序）

1. **摸清现状**：依次执行 `git status`、`git diff`、`git log --oneline -5`（看最近的 message 风格）。工作区干净就直接告诉用户"没有可提交的改动"，流程结束。
2. **逐项检查改动**：对 status 里的每个改动文件，确认它属于哪类变更。出现以下任何一种 → **立即停止并警告用户，不做提交**：
   - 疑似密钥/凭据：`.env`、`*token*`、`*secret*`、`*key*` 文件或 diff 里出现 `ghp_`、`sk-`、`password` 等字样
   - 不该入库的产物：`node_modules/`、二进制大文件、`.exe`、日志文件
   - **嵌套 git 仓库**（status 显示 backtick 或提示 gitlink）：这是常见坑，直接入库会导致别人 clone 下来是空目录。处理方式：先加入 `.gitignore`（追加一行 `目录名/`），单独 commit 这条 ignore 规则，再继续正常提交流程。
3. **起草 message**：遵循 Conventional Commits，中文描述：
   - 格式：`<type>: <一句话说清这次改动做了什么>`
   - type 取值：`feat` 新功能 / `fix` 修复 / `docs` 文档 / `refactor` 重构 / `style` 格式 / `test` 测试 / `chore` 杂务
   - 参考第 1 步 log 里的既有风格保持一致；正文可有可无，需要时空一行后写动机和影响。
4. **向用户确认**：把拟提交的文件清单 + 完整 message 展示出来，问用户"确认提交吗"。用户不同意就按意见修改，不要擅自提交。
5. **执行提交**：`git add <明确列出的文件>`（**不用 `git add -A`/`git add .`**——只加确认过的文件）；message 含中文时走 `git commit -F <临时文件>`（先写入 UTF-8 临时文件，提交后删除），避免命令行编码乱码。
6. **推送（仅当用户要求 push）**：先 `git log origin/<当前分支>..<当前分支> --oneline` 确认待推的就是刚才的提交，再 `git push origin <分支>`。

## 异常处理

- **当前不在 main/默认分支**：提交前告知用户当前分支名，确认是否就提交到这里。
- **add 或 commit 审批被拒绝**：不要绕道（比如改用别的命令拼接），直接问用户想怎么处理。
- **push 被拒绝（non-fast-forward）**：说明远端有新提交，**禁止 `--force`**——把 `git pull --rebase` 的选项解释给用户听，让用户决定；用户不在场就停在推送前一步并报告。
- **存在合并冲突/未跟踪的陌生大目录**：终止流程，报告现状，让用户手动处理。
- **任何一步的命令连续失败 2 次**：停止重试，输出已执行的命令和报错原文，转为请用户手动处理。

## 硬性红线

- 禁止 `git push --force`、`git reset --hard`、`git clean`——无论用户多着急。
- 禁止把第 2 步列出的敏感/无关文件加入提交，即使用户说"全部提交"——先警告，用户明确坚持时才执行并再次说明后果。
- commit message 不编造改动内容；diff 里没有的变更不写进 message。
