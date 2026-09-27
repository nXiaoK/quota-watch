# Docker 生产环境更新

本仓库通过 Dockerfile 在服务器上构建镜像。后续 `main` 有新功能时，拉取源码并重新构建、重建容器即可；`docker restart` 不会更新程序。本文不依赖已发布的镜像，也不配置自动更新。

以下命令在部署服务器上执行，需要 Docker Compose v2、Git 和构建时的网络访问。每一步成功后再继续；出错时保留原容器、镜像和数据。示例使用默认 named volume；绑定宿主机目录的部署见下面的说明。

## 1. 确认原部署，保留数据和配置

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

保留 `.env` 中原登录密码及 `QUOTA_WATCH_MASTER_KEY`；没有设置环境变量密钥时，保留数据卷里的原 `master.key`。不要重新生成密钥或用 `.env.example` 覆盖原 `.env`。如果原配置来自其他文件或 `docker run`，从原配置文件迁入这些值，不要将凭据输出到终端。

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

## 2. 拉取源码，先构建新镜像

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

## 3. 停服务备份，再启动新版

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

## 4. 需要回滚时

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
