#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

ROOT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
cd "$ROOT_DIR"

fail() {
  printf '更新失败：%s\n' "$*" >&2
  exit 1
}

for tool in docker git mktemp python3 tar; do
  command -v "$tool" >/dev/null 2>&1 || fail "缺少 $tool"
done
docker compose version >/dev/null 2>&1 || fail '需要 Docker Compose v2'
docker info >/dev/null 2>&1 || fail '无法连接 Docker，请检查服务和当前用户权限'
[[ -f .env && -f compose.yaml ]] || fail '请在原部署目录运行；需要保留原 .env 和 compose.yaml'
[[ $(git branch --show-current) == main ]] || fail '更新脚本仅支持 main 分支；其他版本请按 UPDATING.md 手动更新'
[[ -z $(git status --porcelain) ]] || fail 'Git 工作区有未提交改动，请先处理后再更新'

# Locate the container by the checkout recorded by Compose, not by guessing a
# project name from the current directory. That protects its existing /data.
container=''
while IFS= read -r candidate; do
  [[ -n $candidate ]] || continue
  deployed_dir=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project.working_dir"}}' "$candidate")
  [[ -d $deployed_dir ]] || continue
  if [[ $(cd "$deployed_dir" && pwd -P) == "$ROOT_DIR" ]]; then
    [[ -z $container ]] || fail '此目录找到多个 quota-watch 容器，请按 UPDATING.md 核对部署'
    container=$candidate
  fi
done < <(docker ps --no-trunc -aq --filter 'label=com.docker.compose.service=quota-watch')
[[ -n $container ]] || fail '此目录没有已有的 quota-watch 容器；不要在新克隆目录更新原数据卷'
[[ $(docker inspect --format '{{.State.Status}}' "$container") == running ]] || fail '原容器未运行，请先排查当前部署状态'
mount_type=$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Type}}{{end}}{{end}}' "$container")
mount_name=$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}' "$container")
mount_source=$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Source}}{{end}}{{end}}' "$container")
case "$mount_type" in
  volume) [[ -n $mount_name ]] || fail '原 /data 数据卷名称为空' ;;
  bind) [[ -d $mount_source ]] || fail '原 /data 绑定目录不存在' ;;
  *) fail '原容器没有可识别的 /data 数据挂载' ;;
esac
original_mount_ref=${mount_name:-$mount_source}
configured_image=$(docker inspect --format '{{.Config.Image}}' "$container")
case "$configured_image" in
  quota-watch-backup:*) fail '当前服务使用回滚镜像，请先按 UPDATING.md 固定恢复后的部署配置，再决定是否更新' ;;
esac
if [[ $mount_type == volume ]]; then
  recovery_label=$(docker volume inspect --format '{{index .Labels "quota-watch.recovery"}}' "$mount_name") \
    || fail '无法检查当前数据卷的恢复状态'
  [[ $recovery_label != true ]] \
    || fail '当前服务运行在自动恢复卷上；请先按 UPDATING.md 核对并固定恢复后的部署配置'
fi
[[ $(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "$container") == healthy ]] \
  || fail '原容器尚未通过健康检查，请先排查当前部署状态'

project=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project"}}' "$container")
config_list=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project.config_files"}}' "$container")
[[ -n $project && $project != '<no value>' && -n $config_list && $config_list != '<no value>' ]] || fail '无法读取原 Compose 项目和配置文件'
IFS=',' read -r -a config_files <<< "$config_list"
[[ ${#config_files[@]} -gt 0 ]] || fail '原 Compose 配置文件列表为空'
COMPOSE=(docker compose --project-directory "$ROOT_DIR" --env-file "$ROOT_DIR/.env" -p "$project")
for file in "${config_files[@]}"; do
  [[ -f $file ]] || fail "原 Compose 文件不存在：$file"
  COMPOSE+=(-f "$file")
done
compose() (
  unset QUOTA_WATCH_USERNAME QUOTA_WATCH_PASSWORD QUOTA_WATCH_MASTER_KEY \
    QUOTA_WATCH_DATA_DIR QUOTA_WATCH_LISTEN COMPOSE_PROJECT_NAME COMPOSE_FILE TZ
  "${COMPOSE[@]}" "$@"
)
check_effective_environment() {
  compose config --format json | python3 -c '
import json, subprocess, sys
config = json.load(sys.stdin)
desired = config["services"]["quota-watch"].get("environment") or {}
raw = subprocess.check_output(["docker", "inspect", "--format", "{{json .Config.Env}}", sys.argv[1]], text=True)
actual = dict(item.split("=", 1) for item in json.loads(raw) if "=" in item)
keys = ("QUOTA_WATCH_USERNAME", "QUOTA_WATCH_PASSWORD", "QUOTA_WATCH_MASTER_KEY",
        "QUOTA_WATCH_DATA_DIR", "QUOTA_WATCH_LISTEN", "TZ")
changed = [key for key in keys if str(desired.get(key, "")) != actual.get(key, "")]
if changed:
    print("现有容器与当前 Compose 的环境变量不一致：" + ", ".join(changed), file=sys.stderr)
    sys.exit(1)
' "$container"
}
check_external_references() {
  compose config --no-env-resolution --format json | python3 -c '
import json, sys
service = json.load(sys.stdin)["services"]["quota-watch"]
if service.get("env_file") or service.get("secrets") or service.get("configs"):
    print("部署引用了外部凭据或配置文件，请按 UPDATING.md 手动更新并备份这些文件", file=sys.stderr)
    sys.exit(1)
'
}
compose config --quiet || fail '原 Compose 配置无效；请检查 .env 和配置文件'
[[ $(compose ps --all -q quota-watch) == "$container" ]] || fail 'Compose 配置未指向原容器，请按 UPDATING.md 核对项目和配置'
check_external_references || fail '无法独立备份原 Compose 的外部凭据或配置文件'
check_effective_environment || fail '不能确定原登录凭据和主密钥会被保留；请按 UPDATING.md 手动核对原部署'

mount_targets=$(docker inspect --format '{{range .Mounts}}{{println .Destination}}{{end}}' "$container")
[[ $mount_targets == /data ]] || fail '原容器还有其它文件挂载，请按 UPDATING.md 手动更新并备份这些文件'
check_data_mount() {
  compose config --format json | python3 -c '
import json, os, sys
config = json.load(sys.stdin)
service = config["services"]["quota-watch"]
mounts = service.get("volumes") or []
if len(mounts) != 1 or not isinstance(mounts[0], dict) or mounts[0].get("target") != "/data":
    print("自动更新仅支持一个 /data 挂载；其它挂载请按 UPDATING.md 手动更新", file=sys.stderr)
    sys.exit(1)
if service.get("env_file") or service.get("secrets") or service.get("configs"):
    print("部署引用了外部凭据或配置文件，请按 UPDATING.md 手动更新并备份这些文件", file=sys.stderr)
    sys.exit(1)
mount = mounts[0]
kind, expected_name, expected_source, project = sys.argv[1:]
if mount.get("type") != kind:
    sys.exit("新 Compose 配置的 /data 挂载类型已改变")
if kind == "volume":
    source = mount.get("source")
    volumes = config.get("volumes") or {}
    definition = volumes.get(source) or {}
    actual_name = definition.get("name") or (str(source) if definition.get("external") else project + "_" + str(source))
    if not source or actual_name != expected_name:
        sys.exit("新 Compose 配置会使用不同的数据卷")
elif os.path.realpath(mount.get("source") or "") != os.path.realpath(expected_source):
    sys.exit("新 Compose 配置会使用不同的 /data 绑定目录")
' "$mount_type" "$mount_name" "$mount_source" "$project"
}
check_data_mount || fail '不能确认新配置会复用原 /data；原容器未停止'
old_image=$(docker inspect --format '{{.Image}}' "$container")
docker image inspect "$old_image" >/dev/null 2>&1 || fail '找不到原容器镜像，无法保存回滚镜像'

backup_root=${QW_BACKUP_ROOT:-"$(dirname "$ROOT_DIR")/quota-watch-backups"}
if python3 -c 'import os, sys; checkout = os.path.realpath(sys.argv[1]); backup = os.path.realpath(sys.argv[2]); sys.exit(0 if os.path.commonpath((checkout, backup)) == checkout else 1)' \
  "$ROOT_DIR" "$backup_root"; then
  fail '备份目录不能位于 Git checkout 内部，否则会阻止下次更新'
fi
if python3 -c 'import os, sys; data = os.path.realpath(sys.argv[1]); backup = os.path.realpath(sys.argv[2]); sys.exit(0 if os.path.commonpath((data, backup)) == data else 1)' \
  "$mount_source" "$backup_root"; then
  fail '备份目录不能位于原 /data 目录内部'
fi
install -d -m 700 "$backup_root"
backup_root=$(cd "$backup_root" && pwd -P)
backup_dir=$(mktemp -d "$backup_root/$(date -u +%Y%m%dT%H%M%SZ).XXXXXX")
backup_tag="quota-watch-backup:$(date -u +%Y%m%dT%H%M%SZ)-$$"
install -m 600 .env "$backup_dir/.env"
docker inspect --format '{{json .Config.Env}}' "$container" > "$backup_dir/container-env.json"
chmod 600 "$backup_dir/container-env.json"
for index in "${!config_files[@]}"; do
  install -m 600 "${config_files[$index]}" "$backup_dir/compose.$index.yaml"
  printf '%s\t%s\n' "$index" "${config_files[$index]}" >> "$backup_dir/compose-files.txt"
done
chmod 600 "$backup_dir/compose-files.txt"
git rev-parse HEAD > "$backup_dir/source-before-update.commit"
printf '%s\n' "$old_image" > "$backup_dir/old-image.id"
printf '%s\n' "$backup_tag" > "$backup_dir/old-image.tag"
printf 'project=%s\ncontainer=%s\ndata_type=%s\ndata_name=%s\ndata_source=%s\n' \
  "$project" "$container" "$mount_type" "$mount_name" "$mount_source" > "$backup_dir/deployment.txt"
chmod 600 "$backup_dir/deployment.txt"
docker image tag "$old_image" "$backup_tag"
docker image save -o "$backup_dir/old-image.tar" "$old_image" || fail '无法保存原镜像文件；原容器仍在运行'
[[ -s $backup_dir/old-image.tar ]] || fail '原镜像备份为空；原容器仍在运行'
chmod 600 "$backup_dir/old-image.tar"

printf '正在拉取源码并构建镜像；原容器继续运行。\n'
git pull --ff-only origin main
git rev-parse HEAD > "$backup_dir/source-after-update.commit"
compose config --quiet || fail '新版本 Compose 配置检查失败；原容器仍在运行'
check_external_references || fail '新版本 Compose 引用了外部文件；原容器仍在运行，请手动核对'
check_effective_environment || fail '新版本 Compose 环境与原容器不同；原容器仍在运行，请手动核对'
check_data_mount || fail '新版本将更换 /data 挂载；原容器仍在运行，请手动核对'
compose build --pull quota-watch || fail '新镜像构建失败；原容器仍在运行'

stopped_old=0
backup_ready=0
rollout_started=0
recovery_volume=''
RECOVERY_COMPOSE=()
recovery_compose() (
  unset QUOTA_WATCH_USERNAME QUOTA_WATCH_PASSWORD QUOTA_WATCH_MASTER_KEY \
    QUOTA_WATCH_DATA_DIR QUOTA_WATCH_LISTEN COMPOSE_PROJECT_NAME COMPOSE_FILE TZ
  "${RECOVERY_COMPOSE[@]}" "$@"
)
stop_project_containers() {
  local candidates candidate running
  candidates=$(docker ps --no-trunc -aq \
    --filter "label=com.docker.compose.project=$project" \
    --filter 'label=com.docker.compose.service=quota-watch') || return 1
  while IFS= read -r candidate; do
    [[ -n $candidate ]] || continue
    running=$(docker inspect --format '{{.State.Running}}' "$candidate") || return 1
    if [[ $running == true ]]; then
      docker stop --time 45 "$candidate" >/dev/null || return 1
    fi
  done <<< "$candidates"
}
recover_from_backup() {
  local candidate created restored_container restored_image restored_type restored_name health
  [[ $backup_ready == 1 && -s $backup_dir/data.tar ]] || {
    printf '没有可用的数据备份，无法自动恢复。\n' >&2
    return 1
  }
  printf '新版未通过检查，正在从升级前备份自动恢复旧服务。\n' >&2
  stop_project_containers || {
    printf '无法停止当前项目容器，自动恢复中止。\n' >&2
    return 1
  }
  if ! docker image inspect "$old_image" >/dev/null 2>&1; then
    docker image load -i "$backup_dir/old-image.tar" >/dev/null || {
      printf '无法加载备份的旧镜像。\n' >&2
      return 1
    }
  fi
  docker image tag "$old_image" "$backup_tag" || {
    printf '无法为旧镜像恢复备份标签。\n' >&2
    return 1
  }
  for ((candidate = 0; candidate < 5; candidate++)); do
    recovery_volume="quota-watch-restore-$(date -u +%Y%m%dT%H%M%SZ)-$$-$RANDOM"
    if ! docker volume inspect "$recovery_volume" >/dev/null 2>&1; then
      break
    fi
  done
  if docker volume inspect "$recovery_volume" >/dev/null 2>&1; then
    printf '无法找到未占用的恢复卷名称。\n' >&2
    recovery_volume=''
    return 1
  fi
  created=$(docker volume create --label quota-watch.recovery=true \
    --label "quota-watch.backup=$backup_dir" "$recovery_volume") || {
    printf '无法创建独立恢复卷。\n' >&2
    return 1
  }
  [[ $created == "$recovery_volume" && \
     $(docker volume inspect --format '{{index .Labels "quota-watch.backup"}}' "$recovery_volume") == "$backup_dir" ]] || {
    printf '恢复卷名称或标签与预期不符，停止自动恢复以保护现有数据。\n' >&2
    return 1
  }
  docker run --rm -i --network none --user 0:0 --entrypoint /bin/sh \
    --mount "type=volume,src=$recovery_volume,dst=/data" \
    "$old_image" -c 'tar -C /data -xpf -' < "$backup_dir/data.tar" || {
    printf '无法将升级前数据解包至恢复卷。\n' >&2
    return 1
  }
  cat > "$backup_dir/compose.auto-recovery.yaml" <<EOF
services:
  quota-watch:
    image: $backup_tag
    pull_policy: never
    volumes:
      - type: volume
        source: quota-watch-restore
        target: /data
volumes:
  quota-watch-restore:
    external: true
    name: $recovery_volume
EOF
  chmod 600 "$backup_dir/compose.auto-recovery.yaml" || return 1
  RECOVERY_COMPOSE=(docker compose --project-directory "$ROOT_DIR" \
    --env-file "$backup_dir/.env" -p "$project")
  for candidate in "${!config_files[@]}"; do
    RECOVERY_COMPOSE+=(-f "$backup_dir/compose.$candidate.yaml")
  done
  RECOVERY_COMPOSE+=(-f "$backup_dir/compose.auto-recovery.yaml")
  recovery_compose config --format json | python3 -c '
import json, sys
config = json.load(sys.stdin)
service = config["services"]["quota-watch"]
mounts = service.get("volumes") or []
if service.get("image") != sys.argv[1] or service.get("pull_policy") != "never":
    sys.exit("恢复配置未锁定旧镜像")
if len(mounts) != 1 or mounts[0].get("target") != "/data" or mounts[0].get("type") != "volume":
    sys.exit("恢复配置的 /data 挂载无效")
source = mounts[0].get("source")
if (config.get("volumes") or {}).get(source, {}).get("name") != sys.argv[2]:
    sys.exit("恢复配置未使用独立恢复卷")
' "$backup_tag" "$recovery_volume" || {
    printf '恢复 Compose 配置校验失败。\n' >&2
    return 1
  }
  recovery_compose up -d --no-build --force-recreate --no-deps quota-watch || {
    printf '旧服务无法用备份配置启动。\n' >&2
    stop_project_containers || true
    return 1
  }
  restored_container=$(recovery_compose ps --all -q quota-watch) || {
    printf '无法查询恢复后的服务容器。\n' >&2
    stop_project_containers || true
    return 1
  }
  [[ -n $restored_container ]] || {
    printf '找不到恢复后的服务容器。\n' >&2
    stop_project_containers || true
    return 1
  }
  restored_image=$(docker inspect --format '{{.Image}}' "$restored_container") || {
    stop_project_containers || true
    return 1
  }
  restored_type=$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Type}}{{end}}{{end}}' "$restored_container") || {
    stop_project_containers || true
    return 1
  }
  restored_name=$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}' "$restored_container") || {
    stop_project_containers || true
    return 1
  }
  if [[ $restored_image != "$old_image" || $restored_type != volume || $restored_name != "$recovery_volume" ]]; then
    printf '恢复容器的旧镜像或 /data 挂载不符合预期。\n' >&2
    stop_project_containers || true
    return 1
  fi
  for ((candidate = 0; candidate < 90; candidate++)); do
    health=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$restored_container" 2>/dev/null || true)
    if [[ $health == healthy ]]; then
      return 0
    fi
    sleep 2
  done
  printf '恢复容器未在 3 分钟内通过健康检查。\n' >&2
  stop_project_containers || true
  return 1
}
restore_old_on_error() {
  local result=$?
  trap - EXIT
  if ((result != 0 && stopped_old == 1)); then
    if ((rollout_started == 1)); then
      if recover_from_backup; then
        printf '旧服务已自动恢复并通过健康检查。恢复卷：%s\n' "$recovery_volume" >&2
        printf '原 /data 挂载已保留：%s (%s；源路径：%s)\n' "$original_mount_ref" "$mount_type" "$mount_source" >&2
        printf '升级前完整备份：%s\n' "$backup_dir" >&2
        printf '升级期间已对外执行的操作无法撤销；请核对记录，并按 UPDATING.md 固定恢复后的部署配置。不要直接重试 update.sh。\n' >&2
      else
        printf '自动恢复失败；请按 UPDATING.md 手动恢复。恢复卷：%s；原 /data：%s (%s)；备份：%s\n' \
          "${recovery_volume:-未创建}" "$original_mount_ref" "$mount_source" "$backup_dir" >&2
      fi
    elif docker container inspect "$container" >/dev/null 2>&1; then
      if [[ $(docker inspect --format '{{.State.Status}}' "$container" 2>/dev/null || true) != running ]]; then
        if docker start "$container" >/dev/null 2>&1; then
          printf '已重新启动原容器。\n' >&2
        else
          printf '无法重新启动原容器，请使用备份恢复：%s\n' "$backup_dir" >&2
        fi
      fi
    else
      printf '原容器不存在，请使用备份和 UPDATING.md 手动恢复：%s\n' "$backup_dir" >&2
    fi
  fi
  exit "$result"
}
trap restore_old_on_error EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

printf '正在停止原容器并备份完整 /data。\n'
stopped_old=1
docker stop --time 45 "$container" >/dev/null || fail '无法停止原容器'
[[ $(docker inspect --format '{{.State.Status}}' "$container") == exited ]] || fail '原容器尚未完全停止，未开始数据备份'
if [[ $mount_type == volume ]]; then
  data_mount="type=volume,src=$mount_name,dst=/data,readonly"
else
  data_mount="type=bind,src=$mount_source,dst=/data,readonly"
fi
docker run --rm --network none --user 0:0 --entrypoint /bin/sh \
  --mount "$data_mount" "$old_image" -c 'tar -C /data -cf - .' > "$backup_dir/data.tar" \
  || fail '数据备份失败；将尝试重新启动原容器'
[[ -s $backup_dir/data.tar ]] && tar -tf "$backup_dir/data.tar" >/dev/null \
  || fail '数据备份校验失败；将尝试重新启动原容器'
backup_ready=1

printf '备份完成，正在重建服务。\n'
rollout_started=1
compose up -d --no-build --force-recreate --no-deps quota-watch \
  || fail "新容器启动失败；备份位于 $backup_dir"
new_container=$(compose ps -q quota-watch)
[[ -n $new_container ]] || fail "找不到新容器；备份位于 $backup_dir"
new_type=$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Type}}{{end}}{{end}}' "$new_container")
new_name=$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}' "$new_container")
new_source=$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Source}}{{end}}{{end}}' "$new_container")
if [[ $new_type != "$mount_type" || $new_name != "$mount_name" || $new_source != "$mount_source" ]]; then
  docker stop "$new_container" >/dev/null 2>&1 || true
  fail "新容器的 /data 挂载与原容器不同，已停止新容器；备份位于 $backup_dir"
fi

for ((attempt = 0; attempt < 90; attempt++)); do
  health=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$new_container" 2>/dev/null || true)
  if [[ $health == healthy ]]; then
    stopped_old=0
    printf 'Quota Watch 已更新并通过健康检查。\n'
    printf '源码版本：%s\n' "$(git rev-parse --short HEAD)"
    printf '旧镜像、配置和完整数据备份：%s\n' "$backup_dir"
    exit 0
  fi
  sleep 2
done
fail "新容器未在 3 分钟内通过健康检查；备份位于 ${backup_dir}，请查看 docker compose logs --tail=100 quota-watch"
