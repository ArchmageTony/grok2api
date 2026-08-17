#!/usr/bin/env python3
"""Unit tests for account_guard classification and hit accounting."""

import importlib.util
import sys
import tempfile
import time
import unittest
from pathlib import Path

spec = importlib.util.spec_from_file_location(
    "account_guard",
    Path(__file__).with_name("account_guard.py"),
)
account_guard = importlib.util.module_from_spec(spec)
sys.modules["account_guard"] = account_guard
spec.loader.exec_module(account_guard)


def make_config(tmp: str, **overrides) -> account_guard.Config:
    values = dict(
        base_url="http://grok2api:8000",
        internal_token="token",
        soft_tps=500.0,
        hard_tps=1000.0,
        poll_seconds=5,
        request_timeout_seconds=30,
        admin_username="",
        admin_password="",
        provider="grok_build",
        window_seconds=86400,
        mute_after=3,
        force_switch_enabled=True,
        force_switch_seconds=120,
        state_file=Path(tmp) / "state.json",
        lock_file=Path(tmp) / "guard.lock",
    )
    values.update(overrides)
    return account_guard.Config(**values)


def audit(account_id="7", output_tokens=100, duration_ms=10000, first_token_ms=1000, **overrides):
    value = {
        "id": "1",
        "requestId": "req-1",
        "qualityProbe": False,
        "provider": "grok_build",
        "streaming": True,
        "statusCode": 200,
        "outputTokens": output_tokens,
        "reasoningTokens": 50,
        "firstTokenMs": first_token_ms,
        "durationMs": duration_ms,
        "accountId": account_id,
    }
    value.update(overrides)
    return value


class FakeAdmin:
    def __init__(self, available=True):
        self.available = available
        self.calls = []

    def set_accounts_enabled(self, account_ids, enabled, provider):
        self.calls.append((list(account_ids), enabled, provider))
        return len(account_ids)


class ClassifyAuditTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.config = make_config(self.tmp.name)

    def tearDown(self):
        self.tmp.cleanup()

    def test_ignores_guard_own_probe(self):
        reason, _ = account_guard.classify_audit(audit(qualityProbe=True), self.config)
        self.assertEqual(reason, "")

    def test_ignores_non_build_or_non_streaming(self):
        reason, _ = account_guard.classify_audit(audit(provider="grok_web"), self.config)
        self.assertEqual(reason, "")
        reason, _ = account_guard.classify_audit(audit(streaming=False), self.config)
        self.assertEqual(reason, "")

    def test_ignores_failed_and_short_requests(self):
        reason, _ = account_guard.classify_audit(audit(statusCode=500), self.config)
        self.assertEqual(reason, "")
        reason, _ = account_guard.classify_audit(audit(errorCode="upstream"), self.config)
        self.assertEqual(reason, "")
        reason, _ = account_guard.classify_audit(audit(output_tokens=10), self.config)
        self.assertEqual(reason, "")

    def test_ignores_missing_account_or_first_token(self):
        reason, _ = account_guard.classify_audit(audit(accountId=None), self.config)
        self.assertEqual(reason, "")
        reason, _ = account_guard.classify_audit(audit(first_token_ms=None), self.config)
        self.assertEqual(reason, "")

    def test_zero_reasoning_user_traffic_is_a_hit(self):
        # 部署策略为质量优先: 用户流量无推理 Token 也计为降智命中。
        reason, _ = account_guard.classify_audit(audit(reasoningTokens=0), self.config)
        self.assertEqual(reason, "missing_thinking")

    def test_zero_reasoning_short_reply_ignored(self):
        # 短回复不要求推理。
        reason, _ = account_guard.classify_audit(audit(reasoningTokens=0, output_tokens=10), self.config)
        self.assertEqual(reason, "")

    def test_soft_and_hard_thresholds(self):
        # 100 tokens / 9s = 11.1 tps -> healthy
        reason, _ = account_guard.classify_audit(audit(), self.config)
        self.assertEqual(reason, "")
        # 6000 tokens / 9s = 666 tps -> soft
        reason, speed = account_guard.classify_audit(audit(output_tokens=6000), self.config)
        self.assertEqual(reason, "soft_tps")
        # 20000 tokens / 9s = 2222 tps -> hard
        reason, _ = account_guard.classify_audit(audit(output_tokens=20000), self.config)
        self.assertEqual(reason, "hard_tps")

    def test_zero_generation_window_ignored(self):
        reason, _ = account_guard.classify_audit(audit(duration_ms=1000, first_token_ms=1000), self.config)
        self.assertEqual(reason, "")


class HitAccountingTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.config = make_config(self.tmp.name)
        self.guard = account_guard.AccountGuard(self.config)
        self.guard.admin = FakeAdmin()
        self.now = time.time()

    def tearDown(self):
        self.tmp.cleanup()

    def test_force_switch_disables_then_restores_after_hold(self):
        self.guard._force_switch("7", "hard_tps", self.now)
        self.assertEqual(self.guard.admin.calls, [(["7"], False, "grok_build")])
        # hold 未到期不恢复
        self.guard._restore_expired(self.now + 60)
        self.assertEqual(len(self.guard.admin.calls), 1)
        # 到期自动恢复
        self.guard._restore_expired(self.now + 121)
        self.assertEqual(self.guard.admin.calls[-1], (["7"], True, "grok_build"))

    def test_force_switch_extends_hold_on_repeat_hit(self):
        self.guard._force_switch("7", "hard_tps", self.now)
        self.guard._force_switch("7", "hard_tps", self.now + 30)
        # 不重复禁用, 只延长
        self.assertEqual(len(self.guard.admin.calls), 1)
        entry = self.guard.state["accounts"]["7"]
        self.assertEqual(entry["forced_until"], self.now + 150)

    def test_third_hit_mutes_and_skips_restore(self):
        for offset in (0, 10, 20):
            self.guard._record_hit("7", "hard_tps", 2000.0, self.now + offset)
        entry = self.guard.state["accounts"]["7"]
        self.assertGreater(entry["muted_at"], 0)
        # 禁用调用发生
        self.assertIn((["7"], False, "grok_build"), self.guard.admin.calls)
        # 已禁言账号不再 force-switch, 也不会被恢复
        self.guard._force_switch("7", "hard_tps", self.now + 30)
        self.guard.state["accounts"]["7"]["forced_until"] = self.now + 31
        self.guard._restore_expired(self.now + 32)
        self.assertNotIn((["7"], True, "grok_build"), self.guard.admin.calls)

    def test_hits_older_than_window_do_not_count(self):
        self.guard._record_hit("7", "hard_tps", 2000.0, self.now - 100000)
        self.guard._record_hit("7", "hard_tps", 2000.0, self.now - 90000)
        self.guard._record_hit("7", "hard_tps", 2000.0, self.now)
        entry = self.guard.state["accounts"]["7"]
        self.assertEqual(len(entry["hits"]), 1)
        self.assertEqual(float(entry.get("muted_at") or 0), 0)

    def test_no_admin_credentials_only_logs(self):
        self.guard.admin = FakeAdmin(available=False)
        for offset in (0, 10, 20):
            self.guard._record_hit("7", "hard_tps", 2000.0, self.now + offset)
        self.assertEqual(self.guard.admin.calls, [])

    def test_state_roundtrip(self):
        self.guard._record_hit("7", "hard_tps", 2000.0, self.now)
        self.guard._save_state()
        clone = account_guard.AccountGuard(self.config)
        self.assertEqual(len(clone.state["accounts"]["7"]["hits"]), 1)


class FetchDedupTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.config = make_config(self.tmp.name)
        self.guard = account_guard.AccountGuard(self.config)

    def tearDown(self):
        self.tmp.cleanup()

    def test_first_run_baselines_without_processing(self):
        pages = [[audit(id="3"), audit(id="2"), audit(id="1")]]
        calls = []

        def fake_request(method, path, token, body=None):
            calls.append(path)
            return {"items": pages.pop(0) if pages else [], "hasMore": False, "nextCursor": ""}

        self.guard.http.request = fake_request
        self.assertEqual(self.guard._fetch_new_audits(), [])
        self.assertTrue(self.guard.state["initialized"])
        # 第二轮只返回新增项
        pages.append([audit(id="4"), audit(id="3")])
        self.guard.http.request = fake_request
        fresh = self.guard._fetch_new_audits()
        self.assertEqual([item["id"] for item in fresh], ["4"])


if __name__ == "__main__":
    unittest.main()
