"""用假的 CLI 验证真实调度流程，不发送模型请求。"""
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "scripts/claude_duo.py"


class DuoTest(unittest.TestCase):
    def run_duo(self, replies, rounds=4, extra=()):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "replies.json").write_text(json.dumps(replies))
            fake = root / "claude"
            fake.write_text('''#!/usr/bin/env python3
import json, pathlib, sys
p = pathlib.Path.cwd()
log = p / "calls.json"
calls = json.loads(log.read_text()) if log.exists() else []
calls.append({"args": sys.argv[1:], "prompt": sys.stdin.read()})
log.write_text(json.dumps(calls))
reply = json.loads((p / "replies.json").read_text())[len(calls)-1]
print(json.dumps(reply))
''')
            fake.chmod(0o700)
            result = subprocess.run(
                [sys.executable, str(SCRIPT), "--repo", str(root),
                 "--claude", str(fake), "--max-rounds", str(rounds),
                 "--run-dir", str(root / "run"), *extra, "实现测试任务"],
                capture_output=True, text=True,
            )
            calls = json.loads((root / "calls.json").read_text()) if (root / "calls.json").exists() else []
            state = json.loads((root / "run/state.json").read_text()) if (root / "run/state.json").exists() else {}
            return result, calls, state

    @staticmethod
    def reply(session, status, message="说明"):
        return {"session_id": session, "is_error": False,
                "structured_output": {"status": status, "message": message}}

    def test_decision_resume_and_review(self):
        result, calls, state = self.run_duo([
            self.reply("worker-id", "needs_decision"),
            self.reply("lead-id", "continue", "选择方案 A"),
            self.reply("worker-id", "ready_for_review"),
            self.reply("lead-id", "approved"),
        ])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(calls[2]["args"][calls[2]["args"].index("--resume") + 1], "worker-id")
        self.assertEqual(calls[3]["args"][calls[3]["args"].index("--resume") + 1], "lead-id")
        self.assertIn("选择方案 A", calls[2]["prompt"])
        self.assertEqual(state["status"], "approved")

    def test_no_approval_before_review(self):
        result, _, state = self.run_duo([
            self.reply("worker-id", "needs_decision"),
            self.reply("lead-id", "approved"),
        ])
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(state["status"], "error")

    def test_cli_error_stops(self):
        result, calls, state = self.run_duo([{"is_error": True, "result": "认证失败"}])
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(len(calls), 1)
        self.assertIn("认证失败", state["error"])

    def test_invalid_output_stops(self):
        result, _, state = self.run_duo([{"session_id": "worker-id", "is_error": False}])
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(state["status"], "error")

    def test_blocker_goes_to_lead(self):
        result, calls, state = self.run_duo([
            self.reply("worker-id", "blocked"),
            self.reply("lead-id", "blocked", "需要凭据"),
        ])
        self.assertEqual(result.returncode, 2)
        self.assertEqual(len(calls), 2)
        self.assertEqual(state["status"], "blocked")

    def test_round_limit_is_not_success(self):
        result, _, state = self.run_duo([
            self.reply("worker-id", "needs_decision"),
            self.reply("lead-id", "continue"),
        ], rounds=1)
        self.assertEqual(result.returncode, 2)
        self.assertEqual(state["status"], "round_limit")

    def test_permission_denial_cannot_be_approved(self):
        denied = self.reply("worker-id", "ready_for_review")
        denied["permission_denials"] = [{"tool_name": "Bash"}]
        result, calls, state = self.run_duo([
            denied, self.reply("lead-id", "blocked"),
        ])
        self.assertEqual(result.returncode, 2)
        self.assertIn("权限拒绝", calls[1]["prompt"])
        self.assertEqual(state["worker"]["status"], "blocked")

    def test_session_change_stops(self):
        result, _, state = self.run_duo([
            self.reply("worker-id", "needs_decision"),
            self.reply("lead-id", "continue"),
            self.reply("unexpected-id", "ready_for_review"),
        ])
        self.assertEqual(result.returncode, 1)
        self.assertIn("session_id", state["error"])

    def test_worker_bypass_applies_on_resume_only_to_worker(self):
        result, calls, _ = self.run_duo([
            self.reply("worker-id", "needs_decision"),
            self.reply("lead-id", "continue"),
            self.reply("worker-id", "ready_for_review"),
            self.reply("lead-id", "approved"),
        ], extra=("--worker-bypass-permissions",))
        self.assertEqual(result.returncode, 0, result.stderr)
        for index, call in enumerate(calls):
            if index % 2 == 0:
                self.assertIn("--dangerously-skip-permissions", call["args"])
                self.assertNotIn("dontAsk", call["args"])
            else:
                self.assertNotIn("--dangerously-skip-permissions", call["args"])
                self.assertIn("dontAsk", call["args"])


if __name__ == "__main__":
    unittest.main()
