#!/usr/bin/env python3
"""Local Sub2API fixtures for testing stored 7d snapshots without upstream calls."""

import argparse
import copy
from datetime import datetime, timedelta, timezone
import hashlib
import hmac
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import re
import tempfile
import threading
from urllib.parse import parse_qs, urlsplit


DEFAULT_API_KEY = "local-test-admin-key"
ACCOUNT_ROUTE = re.compile(r"/api/v1/admin/accounts/([1-9][0-9]*)\Z")
BODY_LIMIT = 64 * 1024


class MockError(Exception):
    def __init__(self, status, message):
        super().__init__(message)
        self.status = status


def timestamp(value):
    return value.astimezone(timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")


class MockState:
    def __init__(self, state_file):
        self.path = Path(state_file)
        self.lock = threading.RLock()
        self.recent_requests = []
        self.blocked_requests = []
        if self.path.exists():
            with self.path.open(encoding="utf-8") as source:
                self.data = json.load(source)
            if (not isinstance(self.data, dict) or self.data.get("version") != 1
                    or not isinstance(self.data.get("accounts"), list)
                    or not isinstance(self.data.get("subscriptions"), list)
                    or not isinstance(self.data.get("reset_counts"), dict)
                    or not isinstance(self.data.get("idempotency"), dict)):
                raise ValueError("invalid mock state file; choose a new --state-file")
            os.chmod(self.path, 0o600)
        else:
            now = datetime.now(timezone.utc).replace(microsecond=0)
            self.data = {
                "version": 1,
                "accounts": [{
                    "id": 1, "name": "本地模拟 Codex", "platform": "openai", "type": "oauth",
                    "status": "active", "group_ids": [9], "created_at": timestamp(now),
                    "credentials": {"chatgpt_account_id": "local-fake-account", "plan_type": "plus"},
                    "extra": {
                        "codex_7d_used_percent": 40.0, "codex_7d_window_minutes": 10080,
                        "codex_7d_reset_at": timestamp(now + timedelta(days=7)),
                        "codex_usage_updated_at": timestamp(now),
                    },
                }],
                "subscriptions": [{
                    "id": 1001 + index, "user_id": 2001 + index, "group_id": 9,
                    "status": "active", "starts_at": timestamp(now - timedelta(days=1)),
                    "expires_at": timestamp(now + timedelta(days=365)),
                    "daily_window_start": timestamp(now - timedelta(days=1)),
                    "weekly_window_start": timestamp(now - timedelta(days=1)),
                    "monthly_window_start": timestamp(now - timedelta(days=1)),
                    "daily_usage_usd": 3.0, "weekly_usage_usd": 20.0, "monthly_usage_usd": 80.0,
                    "user": {"id": 2001 + index, "username": "模拟用户 " + str(index + 1),
                             "email": "test" + str(index + 1) + "@example.invalid"},
                    "group": {"id": 9, "name": "本地模拟分组", "platform": "openai"},
                } for index in range(2)],
                "reset_counts": {str(1001 + index): {"total": 0, "daily": 0, "weekly": 0, "monthly": 0}
                                 for index in range(2)},
                "bulk_action_calls": 0,
                "idempotency": {},
            }
            self._write(self.data)

    def _write(self, data):
        self.path.parent.mkdir(parents=True, exist_ok=True)
        descriptor, temporary = tempfile.mkstemp(prefix=self.path.name + ".", dir=self.path.parent)
        try:
            with os.fdopen(descriptor, "w", encoding="utf-8") as target:
                os.fchmod(target.fileno(), 0o600)
                json.dump(data, target, ensure_ascii=False, indent=2, allow_nan=False)
                target.write("\n")
                target.flush()
                os.fsync(target.fileno())
            os.replace(temporary, self.path)
        finally:
            if os.path.exists(temporary):
                os.unlink(temporary)

    def snapshot(self):
        with self.lock:
            data = copy.deepcopy(self.data)
            data["recent_requests"] = copy.deepcopy(self.recent_requests)
            data["blocked_requests"] = copy.deepcopy(self.blocked_requests)
            return data

    def record_request(self, method, path, status, blocked):
        with self.lock:
            item = {"at": timestamp(datetime.now(timezone.utc)), "method": method,
                    "path": path[:300], "status": status, "blocked": blocked}
            self.recent_requests.append(item)
            del self.recent_requests[:-100]
            if blocked:
                self.blocked_requests.append(copy.deepcopy(item))
                del self.blocked_requests[:-100]

    def update_usage(self, body):
        if set(body) != {"account_id", "used_percent"}:
            raise MockError(400, "body must contain only account_id and used_percent")
        account_id, used = body["account_id"], body["used_percent"]
        if type(account_id) is not int or account_id <= 0:
            raise MockError(400, "account_id must be a positive integer")
        if type(used) not in (int, float) or not 0 <= used <= 100:
            raise MockError(400, "used_percent must be a finite number between 0 and 100")
        with self.lock:
            data = copy.deepcopy(self.data)
            account = next((item for item in data["accounts"] if item["id"] == account_id), None)
            if account is None:
                raise MockError(404, "mock account not found")
            extra = account["extra"]
            previous = datetime.fromisoformat(extra["codex_usage_updated_at"].replace("Z", "+00:00"))
            sampled = max(datetime.now(timezone.utc).replace(microsecond=0), previous + timedelta(seconds=1))
            extra["codex_7d_used_percent"] = float(used)
            extra["codex_usage_updated_at"] = timestamp(sampled)
            self._write(data)
            self.data = data
            return copy.deepcopy(account)

    def reset_subscriptions(self, body, key):
        if set(body) - {"subscription_ids", "action", "daily", "weekly", "monthly"}:
            raise MockError(400, "unknown subscription reset field")
        ids = body.get("subscription_ids")
        if (not isinstance(ids, list) or not 1 <= len(ids) <= 100
                or any(type(value) is not int or value <= 0 for value in ids)
                or len(set(ids)) != len(ids)):
            raise MockError(400, "subscription_ids must contain 1 to 100 distinct positive integers")
        mask = {field: body.get(field, False) for field in ("daily", "weekly", "monthly")}
        if body.get("action") != "reset_quota" or any(type(value) is not bool for value in mask.values()) or not any(mask.values()):
            raise MockError(400, "only reset_quota with at least one boolean quota window is allowed")
        if not key or len(key) > 128 or any(ord(value) < 33 or ord(value) > 126 for value in key):
            raise MockError(400, "a valid Idempotency-Key is required")
        payload = {"subscription_ids": ids, "action": "reset_quota", **mask}
        digest = hashlib.sha256(json.dumps(payload, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
        with self.lock:
            stored = self.data["idempotency"].get(key)
            if stored is not None:
                if stored["digest"] != digest:
                    raise MockError(409, "Idempotency-Key was already used with a different request")
                return copy.deepcopy(stored["result"])
            data = copy.deepcopy(self.data)
            subscriptions = {item["id"]: item for item in data["subscriptions"]}
            now = datetime.now(timezone.utc).replace(microsecond=0)
            result = {"success_count": 0, "failed_count": 0, "results": []}
            for subscription_id in ids:
                subscription = subscriptions.get(subscription_id)
                item = {"subscription_id": subscription_id, "success": subscription is not None}
                if subscription is None:
                    item["error"] = "mock subscription not found"
                    result["failed_count"] += 1
                else:
                    counts = data["reset_counts"][str(subscription_id)]
                    counts["total"] += 1
                    for field, enabled in mask.items():
                        if enabled:
                            subscription[field + "_usage_usd"] = 0.0
                            start = now.astimezone().replace(hour=0, minute=0, second=0) if field == "daily" else now
                            subscription[field + "_window_start"] = timestamp(start)
                            counts[field] += 1
                    result["success_count"] += 1
                result["results"].append(item)
            data["bulk_action_calls"] = data.get("bulk_action_calls", 0) + 1
            data["idempotency"][key] = {"digest": digest, "result": result}
            self._write(data)
            self.data = data
            return copy.deepcopy(result)


class MockHandler(BaseHTTPRequestHandler):
    def log_message(self, format_string, *args):
        pass

    def _body(self):
        if self.headers.get("Transfer-Encoding"):
            raise MockError(400, "Transfer-Encoding is not supported")
        try:
            length = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            raise MockError(400, "invalid Content-Length") from None
        if not 0 < length <= BODY_LIMIT:
            raise MockError(400, "JSON body must be between 1 and 65536 bytes")
        try:
            body = json.loads(self.rfile.read(length))
        except (UnicodeDecodeError, ValueError):
            raise MockError(400, "invalid JSON body") from None
        if not isinstance(body, dict):
            raise MockError(400, "JSON body must be an object")
        return body

    def _page(self, items, query):
        try:
            page = int(query.get("page", ["1"])[0])
            size = int(query.get("page_size", ["20"])[0])
        except ValueError:
            raise MockError(400, "invalid pagination") from None
        if page < 1 or not 1 <= size <= 1000:
            raise MockError(400, "page must be positive and page_size must be between 1 and 1000")
        return {"items": items[(page - 1) * size:page * size], "total": len(items),
                "page": page, "page_size": size, "pages": (len(items) + size - 1) // size}

    def _dispatch(self, path, query):
        state = self.server.mock_state
        if self.command == "GET" and path == "/mock/state":
            return state.snapshot()
        if self.command == "POST" and path == "/mock/usage":
            return state.update_usage(self._body())
        if self.command == "POST" and path == "/api/v1/admin/subscriptions/bulk-action":
            return state.reset_subscriptions(self._body(), self.headers.get("Idempotency-Key", ""))
        data = state.snapshot()
        if self.command == "GET" and path == "/api/v1/admin/accounts":
            if query.get("platform", ["openai"])[0] != "openai" or query.get("type", ["oauth"])[0] != "oauth":
                raise MockError(400, "the local mock only supports OpenAI OAuth accounts")
            items = data["accounts"]
            search = query.get("search", [""])[0].strip().casefold()
            if search:
                items = [item for item in items if search in item["name"].casefold()]
            group = query.get("group", [""])[0]
            status = query.get("status", [""])[0]
            if group:
                items = [item for item in items if group in [str(value) for value in item["group_ids"]]]
            if status:
                items = [item for item in items if item["status"] == status]
            return self._page(items, query)
        match = ACCOUNT_ROUTE.fullmatch(path)
        if self.command == "GET" and match:
            account = next((item for item in data["accounts"] if item["id"] == int(match[1])), None)
            if account is None:
                raise MockError(404, "mock account not found")
            return account
        if self.command == "GET" and path == "/api/v1/admin/subscriptions":
            items = data["subscriptions"]
            for field in ("user_id", "group_id", "status"):
                value = query.get(field, [""])[0]
                if value:
                    items = [item for item in items if str(item[field]) == value]
            platform = query.get("platform", [""])[0]
            if platform:
                items = [item for item in items if item["group"]["platform"] == platform]
            return self._page(items, query)
        raise MockError(404, "route is blocked by the local mock")

    def _handle(self):
        self.connection.settimeout(10)
        try:
            target = urlsplit(self.path)
            path, query = target.path, parse_qs(target.query, keep_blank_values=True)
        except ValueError:
            path, query = "<invalid-path>", {}
        allowed = ((self.command == "GET" and (path in ("/api/v1/admin/accounts", "/api/v1/admin/subscriptions", "/mock/state") or ACCOUNT_ROUTE.fullmatch(path)))
                   or (self.command == "POST" and path in ("/mock/usage", "/api/v1/admin/subscriptions/bulk-action")))
        try:
            key = self.headers.get("x-api-key", "")
            if not hmac.compare_digest(key.encode(), self.server.api_key.encode()):
                raise MockError(401, "administrator API key is required")
            if not allowed:
                raise MockError(404, "route is blocked by the local mock")
            data, status, message = self._dispatch(path, query), 200, ""
        except MockError as error:
            data, status, message = None, error.status, str(error)
        except (OSError, ValueError, KeyError, TypeError):
            data, status, message = None, 500, "local mock state could not be read or saved"
        self.server.mock_state.record_request(self.command, path, status, not bool(allowed))
        body = json.dumps({"code": 0 if status == 200 else status, "message": message, "data": data},
                          ensure_ascii=False, allow_nan=False).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    do_GET = do_POST = do_PUT = do_PATCH = do_DELETE = do_HEAD = do_OPTIONS = do_TRACE = do_CONNECT = _handle


class MockServer(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, state_file, port=8092, api_key=DEFAULT_API_KEY):
        if not api_key or "\r" in api_key or "\n" in api_key:
            raise ValueError("MOCK_MAIN_API_KEY must be nonempty and contain no newlines")
        self.mock_state = MockState(state_file)
        self.api_key = api_key
        super().__init__(("127.0.0.1", port), MockHandler)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--port", type=int, default=8092)
    parser.add_argument("--state-file", default="./data/mock-main.json")
    args = parser.parse_args()
    if not 1 <= args.port <= 65535:
        parser.error("--port must be between 1 and 65535")
    try:
        server = MockServer(args.state_file, args.port, os.environ.get("MOCK_MAIN_API_KEY", DEFAULT_API_KEY))
    except (OSError, ValueError) as error:
        parser.exit(1, "Cannot start local mock: " + str(error) + "\n")
    print("Local mock listening on http://127.0.0.1:" + str(args.port), flush=True)
    print("Only local fixture files are read or changed. No upstream requests are sent.", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
