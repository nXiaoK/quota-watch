# Docker 生产环境更新

本仓库通过 Dockerfile 在服务器上构建镜像。后续 `main` 有新功能时，需要拉取源码、重新构建并重建容器；`docker restart` 不会更新程序。本文不依赖已发布的镜像，也不配置定时更新。

## 一键更新

已有本仓库 Docker Compose 部署时，在**原部署目录**运行。脚本需要 Git、Docker Compose v2、Python 3、`mktemp`、`tar`、Docker 访问权限及构建镜像的网络访问；当前分支须为 `main`、Git 工作树须干净，原容器须正在运行：

```sh
./update.sh
```

脚本从原容器读取 Compose project 和配置文件，核对当前配置中的登录用户名、密码、主密钥及运行参数与原容器一致，并确认只存在一个与原容器相同的 `/data` 挂载。原来的多个 Compose 配置文件、自定义网络，以及 `/data` 为 named volume 或绑定目录的情况都可保留。若服务有其他挂载，或使用 Compose `env_file`、`secrets`、`configs`，脚本会在停机前拒绝更新；请按下文手动流程核对并备份这些额外文件。

脚本先保存升级前的 `.env`、每份原 Compose 配置及其路径清单、旧镜像 ID/标签与 `old-image.tar`，然后拉取 `main`，在原容器运行期间构建新镜像。停止原容器后，它将完整 `/data` 存为 `data.tar`，再重建服务；备份中还记录更新前后的源码提交号。备份默认放在部署目录同级的 `quota-watch-backups` 下，可用 `QW_BACKUP_ROOT` 指定其他目录，但备份目录必须位于仓库和原 `/data` 目录之外。确保备份位置有足够空间容纳旧镜像和完整数据，并保留脚本报告的备份目录以供回滚。

| 备份文件 | 内容 |
| --- | --- |
| `.env`、`container-env.json` | 原 `.env` 与原容器实际环境变量；两者都可能含凭据 |
| `compose-files.txt`、`compose.0.yaml` 等 | 原 Compose 文件的顺序、路径及逐份副本 |
| `deployment.txt` | 原 project、容器 ID 与 `/data` 挂载信息 |
| `old-image.id`、`old-image.tag`、`old-image.tar` | 旧镜像标识、回滚标签与完整镜像归档 |
| `data.tar` | 停止原容器后备份的完整 `/data`；若脚本在停机前失败，此文件尚不存在 |
| `source-before-update.commit`、`source-after-update.commit` | 更新前后的源码提交号 |
| `compose.auto-recovery.yaml` | 仅在自动恢复进入配置生成阶段后出现；指定旧镜像和独立恢复卷 |

备份目录权限为 `0700`，文件为 `0600`。不要分享其中的环境变量、镜像或数据库归档。

**已有 `.env`、容器或数据卷时不要重新执行首次安装。**唯一例外是本脚本首次安装失败：在原目录使用 `./install.sh --resume`，复用已生成的 `.env`。从新目录克隆后直接启动会改变 Compose project，并可能创建新的空数据卷。首次部署请按 [README 的 Docker Compose 步骤](README.md#docker-compose)运行 `./install.sh`；之后始终在同一部署目录更新。

更新前先确认主站连接仍可用，并记下全局自动重置及 Sub2API 自动更新的原开关状态。脚本成功表示新容器通过 `/healthz` 检查；仍需登录确认设置、规则和主站连接。备份完成前出错时，脚本会尝试重新启动原容器。开始重建后，若启动、`/data` 挂载核对或健康检查失败，脚本会停止项目中的 Quota Watch 容器，用旧镜像和 `data.tar` 在独立新卷中恢复旧版，并等待其通过健康检查；原数据卷或绑定目录保留。自动恢复成功后脚本仍报告更新失败。必须核对运行状态、数据和主站动作，并整理恢复后的 Compose 配置；`./update.sh` 会拒绝在回滚镜像或自动恢复卷上直接再次更新。自动恢复失败时，使用备份按下文手动恢复。更新脚本不能撤销已经提交给主站的订阅重置、Sub2API 更新或已发送的通知，恢复旧数据也可能丢失新容器短暂运行期间的记录。

### 一键更新备份的手动回滚

本节只适用于 `./update.sh` 生成的备份，供自动恢复失败或需要人工重建时使用；若自动恢复已经成功，不要再次解包旧数据。先确认 `data.tar` 存在；若脚本在停机前失败，它不会生成这份数据备份，原容器也仍在运行。恢复 `data.tar` 会丢失升级后产生的 Quota Watch 记录，且不能撤销主站已经执行的订阅重置、Sub2API 更新或已发送的通知。先核对这些动作和结果未知的任务，再决定是否恢复升级前状态。以下命令创建新卷，保留当前 `/data` 卷或绑定目录供核对。

在原部署目录的同一个 Bash 会话中逐段执行，任一命令失败都应停止并核对。先填写脚本报告的备份目录，读取备份中记录的原 project，载入旧镜像并恢复其回滚标签；`old-image.tar` 是按镜像 ID 保存的，不应假定载入后自动带有该标签：

```sh
set -euo pipefail
umask 077
QW_BACKUP_DIR='/填写一键更新备份目录的绝对路径'
for QW_REQUIRED in data.tar old-image.tar compose-files.txt .env; do
  test -s "$QW_BACKUP_DIR/$QW_REQUIRED"
done
QW_PROJECT=$(sed -n 's/^project=//p' "$QW_BACKUP_DIR/deployment.txt")
QW_OLD_IMAGE_ID=$(cat "$QW_BACKUP_DIR/old-image.id")
QW_OLD_IMAGE_TAG=$(cat "$QW_BACKUP_DIR/old-image.tag")
test -n "$QW_PROJECT"
test -n "$QW_OLD_IMAGE_ID"
test -n "$QW_OLD_IMAGE_TAG"
docker image load -i "$QW_BACKUP_DIR/old-image.tar"
docker image tag "$QW_OLD_IMAGE_ID" "$QW_OLD_IMAGE_TAG"
```

按 `compose-files.txt` 的原顺序载入每份配置副本，再叠加只用于本次回滚的配置。`--project-directory` 指向原部署目录，使原配置的相对路径继续按原目录解析；原 `.env` 从备份中读取。

```sh
unset QUOTA_WATCH_USERNAME QUOTA_WATCH_PASSWORD QUOTA_WATCH_MASTER_KEY \
  QUOTA_WATCH_DATA_DIR QUOTA_WATCH_LISTEN TZ
QW_COMPOSE=(docker compose --project-directory "$PWD" --env-file "$QW_BACKUP_DIR/.env" -p "$QW_PROJECT")
while IFS=$'\t' read -r QW_INDEX QW_ORIGINAL_PATH; do
  test -f "$QW_BACKUP_DIR/compose.$QW_INDEX.yaml" || exit 1
  QW_COMPOSE+=(-f "$QW_BACKUP_DIR/compose.$QW_INDEX.yaml")
done < "$QW_BACKUP_DIR/compose-files.txt"
QW_RESTORE_VOLUME="quota-watch-restore-$(date -u +%Y%m%dT%H%M%SZ)"
cat > "$QW_BACKUP_DIR/compose.manual-rollback.yaml" <<EOF
services:
  quota-watch:
    image: $QW_OLD_IMAGE_TAG
    pull_policy: never
    volumes:
      - type: volume
        source: quota-watch-restore
        target: /data
volumes:
  quota-watch-restore:
    external: true
    name: $QW_RESTORE_VOLUME
EOF
chmod 600 "$QW_BACKUP_DIR/compose.manual-rollback.yaml"
QW_COMPOSE+=(-f "$QW_BACKUP_DIR/compose.manual-rollback.yaml")
"${QW_COMPOSE[@]}" config --quiet
```

确认新卷名未占用，并核对当前 project 下的 Quota Watch 容器。以下操作会停止仍在运行的容器；自动恢复失败后若容器已不存在，也可继续恢复。在新卷解包后，用旧镜像、旧 `.env` 和备份配置启动。`/data` 的回滚挂载按目标路径覆盖原配置中的挂载，因此原部署使用 named volume 或绑定目录时都不会覆盖原数据。

```sh
docker volume inspect "$QW_RESTORE_VOLUME" >/dev/null 2>&1 && exit 1
QW_CURRENT_CONTAINERS=$(docker ps --no-trunc -aq \
  --filter "label=com.docker.compose.project=$QW_PROJECT" \
  --filter 'label=com.docker.compose.service=quota-watch')
while IFS= read -r QW_CURRENT_CONTAINER; do
  test -n "$QW_CURRENT_CONTAINER" || continue
  docker inspect --format 'project={{index .Config.Labels "com.docker.compose.project"}} status={{.State.Status}}' "$QW_CURRENT_CONTAINER"
  if [[ $(docker inspect --format '{{.State.Running}}' "$QW_CURRENT_CONTAINER") == true ]]; then
    docker stop --time 45 "$QW_CURRENT_CONTAINER"
  fi
done <<< "$QW_CURRENT_CONTAINERS"
docker volume create "$QW_RESTORE_VOLUME"
docker run --rm --network none --user 0:0 --entrypoint /bin/sh \
  --mount "type=volume,src=$QW_RESTORE_VOLUME,dst=/data" \
  --mount "type=bind,src=$QW_BACKUP_DIR,dst=/backup,readonly" \
  "$QW_OLD_IMAGE_ID" -c 'tar -C /data -xpf /backup/data.tar'
"${QW_COMPOSE[@]}" up -d --no-build --force-recreate --no-deps quota-watch
"${QW_COMPOSE[@]}" ps quota-watch
```

等待健康状态变为 `healthy`，再登录核对设置、规则、执行历史和主站连接。保留原数据卷和备份。自动或手动恢复后，容器可能引用备份目录里的 Compose 文件，并运行旧镜像及新恢复卷；先人工核对并将最终选定的镜像、卷、`.env` 和 Compose 配置固定到部署目录，再按下方手动流程规划下一次升级。`./update.sh` 会拒绝直接更新使用 `quota-watch-backup:*` 镜像或带自动恢复标签的卷的服务。本节的 `compose.N.yaml` 和 `compose-files.txt` 只由 `./update.sh` 生成；下面的手动流程使用另一组备份文件名，不可混用回滚命令。

## 手动更新与回滚（高级）

以下命令在部署服务器上执行，需要 Docker Compose v2、Git 和构建时的网络访问。每一步成功后再继续；出错时保留原容器、镜像和数据。示例使用默认 named volume；绑定宿主机目录的部署见下面的说明。

### 1. 确认原部署，保留数据和配置

先找到现有容器，并只查看项目标签和 `/data` 的挂载信息：

```sh
docker ps --format '{{.ID}}\t{{.Names}}\t{{.Image}}'
QW_CONTAINER='填写现有 quota-watch 容器名或 ID'
docker inspect --format 'project={{index .Config.Labels "com.docker.compose.project"}} working_dir={{index .Config.Labels "com.docker.compose.project.working_dir"}} config_files={{index .Config.Labels "com.docker.compose.project.config_files"}}' "$QW_CONTAINER"
docker inspect --format '{{range .Mounts}}{{if eq .Destination "/data"}}type={{.Type}} name={{.Name}} source={{.Source}}{{end}}{{end}}' "$QW_CONTAINER"
```

记录原 project 名、部署目录和配置文件。默认 Compose 的 `quota-watch-data` 会生成类似 `旧项目名_quota-watch-data` 的实际卷名，换目录可能改变项目名。`/data` 包含 SQLite 数据库、可能存在的 WAL 文件以及 `master.key`，必须一起保留。

如果现有目录已是这个 Git 仓库，直接在该目录更新。首次迁入独立仓库时，先克隆，暂时不要启动新容器：

```sh
umask 077
git clone https://github.com/nXiaoK/quota-watch.git
cd quota-watch
QW_OLD_DIR='/填写原部署目录的绝对路径'
install -m 600 "$QW_OLD_DIR/.env" .env
```

保留 `.env` 中原登录用户名、密码及 `QUOTA_WATCH_MASTER_KEY`；没有设置环境变量密钥时，保留数据卷里的原 `master.key`。不要重新生成密钥或用 `.env.example` 覆盖原 `.env`。如果原配置来自其他文件或 `docker run`，从原配置文件迁入这些值，不要将凭据输出到终端。

填写原 project 名，并取出已确认的原数据卷名：

```sh
QW_PROJECT='填写原 Compose project 名'
QW_VOLUME=$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{if eq .Type "volume"}}{{.Name}}{{end}}{{end}}{{end}}' "$QW_CONTAINER")
docker volume inspect "$QW_VOLUME" --format '{{.Name}}'
```

原容器若由 `docker run` 创建，没有 project 标签，可选用一个未被其他服务使用的固定名称，例如 `quota-watch-prod`。`QW_VOLUME` 为空或卷检查失败时不要继续，先核对挂载方式。

在仓库根目录创建本机专用的 `compose.prod.yaml`，将下面 `name` 替换为检查到的完整卷名。使用 `external: true` 时，卷不存在会报错，避免无意创建空卷：

```yaml
volumes:
  quota-watch-data:
    external: true
    name: 填写原实际卷名
```

将原端口、反向代理所需网络以及 Sub2API 所在的 Docker 网络配置也迁入这个文件。默认配置只监听宿主机 `127.0.0.1:8091`；容器中的 `127.0.0.1` 也不是 Sub2API 容器。保留原本可用的地址与网络关系，不要直接覆盖原部署的 Compose 文件。

如果 `/data` 的 `type=bind`，保留显示的绝对 `source` 路径，将 override 中服务的 `/data` 挂载改为该路径，不套用上面的 external volume 配置。后面的备份命令也改为将同一宿主机目录只读挂载到备份容器的 `/data`。

每次更新，在部署仓库根目录确认 `QW_PROJECT`、`QW_CONTAINER` 和 `QW_VOLUME` 后，定义以下命令。所有后续 Compose 操作使用同一 project 和配置文件：

```sh
chmod 600 .env compose.prod.yaml
qw_compose() {
  docker compose -p "$QW_PROJECT" -f compose.yaml -f compose.prod.yaml "$@"
}
qw_compose config --quiet
```

`.env`、本机部署配置和备份含凭据或生产信息，不提交到 Git。若原来使用多个 override，继续带上这些文件，并在后面的备份中保存它们。

### 2. 拉取源码，先构建新镜像

以下针对 `main` 部署。在维护前记下全局自动重置开关的原状态；已启用时先在页面关闭，验收后按原设置恢复。

先保存旧镜像、版本和部署配置。备份放在仓库外，目录权限为 `0700`，文件权限为 `0600`：

```sh
umask 077
QW_STAMP=$(date -u +%Y%m%dT%H%M%SZ)
QW_BACKUP_DIR="$PWD/../quota-watch-backups/$QW_STAMP"
install -d -m 700 "$QW_BACKUP_DIR"
QW_OLD_IMAGE=$(docker inspect --format '{{.Image}}' "$QW_CONTAINER")
docker image tag "$QW_OLD_IMAGE" "quota-watch-backup:$QW_STAMP"
printf '%s\n' "$QW_OLD_IMAGE" > "$QW_BACKUP_DIR/old-image.id"
printf 'quota-watch-backup:%s\n' "$QW_STAMP" > "$QW_BACKUP_DIR/old-image.tag"
git rev-parse HEAD > "$QW_BACKUP_DIR/source-before-update.commit"
install -m 600 .env "$QW_BACKUP_DIR/.env"
install -m 600 compose.yaml "$QW_BACKUP_DIR/compose.yaml"
install -m 600 compose.prod.yaml "$QW_BACKUP_DIR/compose.prod.yaml"
```

首次迁入时，新克隆目录的 Git 版本不代表原容器的源码版本，以上旧镜像才是程序回滚依据。将原部署实际使用的基础 Compose 文件保存为备份目录的 `compose.yaml`，额外 override 也一并保存；回滚使用原配置，再叠加数据卷和旧镜像 override。

检查工作树后更新。若存在自己修改的已跟踪文件，先处理这些改动；不要使用 `reset --hard`：

```sh
git status --short
git switch main
git fetch origin
git pull --ff-only origin main
git rev-parse HEAD > "$QW_BACKUP_DIR/source-after-update.commit"
qw_compose config --quiet
qw_compose build --pull quota-watch
docker pull alpine:3.22
```

构建时旧容器继续运行；构建失败就停在这里。`alpine:3.22` 仅用于后面的备份，提前拉取可缩短停机时间。正式上线也可选择经过验证的明确 tag 或 commit；使用 `git fetch origin --tags` 后切换到指定版本，再执行相同的构建和上线步骤。

### 3. 停服务备份，再启动新版

安排一个维护窗口，停止已确认的旧容器，然后备份整个原数据卷。仅在容器状态为 `exited` 后执行备份：

```sh
docker stop --time 45 "$QW_CONTAINER"
docker inspect --format '{{.State.Status}}' "$QW_CONTAINER"
docker run --rm --network none \
  --mount "type=volume,src=$QW_VOLUME,dst=/data,readonly" \
  --mount "type=bind,src=$QW_BACKUP_DIR,dst=/backup" \
  alpine:3.22 sh -c 'tar -C /data -cpf /backup/data.tar . && chmod 600 /backup/data.tar'
docker run --rm --network none \
  --mount "type=bind,src=$QW_BACKUP_DIR,dst=/backup,readonly" \
  alpine:3.22 tar -tf /backup/data.tar
qw_compose up -d --no-build quota-watch
```

备份失败时不要启动新版；原容器还未被替换，可用 `docker start "$QW_CONTAINER"` 恢复原服务后排查。备份必须包含整个目录，不能只复制正在使用的 `quota-watch.db`。将备份另存到安全位置，环境变量密钥也已随原 `.env` 保存。

验收新版：

```sh
qw_compose ps
QW_NEW_CONTAINER=$(qw_compose ps -q quota-watch)
docker inspect --format '{{.State.Status}} health={{if .State.Health}}{{.State.Health.Status}}{{end}}' "$QW_NEW_CONTAINER"
qw_compose logs --tail=100 quota-watch
```

健康检查需等待变为 `healthy`。随后通过原访问地址登录，确认原设置、规则和执行历史仍在，检查主站连接并模拟规则；这些操作读取主站已有快照，不需要真实重置或发送测试通知。确认无误后恢复原自动重置开关。查看实时请求时，在页面启用“详细运行日志”，执行 `qw_compose logs -f --tail=100 quota-watch`。

不要执行 `docker compose down -v`、删除数据卷或在回滚期清理旧镜像。普通 `up -d` 重建容器会复用指定数据卷；上线后继续保留这次备份。

### 4. 需要回滚时

保存的旧镜像可用于回滚。只有发布说明或实际验证确认旧程序兼容新版数据格式时，才能让旧镜像直接使用当前数据。不能确认兼容时，使用升级前的完整备份恢复到一个新的数据卷，保留当前卷供核对。

恢复旧数据会丢失备份后的记录，并不能撤销主站已经执行的重置或已发送的通知。若新版已经产生新动作，先核对主站和执行历史；无法确认时不要恢复旧状态或重放任务。`unknown` 结果必须人工核对，不能盲目重试。下面仅用于已经完成这项核对、可以恢复升级前状态的情况。

仍在部署仓库根目录，填写对应备份目录，并停止现有 Quota Watch 容器：

```sh
umask 077
QW_BACKUP_DIR='/填写升级前备份目录的绝对路径'
QW_OLD_IMAGE_TAG=$(cat "$QW_BACKUP_DIR/old-image.tag")
docker image inspect "$QW_OLD_IMAGE_TAG" --format '{{.Id}}'
QW_RESTORE_VOLUME="quota-watch-restore-$(date -u +%Y%m%dT%H%M%SZ)"
docker volume ls --filter "name=^$QW_RESTORE_VOLUME$"
```

确认恢复卷名尚不存在后继续。停止当前容器，在新卷解包，再用升级前的配置、密钥和旧镜像启动：

```sh
QW_CURRENT_CONTAINER=$(qw_compose ps -q quota-watch)
docker stop --time 45 "$QW_CURRENT_CONTAINER"
docker volume create "$QW_RESTORE_VOLUME"
docker run --rm --network none \
  --mount "type=volume,src=$QW_RESTORE_VOLUME,dst=/data" \
  --mount "type=bind,src=$QW_BACKUP_DIR,dst=/backup,readonly" \
  alpine:3.22 tar -C /data -xpf /backup/data.tar
cat > "$QW_BACKUP_DIR/compose.rollback.yaml" <<EOF
services:
  quota-watch:
    image: $QW_OLD_IMAGE_TAG
volumes:
  quota-watch-data:
    external: true
    name: $QW_RESTORE_VOLUME
EOF
chmod 600 "$QW_BACKUP_DIR/compose.rollback.yaml"
docker compose --project-directory "$PWD" --env-file "$QW_BACKUP_DIR/.env" \
  -p "$QW_PROJECT" -f "$QW_BACKUP_DIR/compose.yaml" \
  -f "$QW_BACKUP_DIR/compose.prod.yaml" \
  -f "$QW_BACKUP_DIR/compose.rollback.yaml" \
  up -d --no-build quota-watch
```

有额外 override 时，回滚命令也使用备份的对应文件。按第 3 节再次验收，使用回滚命令中相同的文件组合查看状态和日志。原数据卷不会被删除；确认后将部署使用的卷名、镜像及版本记录更新为实际运行状态，避免下次更新误切回其他卷。

本文命令根据仓库当前 Dockerfile、Compose 和 SQLite 存储实现编写；未在你的生产服务器执行。首次迁移及每次上线都应核对实际挂载、网络与健康状态。
