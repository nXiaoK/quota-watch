#!/usr/bin/env python3
"""Seed and control the isolated, real Sub2API database used for local tests."""

import argparse
from datetime import datetime, timedelta, timezone
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile


DATABASE = "sub2api_quota_watch_local"
HOST = "127.0.0.1"
PORT = 15432
DATABASE_USER = "quota_watch_lab"
ADMIN_EMAIL = "admin@quota-watch.local"
USER_EMAIL = "demo@quota-watch.local"
ACCOUNT_NAME = "本地 Codex 归零测试账号"
GROUP_NAME = "本地 Codex 重置测试组"
INSTANCE_KEY = "quota_watch_local_test_instance"
DATASET_KEY = "quota_watch_local_test_dataset"
DEFAULT_API_KEY = "local-test-admin-key"
MAIN_URL = "http://127.0.0.1:8092"
MONITOR_URL = "http://127.0.0.1:8091"


class LabError(Exception):
    pass


def timestamp(value):
    return value.astimezone(timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")


def encode_json(value):
    return json.dumps(value, ensure_ascii=False, separators=(",", ":"), allow_nan=False)


class DatabaseClient:
    def __init__(self, executable):
        self.executable = executable

    def query(self, sql, variables=None):
        command = [
            self.executable, "--no-psqlrc", "--no-password", "--quiet",
            "--tuples-only", "--no-align", "--host", HOST, "--port", str(PORT),
            "--username", DATABASE_USER, "--dbname", DATABASE,
            "--set", "ON_ERROR_STOP=1",
        ]
        for key, value in (variables or {}).items():
            command.extend(["--set", key + "=" + str(value)])
        command.extend(["--file", "-"])
        # Ignore the caller's PostgreSQL service, password, and connection settings.
        environment = {key: os.environ[key] for key in ("PATH", "LANG", "LC_ALL", "TMPDIR") if key in os.environ}
        environment.update({
            "PGCONNECT_TIMEOUT": "5", "PGSSLMODE": "disable",
            "PGAPPNAME": "quota-watch-local-data", "PGPASSFILE": os.devnull,
        })
        try:
            result = subprocess.run(
                command, input=sql, text=True, capture_output=True,
                env=environment, timeout=30, check=False,
            )
        except FileNotFoundError:
            raise LabError("psql 未找到，请通过 --psql 指定本机 PostgreSQL 的 psql 路径") from None
        except subprocess.TimeoutExpired:
            raise LabError("本地数据库操作超时；请检查隔离测试数据库是否已启动") from None
        if result.returncode != 0:
            detail = result.stderr.strip()[-1200:]
            raise LabError("隔离数据库操作失败：" + detail)
        try:
            return json.loads(result.stdout.strip())
        except json.JSONDecodeError:
            raise LabError("psql 未返回预期的 JSON 结果") from None

    def verify(self):
        result = self.query("""
SELECT jsonb_build_object(
  'database', current_database(),
  'marker', (SELECT value FROM settings WHERE key = :'instance_key'),
  'admin_count', (SELECT COUNT(*) FROM users WHERE role = 'admin'),
  'admin_email', (SELECT email FROM users WHERE role = 'admin' LIMIT 1),
  'dataset', (SELECT value::jsonb FROM settings WHERE key = :'dataset_key')
);
""", {"instance_key": INSTANCE_KEY, "dataset_key": DATASET_KEY})
        if (result.get("database") != DATABASE or result.get("marker") != "true"
                or result.get("admin_count") != 1 or result.get("admin_email") != ADMIN_EMAIL):
            raise LabError("拒绝操作：数据库名称、本地测试标记或测试管理员身份不匹配")
        return result


TRANSACTION_GUARD = r"""
BEGIN;
SELECT pg_advisory_xact_lock(1543208092) \gset
CREATE TEMP TABLE local_test_guard (ok BOOLEAN NOT NULL CHECK (ok)) ON COMMIT DROP;
INSERT INTO local_test_guard SELECT
  current_database() = :'database'
  AND COALESCE((SELECT value = 'true' FROM settings WHERE key = :'instance_key'), FALSE)
  AND (SELECT COUNT(*) = 1 FROM users WHERE role = 'admin')
  AND (SELECT COUNT(*) = 1 FROM users WHERE role = 'admin' AND email = :'admin_email');
"""


def base_variables():
    return {"database": DATABASE, "instance_key": INSTANCE_KEY,
            "dataset_key": DATASET_KEY, "admin_email": ADMIN_EMAIL}


def validate_manifest(value):
    if not isinstance(value, dict):
        raise LabError("无效的本地测试数据清单")
    expected = {"version": 1, "database": DATABASE, "host": HOST,
                "port": PORT, "user": DATABASE_USER,
                "main_url": MAIN_URL, "monitor_url": MONITOR_URL}
    for key, expected_value in expected.items():
        if type(value.get(key)) is not type(expected_value) or value[key] != expected_value:
            raise LabError("测试数据清单的数据库身份或版本不匹配")
    for key in ("admin_user_id", "account_id", "user_id", "group_id"):
        if type(value.get(key)) is not int or value[key] <= 0:
            raise LabError("测试数据清单缺少有效的数据 ID")
    ids = value.get("subscription_ids")
    if not isinstance(ids, list) or len(ids) != 1 or type(ids[0]) is not int or ids[0] <= 0:
        raise LabError("测试数据清单必须包含一个有效的测试订阅 ID")
    if set(value) != set(expected) | {"admin_user_id", "account_id", "user_id", "group_id", "subscription_ids"}:
        raise LabError("测试数据清单含未知字段")
    return value


def write_manifest(path, value):
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(prefix=path.name + ".", dir=path.parent)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as target:
            os.fchmod(target.fileno(), 0o600)
            json.dump(value, target, ensure_ascii=False, indent=2, allow_nan=False)
            target.write("\n")
            target.flush()
            os.fsync(target.fileno())
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def load_manifest(path, registered):
    try:
        with path.open(encoding="utf-8") as source:
            manifest = validate_manifest(json.load(source))
    except FileNotFoundError:
        raise LabError("本地数据清单不存在，请先执行 seed") from None
    except (OSError, json.JSONDecodeError) as error:
        raise LabError("无法读取本地数据清单：" + str(error)) from None
    if manifest != validate_manifest(registered):
        raise LabError("文件清单与隔离数据库的数据登记不一致，拒绝操作")
    return manifest


def dataset_variables(manifest):
    return {**base_variables(), "manifest": encode_json(manifest),
            "account_id": manifest["account_id"], "user_id": manifest["user_id"],
            "group_id": manifest["group_id"], "subscription_id": manifest["subscription_ids"][0],
            "user_email": USER_EMAIL, "account_name": ACCOUNT_NAME, "group_name": GROUP_NAME}


DATASET_GUARD = """
INSERT INTO local_test_guard SELECT
  COALESCE((SELECT value::jsonb = :'manifest'::jsonb FROM settings WHERE key = :'dataset_key'), FALSE)
  AND EXISTS (SELECT 1 FROM accounts WHERE id = :'account_id'::bigint
    AND name = :'account_name' AND platform = 'openai' AND type = 'oauth' AND deleted_at IS NULL
    AND extra->'synthetic_ui_test' = 'true'::jsonb
    AND NOT (credentials ?| ARRAY['access_token', 'refresh_token']))
  AND EXISTS (SELECT 1 FROM users WHERE id = :'user_id'::bigint AND email = :'user_email'
    AND role = 'user' AND deleted_at IS NULL)
  AND EXISTS (SELECT 1 FROM groups WHERE id = :'group_id'::bigint AND name = :'group_name'
    AND platform = 'openai' AND subscription_type = 'subscription' AND deleted_at IS NULL)
  AND EXISTS (SELECT 1 FROM user_subscriptions WHERE id = :'subscription_id'::bigint
    AND user_id = :'user_id'::bigint AND group_id = :'group_id'::bigint AND deleted_at IS NULL);
"""


STATUS_SQL = """
SELECT jsonb_build_object(
  'database', current_database(),
  'main_url', :'main_url',
  'account', (SELECT jsonb_build_object(
    'id', id, 'name', name, 'status', status, 'schedulable', schedulable,
    'used_percent', extra->'codex_7d_used_percent',
    'sampled_at', extra->'codex_usage_updated_at', 'reset_at', extra->'codex_7d_reset_at'
  ) FROM accounts WHERE id = :'account_id'::bigint),
  'subscriptions', (SELECT jsonb_agg(jsonb_build_object(
    'id', id, 'user_id', user_id, 'group_id', group_id, 'status', status,
    'daily_usage_usd', daily_usage_usd, 'weekly_usage_usd', weekly_usage_usd,
    'monthly_usage_usd', monthly_usage_usd, 'daily_window_start', daily_window_start,
    'weekly_window_start', weekly_window_start, 'monthly_window_start', monthly_window_start,
    'expires_at', expires_at
  )) FROM user_subscriptions WHERE id = :'subscription_id'::bigint)
);
"""


def seed(client, manifest_path, registered):
    if registered is not None:
        manifest = validate_manifest(registered)
        if manifest_path.exists():
            load_manifest(manifest_path, registered)
        read_status(client, manifest)
        write_manifest(manifest_path, manifest)
        return manifest
    if manifest_path.exists():
        raise LabError("清单文件已存在，但数据库没有数据登记；拒绝自动替换旧数据")
    now = datetime.now(timezone.utc).replace(microsecond=0)
    extra = {
        "synthetic_ui_test": True, "auto_reset_credit_enabled": False,
        "openai_oauth_responses_websockets_v2_enabled": False,
        "responses_websockets_v2_enabled": False, "openai_ws_enabled": False,
        "codex_5h_used_percent": 12, "codex_5h_window_minutes": 300,
        "codex_5h_reset_at": timestamp(now + timedelta(hours=5)),
        "codex_7d_used_percent": 40, "codex_7d_window_minutes": 10080,
        "codex_7d_reset_at": timestamp(now + timedelta(days=7)),
        "codex_usage_updated_at": timestamp(now),
    }
    settings = {
        "admin_api_key": DEFAULT_API_KEY, "site_name": "Sub2API 本地额度测试",
        "openai_codex_version_auto_sync_enabled": "false",
        "claude_code_version_auto_sync_enabled": "false",
        "channel_monitor_enabled": "false",
        "upstream_billing_probe_settings": encode_json({"enabled": False, "interval_minutes": 60}),
    }
    variables = {**base_variables(), "user_email": USER_EMAIL, "account_name": ACCOUNT_NAME,
                 "group_name": GROUP_NAME, "credentials": encode_json({
                     "chatgpt_account_id": "local-quota-watch-1", "plan_type": "plus"}),
                 "extra": encode_json(extra), "settings": encode_json(settings),
                 "host": HOST, "port": PORT, "database_user": DATABASE_USER,
                 "main_url": MAIN_URL, "monitor_url": MONITOR_URL}
    sql = TRANSACTION_GUARD + """
INSERT INTO local_test_guard SELECT
  NOT EXISTS (SELECT 1 FROM accounts)
  AND NOT EXISTS (SELECT 1 FROM user_subscriptions)
  AND (SELECT COUNT(*) = 1 FROM users)
  AND NOT EXISTS (SELECT 1 FROM groups WHERE name <> 'default' OR deleted_at IS NOT NULL)
  AND NOT EXISTS (SELECT 1 FROM settings WHERE key = :'dataset_key');
INSERT INTO settings (key, value, updated_at)
SELECT key, value, NOW() FROM jsonb_each_text(:'settings'::jsonb)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at;
WITH test_group AS (
  INSERT INTO groups (name, description, platform, subscription_type, status,
    rate_multiplier, daily_limit_usd, weekly_limit_usd, monthly_limit_usd, default_validity_days)
  VALUES (:'group_name', '仅本地测试数据', 'openai', 'subscription', 'active', 1, 30, 100, 300, 30)
  RETURNING id
), test_user AS (
  INSERT INTO users (email, password_hash, username, role, status, balance, concurrency)
  SELECT :'user_email', password_hash, '本地测试用户', 'user', 'active', 0, 5
  FROM users WHERE role = 'admin' AND email = :'admin_email'
  RETURNING id
), test_account AS (
  INSERT INTO accounts (name, notes, platform, type, credentials, extra,
    status, schedulable, concurrency, priority, rate_multiplier, quota_dimension)
  VALUES (:'account_name', '仅本地快照；不包含 OAuth Token', 'openai', 'oauth',
    :'credentials'::jsonb, :'extra'::jsonb, 'active', FALSE, 1, 50, 1, 'global')
  RETURNING id
), account_group AS (
  INSERT INTO account_groups (account_id, group_id, priority)
  SELECT test_account.id, test_group.id, 50 FROM test_account CROSS JOIN test_group
  RETURNING account_id
), test_subscription AS (
  INSERT INTO user_subscriptions (user_id, group_id, starts_at, expires_at, status,
    daily_window_start, weekly_window_start, monthly_window_start,
    daily_usage_usd, weekly_usage_usd, monthly_usage_usd, assigned_by, assigned_at, notes)
  SELECT test_user.id, test_group.id, NOW() - INTERVAL '1 day', NOW() + INTERVAL '30 days',
    'active', date_trunc('day', NOW() AT TIME ZONE 'Asia/Shanghai') AT TIME ZONE 'Asia/Shanghai',
    NOW() - INTERVAL '1 day', NOW() - INTERVAL '1 day', 5, 20, 30,
    (SELECT id FROM users WHERE role = 'admin' AND email = :'admin_email'), NOW(), '本地订阅重置测试'
  FROM test_user CROSS JOIN test_group
  RETURNING id
), dataset AS (
  SELECT jsonb_build_object(
    'version', 1, 'database', current_database(), 'host', :'host', 'port', :'port'::int,
    'user', :'database_user', 'main_url', :'main_url', 'monitor_url', :'monitor_url',
    'admin_user_id', (SELECT id FROM users WHERE role = 'admin' AND email = :'admin_email'),
    'account_id', test_account.id, 'user_id', test_user.id, 'group_id', test_group.id,
    'subscription_ids', jsonb_build_array(test_subscription.id)
  ) AS value FROM test_account CROSS JOIN test_user CROSS JOIN test_group CROSS JOIN test_subscription
)
INSERT INTO settings (key, value, updated_at)
SELECT :'dataset_key', value::text, NOW() FROM dataset RETURNING value::jsonb;
COMMIT;
"""
    manifest = validate_manifest(client.query(sql, variables))
    write_manifest(manifest_path, manifest)
    return manifest


def read_status(client, manifest):
    variables = {**dataset_variables(manifest), "main_url": MAIN_URL}
    return client.query(TRANSACTION_GUARD + DATASET_GUARD + STATUS_SQL + "COMMIT;\n", variables)


def update_usage(client, manifest, used_percent, restore=False):
    now = datetime.now(timezone.utc).replace(microsecond=0)
    variables = {**dataset_variables(manifest), "main_url": MAIN_URL,
                 "used_percent": used_percent, "sampled_at": timestamp(now),
                 "reset_at": timestamp(now + timedelta(days=7)),
                 "reset_5h_at": timestamp(now + timedelta(hours=5))}
    sql = TRANSACTION_GUARD + DATASET_GUARD + """
UPDATE accounts SET extra = extra || jsonb_build_object(
  'codex_7d_used_percent', :'used_percent'::int,
  'codex_usage_updated_at', TO_CHAR(
    GREATEST(:'sampled_at'::timestamptz,
      (extra->>'codex_usage_updated_at')::timestamptz + INTERVAL '1 second') AT TIME ZONE 'UTC',
    'YYYY-MM-DD"T"HH24:MI:SS"Z"')
), updated_at = NOW() WHERE id = :'account_id'::bigint;
"""
    if restore:
        sql += """
UPDATE accounts SET extra = extra || jsonb_build_object(
  'codex_7d_reset_at', :'reset_at', 'codex_5h_used_percent', 12,
  'codex_5h_reset_at', :'reset_5h_at'
), updated_at = NOW() WHERE id = :'account_id'::bigint;
UPDATE user_subscriptions SET daily_usage_usd = 5, weekly_usage_usd = 20, monthly_usage_usd = 30,
  daily_window_start = date_trunc('day', NOW() AT TIME ZONE 'Asia/Shanghai') AT TIME ZONE 'Asia/Shanghai',
  weekly_window_start = NOW() - INTERVAL '1 day', monthly_window_start = NOW() - INTERVAL '1 day',
  updated_at = NOW() WHERE id = :'subscription_id'::bigint;
"""
    return client.query(sql + STATUS_SQL + "COMMIT;\n", variables)


def main(argv=None):
    parser = argparse.ArgumentParser(description="控制隔离真实 Sub2API 测试站的数据，不访问上游")
    local_psql = Path("/opt/homebrew/opt/postgresql@18/bin/psql")
    parser.add_argument("--psql", default=str(local_psql) if local_psql.exists() else "psql",
                        help="本机 psql 可执行文件路径")
    parser.add_argument("--lab-dir", type=Path,
                        default=Path(__file__).resolve().parents[1] / "data" / "real-site")
    parser.add_argument("--manifest", type=Path, help="覆盖 --lab-dir/lab.json 的清单路径")
    actions = parser.add_subparsers(dest="action", required=True)
    actions.add_parser("seed", help="仅向空的已初始化测试站添加一次测试数据")
    usage = actions.add_parser("usage", help="模拟数据库快照的 7d 使用率")
    usage.add_argument("used_percent", type=int, choices=(0, 40))
    actions.add_parser("status", help="查看测试账号和订阅的当前数据库数据")
    actions.add_parser("restore", help="恢复 40%% 基线和订阅日 5 / 周 20 / 月 30 用量")
    options = parser.parse_args(argv)
    manifest_path = options.manifest or options.lab_dir / "lab.json"
    client = DatabaseClient(options.psql)
    try:
        identity = client.verify()
        if options.action == "seed":
            manifest = seed(client, manifest_path, identity["dataset"])
            result = {"manifest": manifest, "status": read_status(client, manifest)}
        else:
            manifest = load_manifest(manifest_path, identity["dataset"])
            if options.action == "status":
                result = read_status(client, manifest)
            else:
                percent = 40 if options.action == "restore" else options.used_percent
                result = update_usage(client, manifest, percent, options.action == "restore")
        print(json.dumps(result, ensure_ascii=False, indent=2, allow_nan=False))
        return 0
    except (LabError, OSError) as error:
        print(str(error), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
