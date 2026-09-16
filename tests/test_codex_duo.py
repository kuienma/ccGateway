"""假 Codex 子进程驱动完整 JSONL 调度流程，不调用模型。"""
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / "scripts/codex_duo.py"


class CodexDuoTest(unittest.TestCase):
    def run_duo(self, replies, extra=(), rewrite_message="优化后的可执行任务",
                rewrite_status="continue"):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            replies = [self.reply("lead", rewrite_status, rewrite_message), *replies]
            (root / "replies.json").write_text(json.dumps(replies))
            fake = root / "codex"
            fake.write_text('''#!/usr/bin/env python3
import json, pathlib, sys, time
p = pathlib.Path.cwd()
log = p / "calls.json"
calls = json.loads(log.read_text()) if log.exists() else []
calls.append({"args": sys.argv[1:], "prompt": sys.stdin.read()})
log.write_text(json.dumps(calls))
reply = json.loads((p / "replies.json").read_text())[len(calls)-1]
time.sleep(reply.get("sleep", 0))
for event in reply["events"]:
    print(json.dumps(event), flush=True)
if "output" in reply:
    pathlib.Path(sys.argv[sys.argv.index("--output-last-message")+1]).write_text(json.dumps(reply["output"]))
sys.exit(reply.get("exit", 0))
''')
            fake.chmod(0o700)
            result = subprocess.run([
                sys.executable, str(SCRIPT), "--repo", str(root), "--codex", str(fake),
                "--run-dir", str(root / "run"), *extra, "完成指定测试任务"],
                capture_output=True, text=True, timeout=15)
            calls = json.loads((root / "calls.json").read_text()) if (root / "calls.json").exists() else []
            state = json.loads((root / "run/state.json").read_text()) if (root / "run/state.json").exists() else {}
            return result, calls, state

    def reply(self, sid, status, message="说明"):
        return {"events": [{"type": "thread.started", "thread_id": sid},
                           {"type": "turn.completed", "usage": {}}],
                "output": {"status": status, "message": message}}

    def test_decision_and_review_resume_correct_threads(self):
        result, calls, state = self.run_duo([
            self.reply("worker", "needs_decision"), self.reply("lead", "continue", "采用方案 A"),
            self.reply("worker", "ready_for_review"), self.reply("lead", "approved")])
        self.assertEqual(result.returncode, 0, result.stderr)
        for index, sid in ((3, "worker"), (4, "lead")):
            args = calls[index]["args"]
            self.assertEqual(args[args.index("resume")+1], sid)
            self.assertIn("--output-schema", args)
        self.assertIn("采用方案 A", calls[3]["prompt"])
        self.assertEqual(state["status"], "approved")
        self.assertIn('sandbox_mode="read-only"', calls[0]["args"])
        self.assertIn('sandbox_mode="workspace-write"', calls[1]["args"])

    def test_lead_rewrites_task_before_worker_starts(self):
        result, calls, state = self.run_duo([
            self.reply("worker", "ready_for_review"), self.reply("lead", "approved")],
            rewrite_message="明确后的目标、范围和验收标准")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(calls), 3)
        self.assertIn("改写", calls[0]["prompt"])
        self.assertIn("完成指定测试任务", calls[0]["prompt"])
        self.assertIn("明确后的目标、范围和验收标准", calls[1]["prompt"])
        self.assertEqual(state["rewritten_task"], "明确后的目标、范围和验收标准")
        self.assertEqual(calls[0]["args"][0], "exec")
        self.assertEqual(calls[1]["args"][0], "exec")

    def test_rewrite_blocker_stops_before_worker(self):
        result, calls, state = self.run_duo(
            [], rewrite_message="需要用户提供业务范围", rewrite_status="blocked")
        self.assertEqual(result.returncode, 2, result.stderr)
        self.assertEqual(len(calls), 1)
        self.assertEqual(state["status"], "blocked")
        self.assertNotIn("rewritten_task", state)

    def test_bypass_sticks_on_worker_resume(self):
        result, calls, _ = self.run_duo([
            self.reply("worker", "needs_decision"), self.reply("lead", "continue"),
            self.reply("worker", "ready_for_review"), self.reply("lead", "approved")],
            extra=("--worker-bypass-permissions",))
        self.assertEqual(result.returncode, 0, result.stderr)
        for index, call in enumerate(calls):
            is_worker = index in (1, 3)
            self.assertEqual("--dangerously-bypass-approvals-and-sandbox" in call["args"], is_worker)

    def test_role_reasoning_effort_sticks_on_resume(self):
        result, calls, state = self.run_duo([
            self.reply("worker", "needs_decision"), self.reply("lead", "continue"),
            self.reply("worker", "ready_for_review"), self.reply("lead", "approved")],
            extra=("--worker-reasoning-effort", "high",
                   "--lead-reasoning-effort", "max"))
        self.assertEqual(result.returncode, 0, result.stderr)
        for index, call in enumerate(calls):
            expected = ('model_reasoning_effort="high"' if index in (1, 3)
                        else 'model_reasoning_effort="max"')
            self.assertIn(expected, call["args"])
        self.assertEqual(state["worker_reasoning_effort"], "high")
        self.assertEqual(state["lead_reasoning_effort"], "max")

    def test_turn_failed_is_error_even_with_zero_exit(self):
        reply = self.reply("worker", "ready_for_review")
        reply["events"][-1] = {"type": "turn.failed", "error": {"message": "认证失败"}}
        result, _, state = self.run_duo([reply])
        self.assertEqual(result.returncode, 1)
        self.assertIn("认证失败", state["error"])

    def test_missing_final_message_is_error(self):
        reply = self.reply("worker", "ready_for_review")
        del reply["output"]
        result, _, state = self.run_duo([reply])
        self.assertEqual(result.returncode, 1)
        self.assertEqual(state["status"], "error")

    def test_cannot_approve_pending_decision(self):
        result, _, state = self.run_duo([
            self.reply("worker", "needs_decision"), self.reply("lead", "approved")])
        self.assertEqual(result.returncode, 1)
        self.assertEqual(state["status"], "error")

    def test_cannot_reuse_worker_thread_for_lead(self):
        result, _, state = self.run_duo([
            self.reply("worker", "ready_for_review"), self.reply("worker", "approved")])
        self.assertEqual(result.returncode, 1)
        self.assertEqual(state["status"], "error")

    def test_blocker_is_reported(self):
        result, _, state = self.run_duo([
            self.reply("worker", "blocked"), self.reply("lead", "blocked")])
        self.assertEqual(result.returncode, 2)
        self.assertEqual(state["status"], "blocked")

    def test_round_limit(self):
        result, _, state = self.run_duo([
            self.reply("worker", "needs_decision"), self.reply("lead", "continue")],
            extra=("--max-rounds", "1"))
        self.assertEqual(result.returncode, 2)
        self.assertEqual(state["status"], "round_limit")

    def test_timeout(self):
        reply = self.reply("worker", "ready_for_review")
        reply["sleep"] = 10
        result, _, state = self.run_duo([reply], extra=("--timeout", "1"))
        self.assertEqual(result.returncode, 1)
        self.assertEqual(state["status"], "error")


if __name__ == "__main__":
    unittest.main()
