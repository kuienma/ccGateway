# 两个独立 Claude Code CLI 的编码与决策循环

运行环境：Python 3、已登录的 Claude Code。本机检查版本为 2.1.273；
脚本使用的 `--permission-prompts none` 需要 2.1.259 或更新版本。

在普通终端运行：

```bash
python3 scripts/claude_duo.py \
  --repo /Users/sh/zhshl/gateway \
  --allow 'Bash(go test *)' \
  --allow 'Bash(go build *)' \
  '填写具体编码任务，以及完成后应通过的验收条件'
```

脚本直接修改 `--repo` 指定目录，建议指定事先准备好的独立 worktree。
不要把示例任务原样当作实际任务运行。

Python 只负责启动进程、保存日志和传递结果；技术决定由主 Claude 会话作出。
两个会话串行轮流工作，分别保留自己的 session ID，每轮都启动独立 CLI 进程。
编码端先工作，返回待决策问题、待验收结果或阻塞；主端读报告、检查文件，
返回继续指令、验收通过或必须由用户解决的阻塞。继续指令通过
`claude -p --resume <worker-session-id>` 发回原编码会话，主端也恢复自身会话。

模型默认沿用本机配置；可使用 `--lead-model`、`--worker-model` 分别指定。
默认最多 8 轮，每次 CLI 调用最多 900 秒，分别用 `--max-rounds`、`--timeout` 调整。
每轮包含一次编码调用和一次主端调用，实际消耗取决于两个会话的运行情况。

## 权限和审核

编码端可读写文件；主端只有 Read、Grep、Glob，只读核验文件和测试报告。
需要补充验证时，主端安排编码端执行，主端本身不运行测试。
用重复的 `--allow` 参数预先授权具体命令；默认不会自动批准任意 Bash 命令。
默认工具使用 `dontAsk` 模式及 `--permission-prompts none`，权限不足会形成阻塞，
不会出现无人回答的交互弹窗，也不会绕过权限。

如果明确希望编码端跳过 Claude 的工具权限确认，可以这样启动：

```bash
python3 scripts/claude_duo.py --worker-bypass-permissions '你的编码任务和验收条件'
```

该选项在编码端的首次调用和每次恢复时传入 `--dangerously-skip-permissions`，
无需逐条提供 `--allow`。主端仍使用只读工具。
这允许编码端执行当前系统用户权限范围内的命令，也可能修改项目外的文件；
它不会赋予 root 权限，不能解除操作系统限制、组织强制策略或补足缺失凭据。
建议在隔离环境中运行。`--permission-prompts none` 仍保留以禁止等待交互回答。

本版本限制内置工具并关闭自动加载的 MCP 服务，以固定两个会话的工具范围。
它仍可能加载本机或项目的指令、skills 和 hooks；这里只应运行可信项目。
如果目标项目要求必须使用某个 MCP（例如 code-review-graph），先调整脚本的
MCP 配置和工具权限再使用，不能用此脚本跳过项目要求。
“不发布、不推送、不扩大任务”等属于角色指令，不是操作系统沙箱；
命令权限及工作目录隔离仍需按实际项目配置。

## 记录与退出

每次运行创建 `.runtime/claude-duo/<随机 ID>/`，保存：

- `state.json`：任务、两端 session ID、最近结果与退出状态。
- `NNN-worker/lead.prompt.txt`：每轮输入。
- `NNN-worker/lead.stdout.json`、`.stderr.txt`：CLI 原始结果与错误日志。

状态只有 `approved` 才表示主端验收通过（退出码 0）。
阻塞和轮数上限退出码 2，CLI 错误、异常输出、超时和中断退出码 1。
超时或 Ctrl-C 会终止当前 CLI 进程组；已发生的文件修改不会回滚。
日志可能包含项目代码和问题描述，应按项目资料保存。

这个初版支持循环内自动恢复会话，不支持整个调度器跨进程断点续跑。
中断后可从 `state.json` 取出 session ID，在普通终端手工接续：

```bash
claude --resume <session-id>
```

不要同时恢复仍在运行的会话，也不要使用 `--continue` 猜测最近会话。
脚本应从普通终端启动；未实现从另一个 Claude 会话中嵌套启动的兼容处理。

## 验证

```bash
python3 -B -m unittest discover -s tests -p test_claude_duo.py
```

测试使用假的 CLI 子进程验证实际调度、会话恢复、错误与验收门槛，不发送模型请求。
真实模型的编码效果和账号权限需要在实际任务中验证。

参考：[Claude Code 程序化调用](https://code.claude.com/docs/en/headless)。
