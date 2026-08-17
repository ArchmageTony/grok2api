#!/usr/bin/env python3
"""Standalone degraded-account guard for grok2api.

Reads passive request audits through the quality-guard internal API and
attributes TPS anomalies to accounts. Strategy mirrors the upstream-side
enhancement distribution:

- every degrade hit force-switches the account: it is disabled for a short
  hold (default 120s) so the scheduler picks another account, then restored
  automatically;
- 3 hits within a rolling 24h window mute the account long-term; a muted
  account is never re-enabled by this guard and needs an operator.

Deliberate deviation from the enhancement sidecar: arbitrary user traffic is
never classified as missing_thinking. Reasoning tokens can be legitimately
absent (non-reasoning model, reasoning disabled per request), so only the
panel-equivalent TPS thresholds count as account degrade hits.

The process makes outbound calls only: the internal audit API (quality-guard
token from bootstrap.json) and the admin API (login with operator-injected
credentials). It exposes no ports and touches only its own state file.
"""

from __future__ import annotations

import fcntl
import json
import os
import ssl
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import Any

INTERNAL_API_PREFIX = "/api/internal/v1/quality-guard"
BOOTSTRAP_FILE = Path(os.environ.get("GROK2API_BOOTSTRAP_FILE") or "/var/lib/grok2api-quality-guard/bootstrap.json")
STATE_DIR = Path(os.environ.get("ACCOUNT_GUARD_STATE_DIR") or "/var/lib/grok2api-account-guard")

DEFAULT_WINDOW_SECONDS = 24 * 60 * 60
DEFAULT_MUTE_AFTER = 3
DEFAULT_FORCE_SWITCH_SECONDS = 120
MIN_OUTPUT_TOKENS = 32
MAX_SEEN_AUDIT_IDS = 2000
PASSIVE_PAGE_SIZE = 200
PASSIVE_MAX_PAGES = 10


def _env_bool(name: str, default: bool) -> bool:
    raw = os.environ.get(name)
    if raw is None or not raw.strip():
        return default
    return raw.strip().lower() in {"1", "true", "yes", "on"}


def _env_int(name: str, default: int, minimum: int, maximum: int) -> int:
    raw = os.environ.get(name)
    if raw is None or not raw.strip():
        return default
    try:
        value = int(raw)
    except ValueError:
        return default
    return max(minimum, min(maximum, value))


def log_event(event: str, **fields: Any) -> None:
    line = {"ts": round(time.time(), 3), "event": event}
    line.update(fields)
    print(json.dumps(line, ensure_ascii=False, separators=(",", ":")), flush=True)


class ApiError(Exception):
    def __init__(self, code: int, error_code: str, message: str) -> None:
        super().__init__(f"HTTP {code} {error_code}: {message}")
        self.code = code
        self.error_code = error_code


@dataclass
class Config:
    base_url: str
    internal_token: str
    soft_tps: float
    hard_tps: float
    poll_seconds: int
    request_timeout_seconds: int
    admin_username: str
    admin_password: str
    provider: str
    window_seconds: int
    mute_after: int
    force_switch_enabled: bool
    force_switch_seconds: int
    state_file: Path
    lock_file: Path

    @classmethod
    def load(cls) -> "Config":
        with BOOTSTRAP_FILE.open("r", encoding="utf-8") as handle:
            payload = json.load(handle)
        if not isinstance(payload, dict) or payload.get("version") != 1:
            raise ValueError("unsupported quality guard bootstrap")
        values = payload.get("config")
        if not isinstance(values, dict):
            raise ValueError("quality guard bootstrap config is missing")
        # 管理页热保存的运行时策略覆盖 bootstrap 中的静态值, 与质量守护同口径。
        runtime_path = BOOTSTRAP_FILE.with_name("runtime-config.json")
        try:
            with runtime_path.open("r", encoding="utf-8") as handle:
                runtime = json.load(handle)
            settings = runtime.get("settings") if isinstance(runtime, dict) else None
            if isinstance(settings, dict):
                for key in ("soft_tps", "hard_tps", "passive_poll_seconds"):
                    if settings.get(key) is not None:
                        values = {**values, key: settings[key]}
        except (OSError, ValueError):
            pass
        token = str(payload.get("internal_token") or "").strip()
        if not token:
            raise ValueError("bootstrap internal_token is missing")
        password = os.environ.get("GROK2API_ADMIN_PASSWORD", "") or ""
        password_file = (os.environ.get("GROK2API_ADMIN_PASSWORD_FILE") or "").strip()
        if not password and password_file:
            password = Path(password_file).read_text(encoding="utf-8").rstrip("\r\n")
        poll = int(values.get("passive_poll_seconds") or 0)
        return cls(
            base_url=(os.environ.get("GROK2API_BASE_URL") or "http://grok2api:8000").strip(),
            internal_token=token,
            soft_tps=float(values.get("soft_tps") or 0),
            hard_tps=float(values.get("hard_tps") or 0),
            poll_seconds=max(5, min(300, poll or 5)),
            request_timeout_seconds=60,
            admin_username=(os.environ.get("GROK2API_ADMIN_USERNAME") or "").strip(),
            admin_password=password,
            provider=(os.environ.get("ACCOUNT_GUARD_PROVIDER") or "grok_build").strip(),
            window_seconds=_env_int("ACCOUNT_GUARD_WINDOW_SECONDS", DEFAULT_WINDOW_SECONDS, 3600, 7 * 86400),
            mute_after=_env_int("ACCOUNT_GUARD_MUTE_AFTER", DEFAULT_MUTE_AFTER, 2, 100),
            force_switch_enabled=_env_bool("ACCOUNT_GUARD_FORCE_SWITCH_ENABLED", True),
            force_switch_seconds=_env_int("ACCOUNT_GUARD_FORCE_SWITCH_SECONDS", DEFAULT_FORCE_SWITCH_SECONDS, 30, 900),
            state_file=STATE_DIR / "state.json",
            lock_file=STATE_DIR / "guard.lock",
        )


class HttpClient:
    def __init__(self, base_url: str, timeout_seconds: int) -> None:
        self.base_url = base_url.rstrip("/")
        self.timeout_seconds = timeout_seconds
        self.ssl_context = ssl.create_default_context()

    def request(self, method: str, path: str, token: str, body: dict[str, Any] | None = None) -> Any:
        data = None if body is None else json.dumps(body, separators=(",", ":")).encode()
        headers = {"Accept": "application/json"}
        if data is not None:
            headers["Content-Type"] = "application/json"
        if token:
            headers["Authorization"] = f"Bearer {token}"
        request = urllib.request.Request(self.base_url + path, data=data, headers=headers, method=method)
        try:
            with self._open(request) as response:
                payload = json.load(response)
        except urllib.error.HTTPError as exc:
            try:
                payload = json.loads(exc.read().decode("utf-8", "replace"))
            except (ValueError, OSError):
                payload = {}
            error = payload.get("error") or {}
            raise ApiError(exc.code, str(error.get("code", "request_failed")), str(error.get("message", "request failed"))) from exc
        except (urllib.error.URLError, TimeoutError, OSError, ValueError) as exc:
            raise RuntimeError(f"request failed: {type(exc).__name__}") from exc
        return payload.get("data", payload)

    def _open(self, request: urllib.request.Request):
        if request.full_url.startswith("https://"):
            return urllib.request.urlopen(request, timeout=self.timeout_seconds, context=self.ssl_context)
        return urllib.request.urlopen(request, timeout=self.timeout_seconds)


class AdminApi:
    """Admin API, used only for disabling/restoring degraded accounts."""

    def __init__(self, http: HttpClient, username: str, password: str) -> None:
        self.http = http
        self.username = username
        self.password = password
        self.token = ""

    @property
    def available(self) -> bool:
        return bool(self.username and self.password)

    def _ensure_token(self) -> None:
        if self.token:
            return
        payload = self.http.request("POST", "/api/admin/v1/auth/login", "", {
            "username": self.username,
            "password": self.password,
        })
        token = ""
        if isinstance(payload, dict):
            token = str(payload.get("token") or "")
            tokens = payload.get("tokens")
            if not token and isinstance(tokens, dict):
                token = str(tokens.get("accessToken") or tokens.get("token") or "")
            session = payload.get("session")
            if not token and isinstance(session, dict):
                token = str(session.get("token") or session.get("accessToken") or "")
        if not token:
            raise RuntimeError("admin login did not return a token")
        self.token = token

    def set_accounts_enabled(self, account_ids: list[str], enabled: bool, provider: str) -> int:
        if not account_ids:
            return 0
        self._ensure_token()
        try:
            result = self.http.request("PATCH", "/api/admin/v1/accounts/batch", self.token, {
                "ids": [str(value) for value in account_ids],
                "provider": provider,
                "enabled": enabled,
            })
        except ApiError as exc:
            if exc.code == 401:
                self.token = ""
                self._ensure_token()
                result = self.http.request("PATCH", "/api/admin/v1/accounts/batch", self.token, {
                    "ids": [str(value) for value in account_ids],
                    "provider": provider,
                    "enabled": enabled,
                })
            else:
                raise
        return int(result.get("updated") or 0)


def classify_audit(value: dict[str, Any], config: Config) -> tuple[str, float]:
    """Return ("", 0) to ignore, or (reason, speed) for a degrade hit."""
    if bool(value.get("qualityProbe")):
        return "", 0.0
    if value.get("provider") != "grok_build" or not bool(value.get("streaming")):
        return "", 0.0
    status = int(value.get("statusCode") or 0)
    if status < 200 or status >= 300 or value.get("errorCode"):
        return "", 0.0
    account_id = str(value.get("accountId") or "").strip()
    if not account_id.isdigit() or int(account_id) < 1:
        return "", 0.0
    first_token_ms = value.get("firstTokenMs")
    if first_token_ms is None:
        return "", 0.0
    generation_ms = int(value.get("durationMs") or 0) - int(first_token_ms)
    output_tokens = max(0, int(value.get("outputTokens") or 0))
    if generation_ms <= 0 or output_tokens < MIN_OUTPUT_TOKENS:
        return "", 0.0
    speed = float(output_tokens) * 1000 / float(generation_ms)
    if config.hard_tps > 0 and speed >= config.hard_tps:
        return "hard_tps", speed
    if config.soft_tps > 0 and speed >= config.soft_tps:
        return "soft_tps", speed
    return "", speed


class AccountGuard:
    def __init__(self, config: Config) -> None:
        self.config = config
        self.http = HttpClient(config.base_url, config.request_timeout_seconds)
        self.admin = AdminApi(self.http, config.admin_username, config.admin_password)
        self.state: dict[str, Any] = {"version": 1, "initialized": False, "seen_audit_ids": [], "accounts": {}}
        self._load_state()

    def _load_state(self) -> None:
        try:
            with self.config.state_file.open("r", encoding="utf-8") as handle:
                data = json.load(handle)
        except (OSError, ValueError):
            return
        if isinstance(data, dict) and data.get("version") == 1:
            self.state = data

    def _save_state(self) -> None:
        self.config.state_file.parent.mkdir(parents=True, exist_ok=True)
        fd, temp_path = tempfile.mkstemp(dir=str(self.config.state_file.parent), prefix=".state-", suffix=".tmp")
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as handle:
                json.dump(self.state, handle, ensure_ascii=False)
            os.chmod(temp_path, 0o600)
            os.replace(temp_path, self.config.state_file)
        except BaseException:
            try:
                os.unlink(temp_path)
            except OSError:
                pass
            raise

    def _fetch_new_audits(self) -> list[dict[str, Any]]:
        known = set(str(value) for value in self.state.get("seen_audit_ids") or [])
        collected: list[dict[str, Any]] = []
        fetched_ids: list[str] = []
        cursor = ""
        for _ in range(PASSIVE_MAX_PAGES):
            query: dict[str, Any] = {"pagination": "cursor", "pageSize": PASSIVE_PAGE_SIZE, "period": "24h"}
            if cursor:
                query["cursor"] = cursor
            page = self.http.request("GET", f"{INTERNAL_API_PREFIX}/request-audits?{urllib.parse.urlencode(query)}", self.config.internal_token)
            items = list(page.get("items") or [])
            if not items:
                break
            reached_known = False
            for item in items:
                audit_id = str(item.get("id") or "").strip()
                if not audit_id:
                    continue
                fetched_ids.append(audit_id)
                if audit_id in known:
                    reached_known = True
                    break
                collected.append(item)
            if reached_known or not page.get("hasMore"):
                break
            cursor = str(page.get("nextCursor") or "")
            if not cursor:
                break
        combined: list[str] = []
        seen: set[str] = set()
        for audit_id in [*fetched_ids, *self.state.get("seen_audit_ids", [])]:
            audit_id = str(audit_id)
            if audit_id and audit_id not in seen:
                seen.add(audit_id)
                combined.append(audit_id)
            if len(combined) >= MAX_SEEN_AUDIT_IDS:
                break
        self.state["seen_audit_ids"] = combined
        if not self.state.get("initialized"):
            self.state["initialized"] = True
            log_event("baseline_initialized", audit_count=len(fetched_ids))
            return []
        collected.reverse()
        return collected

    def _account_entry(self, account_id: str) -> dict[str, Any]:
        accounts = self.state.setdefault("accounts", {})
        entry = accounts.setdefault(account_id, {})
        if not isinstance(entry, dict):
            entry = {}
            accounts[account_id] = entry
        return entry

    def _record_hit(self, account_id: str, reason: str, speed: float, now: float) -> None:
        entry = self._account_entry(account_id)
        cutoff = now - self.config.window_seconds
        hits = [float(value) for value in entry.get("hits", []) if isinstance(value, (int, float)) and float(value) >= cutoff]
        hits.append(now)
        entry["hits"] = hits
        entry["last_reason"] = reason
        entry["last_tps"] = round(speed, 3)
        entry["last_at"] = now
        if len(hits) >= self.config.mute_after and float(entry.get("muted_at") or 0) <= 0:
            self._mute_account(account_id, reason, len(hits))

    def _mute_account(self, account_id: str, reason: str, hits: int) -> None:
        entry = self._account_entry(account_id)
        if not self.admin.available:
            log_event("account_auto_mute_skipped", account_id=account_id, reason=reason, hits=hits, error_type="no_admin_credentials")
            return
        try:
            updated = self.admin.set_accounts_enabled([account_id], False, self.config.provider)
        except Exception as exc:
            log_event("account_auto_mute_failed", account_id=account_id, reason=reason, hits=hits, error_type=type(exc).__name__)
            return
        entry["muted_at"] = time.time()
        entry.pop("forced_until", None)
        log_event("account_auto_muted", account_id=account_id, reason=reason, hits=hits, updated=updated)

    def _force_switch(self, account_id: str, reason: str, now: float) -> None:
        """Temporarily disable a degraded account so the next turn picks another."""
        if not self.config.force_switch_enabled:
            return
        entry = self._account_entry(account_id)
        if float(entry.get("muted_at") or 0) > 0:
            return
        hold = self.config.force_switch_seconds
        current_until = float(entry.get("forced_until") or 0)
        if current_until > now:
            entry["forced_until"] = max(current_until, now + hold)
            return
        if not self.admin.available:
            log_event("account_force_switch_skipped", account_id=account_id, reason=reason, error_type="no_admin_credentials")
            return
        try:
            updated = self.admin.set_accounts_enabled([account_id], False, self.config.provider)
        except Exception as exc:
            log_event("account_force_switch_failed", account_id=account_id, reason=reason, error_type=type(exc).__name__)
            return
        entry["forced_until"] = now + hold
        entry["forced_reason"] = reason
        log_event("account_force_switched", account_id=account_id, reason=reason, hold_seconds=hold, updated=updated)

    def _restore_expired(self, now: float) -> None:
        accounts = self.state.get("accounts") or {}
        if not isinstance(accounts, dict):
            return
        expired = [
            (str(account_id), entry)
            for account_id, entry in accounts.items()
            if isinstance(entry, dict)
            and float(entry.get("forced_until") or 0) > 0
            and float(entry.get("forced_until") or 0) <= now
        ]
        if not expired or not self.admin.available:
            return
        for account_id, entry in expired:
            if float(entry.get("muted_at") or 0) > 0:
                entry.pop("forced_until", None)
                continue
            try:
                updated = self.admin.set_accounts_enabled([account_id], True, self.config.provider)
            except Exception as exc:
                log_event("account_force_switch_restore_failed", account_id=account_id, error_type=type(exc).__name__)
                continue
            entry.pop("forced_until", None)
            log_event("account_force_switch_restored", account_id=account_id, updated=updated)

    def _reload_policy(self) -> None:
        """每轮重新读取 bootstrap 与运行时策略, 跟随管理页热保存的阈值。"""
        try:
            fresh = Config.load()
        except (ValueError, OSError) as exc:
            log_event("policy_reload_failed", error=str(exc)[:200])
            return
        self.config.soft_tps = fresh.soft_tps
        self.config.hard_tps = fresh.hard_tps
        self.config.poll_seconds = fresh.poll_seconds
        self.config.internal_token = fresh.internal_token

    def run_cycle(self) -> None:
        self._reload_policy()
        now = time.time()
        self._restore_expired(now)
        for audit_value in self._fetch_new_audits():
            reason, speed = classify_audit(audit_value, self.config)
            if not reason:
                continue
            account_id = str(audit_value.get("accountId") or "").strip()
            log_event(
                "degrade_hit",
                account_id=account_id,
                node_id=str(audit_value.get("egressNodeId") or ""),
                reason=reason,
                output_tps=round(speed, 3),
                request_id=str(audit_value.get("requestId") or ""),
            )
            self._record_hit(account_id, reason, speed, now)
            self._force_switch(account_id, reason, now)
        self._save_state()

    def run(self) -> None:
        log_event(
            "guard_started",
            base_url=self.config.base_url,
            soft_tps=self.config.soft_tps,
            hard_tps=self.config.hard_tps,
            window_seconds=self.config.window_seconds,
            mute_after=self.config.mute_after,
            force_switch_enabled=self.config.force_switch_enabled,
            force_switch_seconds=self.config.force_switch_seconds,
            admin_configured=self.admin.available,
        )
        while True:
            started = time.time()
            try:
                self.run_cycle()
            except Exception as exc:
                log_event("cycle_failed", error_type=type(exc).__name__, error=str(exc)[:200])
            elapsed = time.time() - started
            time.sleep(max(1.0, self.config.poll_seconds - elapsed))


def main() -> int:
    try:
        config = Config.load()
    except (ValueError, OSError) as exc:
        log_event("config_invalid", error=str(exc))
        return 2
    config.state_file.parent.mkdir(parents=True, exist_ok=True)
    lock_handle = config.lock_file.open("w")
    try:
        fcntl.flock(lock_handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError:
        log_event("lock_held", lock_file=str(config.lock_file))
        return 3
    AccountGuard(config).run()
    return 0


if __name__ == "__main__":
    sys.exit(main())
