#!/usr/bin/env python3
"""Run an isolated Sub2API UI test instance on macOS."""

import argparse
import json
import os
from pathlib import Path
import secrets
import signal
import socket
import subprocess
import sys
import time

PROJECT_ROOT = Path(__file__).resolve().parents[1]
DEFAULT_LAB = PROJECT_ROOT / "data/real-site"
PRICING_FALLBACK = Path("backend/resources/model-pricing/model_prices_and_context_window.json")
PG_BIN = Path("/opt/homebrew/opt/postgresql@18/bin")
PG_PORT = 15432
REDIS_PORT = 16379
SITE_PORT = 8092
ADMIN_EMAIL = "admin@quota-watch.local"
ADMIN_PASSWORD = "LocalQuotaWatch123!"


def write_private(path, content):
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as target:
        os.fchmod(target.fileno(), 0o600)
        target.write(content)


def run(command, **kwargs):
    subprocess.run([str(value) for value in command], check=True, **kwargs)


def listening(port):
    with socket.socket() as connection:
        connection.settimeout(0.2)
        return connection.connect_ex(("127.0.0.1", port)) == 0


def resolve_sub2api_dir(value):
    if value is None:
        if PROJECT_ROOT.parent.name != "tools":
            raise RuntimeError("Specify the Sub2API source checkout with --sub2api-dir or QUOTA_WATCH_SUB2API_DIR")
        value = PROJECT_ROOT.parent.parent
    directory = Path(value).expanduser().resolve()
    for relative in (Path("backend/go.mod"), Path("backend/cmd/server/main.go"),
                     Path("frontend/package.json"), PRICING_FALLBACK):
        if not (directory / relative).is_file():
            raise RuntimeError("Not a Sub2API source checkout: " + str(directory) + " (missing " + str(relative) + ")")
    return directory


def environment(lab):
    path = lab / "environment.json"
    if path.exists():
        with path.open(encoding="utf-8") as source:
            values = json.load(source)
        expected = {"DATABASE_HOST": "127.0.0.1", "DATABASE_PORT": str(PG_PORT),
                    "DATABASE_USER": "quota_watch_lab", "DATABASE_DBNAME": "sub2api_quota_watch_local",
                    "REDIS_HOST": "127.0.0.1", "REDIS_PORT": str(REDIS_PORT),
                    "SERVER_HOST": "127.0.0.1", "SERVER_PORT": str(SITE_PORT),
                    "DATA_DIR": str(lab / "site-data"), "CONFIG_FILE": str(lab / "runtime.yaml"),
                    "ADMIN_EMAIL": ADMIN_EMAIL}
        if not isinstance(values, dict) or any(values.get(key) != value for key, value in expected.items()):
            raise RuntimeError("Test environment identity changed; refusing to use another database or application configuration")
    else:
        values = {
            "AUTO_SETUP": "true", "DATA_DIR": str(lab / "site-data"),
            "CONFIG_FILE": str(lab / "runtime.yaml"),
            "DATABASE_HOST": "127.0.0.1", "DATABASE_PORT": str(PG_PORT),
            "DATABASE_USER": "quota_watch_lab", "DATABASE_PASSWORD": "local-test-db",
            "DATABASE_DBNAME": "sub2api_quota_watch_local", "DATABASE_SSLMODE": "disable",
            "DATABASE_MAX_OPEN_CONNS": "10", "DATABASE_MAX_IDLE_CONNS": "2",
            "REDIS_HOST": "127.0.0.1", "REDIS_PORT": str(REDIS_PORT), "REDIS_DB": "0",
            "REDIS_POOL_SIZE": "20", "REDIS_MIN_IDLE_CONNS": "1",
            "SERVER_HOST": "127.0.0.1", "SERVER_PORT": str(SITE_PORT), "SERVER_MODE": "release",
            "ADMIN_EMAIL": ADMIN_EMAIL, "ADMIN_PASSWORD": ADMIN_PASSWORD,
            "JWT_SECRET": secrets.token_hex(32), "TOTP_ENCRYPTION_KEY": secrets.token_hex(32),
            "TOKEN_REFRESH_ENABLED": "false", "SETUP_MIGRATION_TIMEOUT_SECONDS": "180",
            "TZ": "Asia/Shanghai",
        }
        write_private(path, json.dumps(values, indent=2) + "\n")
    return {**os.environ, **values}


def prepare(lab, sub2api_dir):
    if sys.platform != "darwin" or not Path("/usr/bin/sandbox-exec").exists():
        raise RuntimeError("This local launcher requires macOS sandbox-exec; it will not run without outbound isolation")
    if not (PG_BIN / "initdb").exists():
        raise RuntimeError("PostgreSQL 18 is required: brew install postgresql@18")
    for name in ("pg", "pg-socket", "redis", "site-data", "pricing", "logs", "pids", "bin"):
        (lab / name).mkdir(parents=True, exist_ok=True, mode=0o700)
    if not (lab / "pg/PG_VERSION").exists():
        run([PG_BIN / "initdb", "-D", lab / "pg", "-U", "quota_watch_lab", "-A", "trust", "--locale=C", "-E", "UTF8"])
    environment(lab)
    runtime = {
        "token_refresh": {"enabled": False},
        "gateway": {"cn_providers": {"balance_check_enabled": False}},
        "pricing": {"remote_url": "", "hash_url": "", "data_dir": str(lab / "pricing"),
                    "fallback_file": str(sub2api_dir / PRICING_FALLBACK)},
        "ops": {"enabled": False},
    }
    # JSON is a valid YAML document; paths and empty values remain exact.
    write_private(lab / "runtime.yaml", json.dumps(runtime, indent=2) + "\n")
    profile = """(version 1)
(allow default)
(deny network-outbound
  (require-not
    (require-any
      (remote tcp \"localhost:15432\")
      (remote tcp \"localhost:16379\"))))
"""
    write_private(lab / "network.sb", profile)


def backend_pid(lab):
    path = lab / "pids/sub2api.pid"
    if not path.exists():
        return None
    try:
        pid = int(path.read_text().strip())
        command = subprocess.run(["/bin/ps", "-p", str(pid), "-o", "command="],
                                 text=True, capture_output=True, check=False).stdout
        if str(lab / "bin/sub2api") not in command:
            return None
        return pid
    except (OSError, ValueError):
        return None


def up(lab, sub2api_dir):
    binary = lab / "bin/sub2api"
    if not binary.is_file() or not os.access(binary, os.X_OK):
        raise RuntimeError("Build the embedded Sub2API binary into " + str(binary))
    prepare(lab, sub2api_dir)
    pg_status = subprocess.run([str(PG_BIN / "pg_ctl"), "-D", str(lab / "pg"), "status"],
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    if pg_status.returncode != 0:
        if listening(PG_PORT):
            raise RuntimeError("PostgreSQL test port is already occupied; no existing database was changed")
        options = "-p " + str(PG_PORT) + " -h 127.0.0.1 -k " + str(lab / "pg-socket")
        run([PG_BIN / "pg_ctl", "-D", lab / "pg", "-l", lab / "logs/postgres.log", "-o", options, "-w", "start"])
    if not listening(REDIS_PORT):
        run(["/opt/homebrew/bin/redis-server", "--bind", "127.0.0.1", "--port", str(REDIS_PORT),
             "--dir", lab / "redis", "--appendonly", "yes", "--save", "", "--daemonize", "yes",
             "--pidfile", lab / "pids/redis.pid", "--logfile", lab / "logs/redis.log", "--protected-mode", "yes"])
    else:
        info = subprocess.run(["/opt/homebrew/bin/redis-cli", "-p", str(REDIS_PORT), "--raw", "CONFIG", "GET", "dir"],
                              capture_output=True, text=True, check=True).stdout.splitlines()
        if len(info) < 2 or Path(info[1]).resolve() != (lab / "redis").resolve():
            raise RuntimeError("Redis test port belongs to another instance; it was left unchanged")
    if backend_pid(lab):
        print("Sub2API test instance is already running at http://127.0.0.1:" + str(SITE_PORT))
        return
    if listening(SITE_PORT):
        raise RuntimeError("Port 8092 is occupied. Stop the previous Python mock before starting this real test site")
    with (lab / "logs/sub2api.log").open("ab") as log:
        process = subprocess.Popen(["/usr/bin/sandbox-exec", "-f", str(lab / "network.sb"), str(binary)],
                                   cwd=lab, env=environment(lab), stdout=log, stderr=log, start_new_session=True)
    write_private(lab / "pids/sub2api.pid", str(process.pid) + "\n")
    for _ in range(180):
        if process.poll() is not None:
            raise RuntimeError("Sub2API exited; inspect " + str(lab / "logs/sub2api.log"))
        if listening(SITE_PORT):
            print("Real Sub2API test UI: http://127.0.0.1:" + str(SITE_PORT))
            print("Backend outbound connections are limited to the isolated local database and Redis ports")
            return
        time.sleep(1)
    raise RuntimeError("Startup timed out; inspect " + str(lab / "logs/sub2api.log"))


def down(lab):
    pid = backend_pid(lab)
    if pid:
        os.kill(pid, signal.SIGTERM)
        for _ in range(45):
            if not backend_pid(lab):
                break
            time.sleep(1)
    if (lab / "pg/PG_VERSION").exists():
        subprocess.run([str(PG_BIN / "pg_ctl"), "-D", str(lab / "pg"), "-m", "fast", "-w", "stop"], check=False)
    if listening(REDIS_PORT):
        info = subprocess.run(["/opt/homebrew/bin/redis-cli", "-p", str(REDIS_PORT), "--raw", "CONFIG", "GET", "dir"],
                              capture_output=True, text=True, check=False).stdout.splitlines()
        if len(info) >= 2 and Path(info[1]).resolve() == (lab / "redis").resolve():
            run(["/opt/homebrew/bin/redis-cli", "-p", str(REDIS_PORT), "SHUTDOWN", "SAVE"])
    print("Only this test stack was stopped; its data is preserved")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--lab-dir", type=Path, default=DEFAULT_LAB)
    parser.add_argument("--sub2api-dir", type=Path, default=os.environ.get("QUOTA_WATCH_SUB2API_DIR"),
                        help="Sub2API source checkout; defaults to QUOTA_WATCH_SUB2API_DIR or the enclosing monorepo")
    parser.add_argument("action", choices=("prepare", "up", "down", "status"))
    args = parser.parse_args(argv)
    lab = args.lab_dir.expanduser().resolve()
    try:
        if args.action == "prepare":
            prepare(lab, resolve_sub2api_dir(args.sub2api_dir))
        elif args.action == "up":
            up(lab, resolve_sub2api_dir(args.sub2api_dir))
        elif args.action == "down":
            down(lab)
        else:
            print(json.dumps({"main_url": "http://127.0.0.1:8092", "backend_running": backend_pid(lab) is not None,
                              "postgres_listening": listening(PG_PORT), "redis_listening": listening(REDIS_PORT)}, indent=2))
    except (OSError, RuntimeError, subprocess.CalledProcessError) as error:
        parser.exit(1, str(error) + "\n")


if __name__ == "__main__":
    main()
