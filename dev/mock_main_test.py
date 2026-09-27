import concurrent.futures
from datetime import datetime
import http.client
import json
from pathlib import Path
import stat
import tempfile
import threading
import unittest

from mock_main import DEFAULT_API_KEY, MockServer, MockState


class MockMainTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.path = Path(self.directory.name) / "mock.json"
        self.server = MockServer(self.path, port=0)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.port = self.server.server_address[1]

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)
        self.directory.cleanup()

    def request(self, method, path, body=None, key=DEFAULT_API_KEY, headers=None, raw=None):
        connection = http.client.HTTPConnection("127.0.0.1", self.port, timeout=3)
        request_headers = {"Content-Type": "application/json", **(headers or {})}
        if key is not None:
            request_headers["x-api-key"] = key
        payload = raw if raw is not None else json.dumps(body) if body is not None else None
        try:
            connection.request(method, path, body=payload, headers=request_headers)
            response = connection.getresponse()
            content = response.read()
            return response.status, json.loads(content) if content else None
        finally:
            connection.close()

    def test_read_only_snapshots_and_account_filters(self):
        status, account = self.request("GET", "/api/v1/admin/accounts/1")
        self.assertEqual(status, 200)
        initial = account["data"]["extra"]
        for _ in range(3):
            _, current = self.request("GET", "/api/v1/admin/accounts/1")
            self.assertEqual(current["data"]["extra"], initial)
        _, page = self.request("GET", "/api/v1/admin/accounts?platform=openai&type=oauth&group=9&search=Codex&page_size=1")
        self.assertEqual(page["data"]["items"][0]["id"], 1)
        self.assertEqual(page["data"]["total"], 1)
        _, empty = self.request("GET", "/api/v1/admin/accounts?group=10")
        self.assertEqual(empty["data"]["items"], [])
        self.assertEqual(self.request("GET", "/api/v1/admin/accounts?platform=anthropic")[0], 400)
        self.assertEqual(self.request("GET", "/api/v1/admin/accounts/99")[0], 404)

    def test_control_updates_snapshot_time_monotonically_and_persists(self):
        _, account = self.request("GET", "/api/v1/admin/accounts/1")
        previous = datetime.fromisoformat(account["data"]["extra"]["codex_usage_updated_at"].replace("Z", "+00:00"))
        for used in (0, 40, 0):
            status, updated = self.request("POST", "/mock/usage", {"account_id": 1, "used_percent": used})
            self.assertEqual(status, 200)
            extra = updated["data"]["extra"]
            self.assertEqual(extra["codex_7d_used_percent"], used)
            sampled = datetime.fromisoformat(extra["codex_usage_updated_at"].replace("Z", "+00:00"))
            self.assertGreater(sampled, previous)
            previous = sampled
        reloaded = MockState(self.path).snapshot()
        self.assertEqual(reloaded["accounts"][0]["extra"], extra)
        self.assertEqual(stat.S_IMODE(self.path.stat().st_mode), 0o600)

    def test_subscription_filters_and_partial_reset(self):
        _, page = self.request("GET", "/api/v1/admin/subscriptions?group_id=9&status=active&page_size=1&page=2")
        self.assertEqual(page["data"]["total"], 2)
        self.assertEqual(page["data"]["pages"], 2)
        self.assertEqual(page["data"]["items"][0]["id"], 1002)
        _, page = self.request("GET", "/api/v1/admin/subscriptions?user_id=2001")
        self.assertEqual([item["id"] for item in page["data"]["items"]], [1001])
        body = {"subscription_ids": [1001, 9999], "action": "reset_quota", "weekly": True}
        status, result = self.request("POST", "/api/v1/admin/subscriptions/bulk-action", body,
                                      headers={"Idempotency-Key": "partial-reset"})
        self.assertEqual(status, 200)
        self.assertEqual(result["data"]["success_count"], 1)
        self.assertEqual(result["data"]["failed_count"], 1)
        _, state = self.request("GET", "/mock/state")
        subscription = state["data"]["subscriptions"][0]
        self.assertEqual(subscription["weekly_usage_usd"], 0)
        self.assertEqual(subscription["daily_usage_usd"], 3)
        self.assertEqual(subscription["monthly_usage_usd"], 80)
        self.assertEqual(state["data"]["subscriptions"][1]["weekly_usage_usd"], 20)
        self.assertEqual(state["data"]["reset_counts"]["1001"], {"total": 1, "daily": 0, "weekly": 1, "monthly": 0})

    def test_idempotency_replay_conflict_persistence_and_concurrency(self):
        body = {"subscription_ids": [1001, 1002], "action": "reset_quota", "daily": True, "monthly": True}
        headers = {"Idempotency-Key": "same-reset"}
        with concurrent.futures.ThreadPoolExecutor(max_workers=5) as pool:
            responses = list(pool.map(lambda _: self.request("POST", "/api/v1/admin/subscriptions/bulk-action", body, headers=headers), range(5)))
        self.assertTrue(all(response == responses[0] for response in responses))
        self.assertEqual(responses[0][0], 200)
        saved = MockState(self.path)
        state = saved.snapshot()
        self.assertEqual(state["bulk_action_calls"], 1)
        self.assertEqual(state["reset_counts"]["1001"], {"total": 1, "daily": 1, "weekly": 0, "monthly": 1})
        self.assertEqual(saved.reset_subscriptions(body, "same-reset"), responses[0][1]["data"])
        changed = {**body, "weekly": True}
        self.assertEqual(self.request("POST", "/api/v1/admin/subscriptions/bulk-action", changed, headers=headers)[0], 409)
        self.assertEqual(self.server.mock_state.snapshot()["bulk_action_calls"], 1)

    def test_all_api_and_control_routes_require_key(self):
        for method, path, body in (
            ("GET", "/mock/state", None),
            ("POST", "/mock/usage", {"account_id": 1, "used_percent": 0}),
            ("GET", "/api/v1/admin/accounts", None),
            ("GET", "/api/v1/admin/subscriptions", None),
        ):
            with self.subTest(path=path):
                self.assertEqual(self.request(method, path, body, key=None)[0], 401)
                self.assertEqual(self.request(method, path, body, key="wrong-key")[0], 401)
        self.assertEqual(self.server.mock_state.snapshot()["accounts"][0]["extra"]["codex_7d_used_percent"], 40)

    def test_upstream_and_unlisted_routes_are_blocked_and_logged_without_query_or_key(self):
        paths = (
            "/api/v1/admin/openai/accounts/1/quota", "/api/v1/admin/accounts/1/usage",
            "/api/v1/admin/accounts/1/refresh", "/api/v1/admin/system/version",
            "/api/v1/admin/accounts/1/test", "/api/v1/admin/accounts/01", "/unknown",
        )
        for path in paths:
            for method in ("GET", "POST"):
                self.assertEqual(self.request(method, path + "?secret=do-not-record", {})[0], 404)
        self.assertEqual(self.request("PUT", "/api/v1/admin/accounts/1", {})[0], 404)
        _, state = self.request("GET", "/mock/state")
        self.assertEqual(len(state["data"]["blocked_requests"]), len(paths) * 2 + 1)
        log = json.dumps(state["data"]["recent_requests"])
        self.assertNotIn("do-not-record", log)
        self.assertNotIn(DEFAULT_API_KEY, log)
        self.assertEqual(state["data"]["bulk_action_calls"], 0)

    def test_bad_payloads_do_not_change_data(self):
        before = self.server.mock_state.snapshot()["accounts"]
        for body in ({"account_id": 1, "used_percent": -1}, {"account_id": 1, "used_percent": float("nan")},
                     {"account_id": 1, "used_percent": float("inf")}, {"account_id": 1, "used_percent": 10 ** 500},
                     {"account_id": True, "used_percent": 0}, {"account_id": 1, "used_percent": "0"},
                     {"account_id": 1, "used_percent": 0, "extra": "not-allowed"}, []):
            self.assertEqual(self.request("POST", "/mock/usage", body)[0], 400)
        self.assertEqual(self.request("POST", "/mock/usage", {"account_id": 99, "used_percent": 0})[0], 404)
        self.assertEqual(self.request("POST", "/mock/usage", raw="{invalid")[0], 400)
        self.assertEqual(self.server.mock_state.snapshot()["accounts"], before)
        reset = {"subscription_ids": [1001], "action": "reset_quota", "weekly": True}
        for body in ({**reset, "action": "extend"}, {**reset, "weekly": False}, {**reset, "weekly": 1},
                     {**reset, "subscription_ids": [1001, 1001]}, {**reset, "days": 30}):
            self.assertEqual(self.request("POST", "/api/v1/admin/subscriptions/bulk-action", body,
                                          headers={"Idempotency-Key": "bad-reset"})[0], 400)
        self.assertEqual(self.request("POST", "/api/v1/admin/subscriptions/bulk-action", reset)[0], 400)
        self.assertEqual(self.server.mock_state.snapshot()["bulk_action_calls"], 0)


if __name__ == "__main__":
    unittest.main()
