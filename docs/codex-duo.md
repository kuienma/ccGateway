# 两个独立 Codex CLI 的编码与决策循环

脚本：`scripts/codex_duo.py`，可单独复制使用，无第三方 Python 依赖。
需要 Python 3、已登录的 Codex CLI 和一个可信 Git 项目。
本机已核对 CLI 版本 `0.154.0` 的参数；旧版本若不支持
`codex exec resume --output-schema`，应先升级。

## 启动

在普通终端中，使用编码端跳过审批和沙箱的模式：

```bash
python3 scripts/codex_duo.py \
  --repo /Users/sh/zhshl/gateway \
  --worker-bypass-permissions \
  --worker-reasoning-effort high \
  --lead-reasoning-effort max \
  '填写实际编码任务和验收条件'
```

`--worker-bypass-permissions` 在编码端每次调用（包括 resume）传入
`--dangerously-bypass-approvals-and-sandbox`。这允许编码端以当前用户权限
执行命令和修改项目外文件，不会赋予 root 权限或解除组织强制策略。
该模式适合由外部容器或虚拟机隔离的运行环境。

不传该选项时，编码端使用 `workspace-write` 沙箱，审批策略为 `never`：

```bash
python3 scripts/codex_duo.py '填写实际编码任务和验收条件'
```

主端始终请求 `read-only` 沙箱和 `never` 审批策略，负责查看实际文件、
审核报告与决策，需要补充测试时安排编码端执行。
脚本沿用本机 Codex 的登录、模型/provider、项目指令和 MCP 配置；
shell 沙箱并不等于对外部 MCP 服务的只读保证，只应使用已信任的配置。

脚本直接修改 `--repo` 指定的目录。需要 Git 隔离时，应先准备独立 worktree，
再把目录传给 `--repo`；worktree 本身不是权限沙箱。

## 两个会话如何协作

1. 主端先启动独立会话，结合项目上下文把用户原始任务改写成包含目标、范围、约束、
   验收标准和验证要求的执行指令；结果保存到 `state.json` 的 `rewritten_task`。
2. 编码端收到原始任务与改写后的执行指令，启动 `codex exec --json
   --output-schema ... --output-last-message ... -`。
3. Python 从 JSONL 中的 `thread.started` 取得双方会话 ID。
4. 编码端返回 `needs_decision`、`ready_for_review` 或 `blocked`。
5. 主端恢复首次改写任务的会话，读取实际修改和报告，返回 `continue`、`approved`
   或 `blocked`。
6. 若继续，使用 `codex exec resume <编码会话 ID> ... -` 发回主端决定，双方持续恢复
   各自会话，直到验收通过、阻塞或达到上限。

两个 CLI 进程串行轮流执行，不是两个终端窗口同时运行。需求改写是第 0 轮主端调用，
不占用 `--max-rounds` 的编码／审核轮数。
技术决定由主 Codex 作出，Python 只负责调度、协议验证与日志。
每轮都会重新提供角色指令及 JSON schema，避免依赖恢复时的隐式参数继承。
脚本不会把单纯的 CLI 退出码 0 当作任务完成：还需完整回合事件、有效 JSON
以及主端在编码端提交验收后明确批准。

可选参数：

| 参数 | 用途 |
| --- | --- |
| `--lead-model <模型>` | 指定主端模型，默认本机配置 |
| `--worker-model <模型>` | 指定编码端模型，默认本机配置 |
| `--lead-reasoning-effort <等级>` | 指定主端思考程度，默认本机配置 |
| `--worker-reasoning-effort <等级>` | 指定编码端思考程度，默认本机配置 |
| `--max-rounds 8` | 最大编码／审核轮数，默认 8 |
| `--timeout 900` | 单次 CLI 超时秒数，默认 900 |
| `--run-dir <目录>` | 指定新的日志目录，不能已经存在 |
| `--codex <路径>` | 指定 Codex 可执行文件 |

思考程度支持 `minimal`、`low`、`medium`、`high`、`xhigh`、`max`。具体可用值由
所选模型决定；例如 `gpt-6-astra` 支持到 `max`。脚本会在每次
新调用和 `resume` 时重新传入对应角色的设置。模型不支持某个等级时，Codex CLI
会返回错误；不传参数时沿用本机 `model_reasoning_effort` 配置。

## 日志、失败和恢复

默认在目标项目 `.runtime/codex-duo/<随机 ID>/` 保存：

- `state.json`：任务、双方会话 ID、最近结果、最终状态和权限模式。
- `000-lead.*`：主端首次改写用户任务的输入、事件、结果和诊断日志。
- `worker.schema.json`、`lead.schema.json`：结构化输出协议。
- `NNN-worker/lead.prompt.txt`：输入指令。
- `NNN-worker/lead.events.jsonl`：原始事件流。
- `NNN-worker/lead.result.json`：模型最终结构化结果。
- `NNN-worker/lead.stderr.txt`：CLI 诊断输出。

退出码 0 表示 `approved`；2 表示阻塞或达到轮数上限；1 表示 CLI／协议错误、
超时或中断。超时和 Ctrl-C 会终止当前进程组，已经发生的文件修改不会回滚。
运行两端会产生各自的模型用量；轮数与超时上限不是金额预算上限。

本版支持循环内自动恢复会话，不支持调度器跨进程断点续跑。
可从 `state.json` 找到已完成调用的会话 ID，在普通终端手工接续：

```bash
codex resume <会话 ID>
```

如果调用中途被终止，最新 ID 可能只存在于 `events.jsonl` 的 `thread.started`
事件中。确认原进程停止后再恢复。手工恢复时按需重新指定权限模式。

## 验证

```bash
python3 -B -m unittest discover -s tests -p 'test_*duo.py'
```

测试通过假的 CLI 子进程验证协议和流程，不调用付费模型。
尚未进行真实模型端到端任务验证。

参考：[OpenAI 官方非交互执行文档](https://developers.openai.com/codex/noninteractive/)。
