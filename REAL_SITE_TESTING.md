# 用完整 Sub2API 本地测试站查看效果

本文说明如何在本机构建并启动完整 Sub2API 后端和 Web UI，使用独立 PostgreSQL 与 Redis。测试站的账号、用户、分组、订阅是隔离数据库里的真实记录；账号没有 OAuth Token，额度变化由测试脚本写入这套隔离数据库。

## 首次构建和启动

启动工具只支持 macOS，并需要已安装的 Homebrew PostgreSQL 18、Redis、Python 3，以及 Sub2API 源码版本所需的 Go、Node.js 和 pnpm。当前源码使用 Go 1.27、Node.js 24 和 pnpm 9。Quota Watch 独立仓库不包含完整 Sub2API，先单独准备需要测试的 Sub2API 源码。

若此前运行了 Python 模拟主站，先在其终端按 `Ctrl+C` 停止，释放 8092。

以下 `cd quota-watch` 表示进入克隆后的项目根目录；按实际路径调整，并替换 Sub2API 源码目录：

```sh
cd quota-watch
export QUOTA_WATCH_SUB2API_DIR="/path/to/sub2api"
LAB_DIR="$PWD/data/real-site"
(
  set -e
  mkdir -p "$LAB_DIR/bin"
  pnpm --dir "$QUOTA_WATCH_SUB2API_DIR/frontend" install --frozen-lockfile
  pnpm --dir "$QUOTA_WATCH_SUB2API_DIR/frontend" run build
  test -s "$QUOTA_WATCH_SUB2API_DIR/backend/internal/web/dist/index.html"
  (
    cd "$QUOTA_WATCH_SUB2API_DIR/backend"
    CGO_ENABLED=0 go build -tags embed -buildvcs=false -trimpath \
      -o "$LAB_DIR/bin/sub2api" ./cmd/server
  )
  python3 dev/real_site.py up
)
```

Sub2API 前端构建产物位于 `backend/internal/web/dist`，后端须用 `-tags embed` 构建才能提供原 Web UI。不要用 Quota Watch 的二进制或未嵌入前端的后端替代。修改 Sub2API 源码后，先停止测试站，重复前端和后端构建，再重新启动。

也可直接传 `python3 dev/real_site.py --sub2api-dir /path/to/sub2api up`；命令行参数优先于 `QUOTA_WATCH_SUB2API_DIR`。原 `sub2api/tools/quota-watch` 目录布局下会自动检查所在的 Sub2API 源码目录。独立仓库必须明确指定实际源码目录；缺少源码、定价文件或可执行测试二进制时会报错。

首次启动完成后，使用下述固定测试连接登记隔离实例标记。命令会先校验库名、数据库用户、唯一测试管理员及空的账号/订阅数据；任一条件不匹配会使事务失败，不会登记标记。

```sh
env -i PATH="$PATH" PGCONNECT_TIMEOUT=5 PGSSLMODE=disable PGPASSFILE=/dev/null \
  /opt/homebrew/opt/postgresql@18/bin/psql --no-psqlrc --no-password \
  --host 127.0.0.1 --port 15432 --username quota_watch_lab \
  --dbname sub2api_quota_watch_local --set ON_ERROR_STOP=1 <<'SQL'
BEGIN;
CREATE TEMP TABLE local_test_guard (ok BOOLEAN NOT NULL CHECK (ok)) ON COMMIT DROP;
INSERT INTO local_test_guard SELECT
  current_database() = 'sub2api_quota_watch_local'
  AND current_user = 'quota_watch_lab'
  AND (SELECT COUNT(*) = 1 FROM users)
  AND (SELECT COUNT(*) = 1 FROM users WHERE role = 'admin' AND email = 'admin@quota-watch.local')
  AND NOT EXISTS (SELECT 1 FROM accounts)
  AND NOT EXISTS (SELECT 1 FROM user_subscriptions)
  AND NOT EXISTS (SELECT 1 FROM groups WHERE name <> 'default' OR deleted_at IS NOT NULL)
  AND NOT EXISTS (SELECT 1 FROM settings WHERE key = 'quota_watch_local_test_dataset');
INSERT INTO settings (key, value, updated_at)
VALUES ('quota_watch_local_test_instance', 'true', NOW())
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at;
COMMIT;
SQL

python3 dev/real_site_data.py seed
python3 dev/real_site_data.py status
```

实例标记只需在首次初始化的空测试库登记一次；已有测试数据时直接查看 `status`，不要重复执行登记 SQL。`seed` 输出实际数据 ID，后续页面选择和表中的 ID 以该输出为准。Quota Watch 的构建、启动和独立测试登录密码设置见 [TESTING.md](TESTING.md) 的第 1、3 步。

## 地址与测试登录

| 用途 | 地址 |
| --- | --- |
| 完整 Sub2API 测试站 | http://127.0.0.1:8092 |
| 外置监控服务 | http://127.0.0.1:8091 |

Sub2API 测试管理员：

- 邮箱：`admin@quota-watch.local`
- 密码：`LocalQuotaWatch123!`
- 本地管理员 API Key：`local-test-admin-key`

这些都是公开的测试凭据，仅用于上述隔离测试站，不可用于生产环境。8091 监控服务使用另行设置的测试登录密码；通知测试会发送到你自己配置的目标。

首次进入 Sub2API 管理后台会显示项目原有的部署与运营合规提示。请阅读并自行确认；未完成时管理员接口返回 423，外置监控不能读取账号。测试工具没有伪造或绕过此确认记录。

## 初始化后的测试数据

| 对象 | 名称 / ID | 初始值 |
| --- | --- | --- |
| 上游账号记录 | 本地 Codex 归零测试账号，账号 #1 | 7d 40%，5h 12% |
| 测试用户 | 本地测试用户，用户 #2 | demo@quota-watch.local |
| 订阅分组 | 本地 Codex 重置测试组，分组 #2 | 周额度 100 |
| 用户订阅 | 订阅 #1 | 日用量 5、周用量 20、月用量 30 |

账号保持不可调度，且不含 access_token、refresh_token，避免将测试数据用于模型请求。后端进程的网络出站仅放通独立 PostgreSQL `15432` 和 Redis `16379`；其他目的地受操作系统限制。数据库与缓存默认保存在本项目的 `data/real-site`，不要将生产数据放入这个目录。

## 查看与连接

1. 登录 8092，在“账号管理”查看测试账号 7d `40%`。
2. 在“订阅管理”查看测试用户订阅的周用量 `20`。
3. 进入 8091“连接设置”：主站地址填 `http://127.0.0.1:8092`，管理员 Key 填 `local-test-admin-key`，测试连接并保存。
4. 勾选账号 #1，保存启用规则，先保持全局及规则自动重置关闭；选择要验证的 TG/邮件渠道。
5. 点击“立即检查快照”，确认已记录非零 `40%` 基线。

若已有规则绑定了旧模拟订阅 #1001/#1002，请重新选择本测试站中 `seed` 输出的订阅。

## 模拟账号归零并查看通知

在终端执行：

```sh
cd quota-watch
python3 dev/real_site_data.py usage 0
```

这个命令只更新隔离 PostgreSQL 中测试账号的 7d 原始快照，并推进快照更新时间，不访问 OpenAI。

刷新 8092 的账号列表，应看到 `0%`。在 8091 点击“立即检查快照”或等下一轮，经过复核后出现 `40% → 0%` 事件；规则渠道已启用并保存时，通知真实发送到你配置的 Telegram/邮箱。

首次即为零不会触发事件，必须先建立非零基线。

## 验证真实测试订阅被清零

要测试订阅联动：

1. 先恢复测试数据：

   ```sh
   python3 dev/real_site_data.py restore
   ```

2. 在 8092 确认账号 `40%`、订阅 #1 周用量 `20`。
3. 在 8091 为规则勾选订阅 #1，仅选择周用量，开启规则自动重置与全局自动重置，保存。
4. 点击“立即检查快照”，确认记录新 `40%` 基线。
5. 再执行 `python3 dev/real_site_data.py usage 0`。
6. 在 8091 检查订阅执行结果，在 8092 刷新订阅列表，周用量应变为 `0`。日/月仍为 `5/30`，订阅到期时间保持不变。

这里的订阅清零会通过真正 Sub2API 的管理员 API 落到测试数据库，能够从原 UI 查看；它不会修改生产订阅。

关闭自动重置时已经观察到的归零事件不会在后续打开开关时补执行，因此联动演练需重新经过 `restore → 非零基线 → usage 0`。

## 核对数据库状态

```sh
python3 dev/real_site_data.py status
```

输出包括测试账号用量、采样时间和测试订阅日/周/月用量。脚本固定使用本机独立端口、库名、测试标记和数据清单，身份不匹配时拒绝写入，不通过用户提供的数据库 URL 操作其他库。

## 停止和重启真实测试站

```sh
python3 dev/real_site.py down
python3 dev/real_site.py --sub2api-dir /path/to/sub2api up
```

`down` 只停止这一套 Sub2API/PG/Redis，保留测试数据；不会停止 8091 的监控服务。`up` 保持出站隔离，不会因为重启放开上游访问。

重新打开终端时需重新设置 `QUOTA_WATCH_SUB2API_DIR`，或像上面一样传入 `--sub2api-dir`。若使用 `--lab-dir` 指定其他测试目录，后端二进制必须构建到该目录的 `bin/sub2api`，数据工具也要传同一个 `--lab-dir`。测试目录包含本地数据库、密码和密钥文件，不要提交或分享。
