#!/usr/bin/env bash
set -Eeuo pipefail

source_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
test_root=$(mktemp -d "${TMPDIR:-/tmp}/quota-watch-deploy-test.XXXXXX")
trap 'rm -rf -- "$test_root"' EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_contains() {
  grep -Fq -- "$2" "$1" || fail "expected '$2' in $1"
}

assert_absent() {
  if grep -Fq -- "$2" "$1"; then
    fail "unexpected '$2' in $1"
  fi
}

make_fixture() {
  local name=$1 fixture="$test_root/$1"
  mkdir -p "$fixture/bin" "$fixture/state/data"
  cp "$source_dir/install.sh" "$source_dir/update.sh" "$source_dir/compose.yaml" "$fixture/"
  printf 'persistent settings\n' > "$fixture/state/data/settings.db"
  tar -C "$fixture/state/data" -cf "$fixture/state/archive.tar" .
  printf '%s\n' "$name" > "$fixture/state/mode"
  printf 'old-container\n' > "$fixture/state/current_container"
  printf 'running\n' > "$fixture/state/old_status"
  printf 'before-commit\n' > "$fixture/state/git_rev"
  : > "$fixture/state/docker.calls"
  : > "$fixture/state/git.calls"

  cat > "$fixture/bin/docker" <<'MOCK_DOCKER'
#!/usr/bin/env bash
set -Eeuo pipefail
printf '%s\n' "$*" >> "$FAKE_STATE/docker.calls"
mode=$(<"$FAKE_STATE/mode")
current=$(<"$FAKE_STATE/current_container")
case ${1:-} in
  info) exit 0 ;;
  compose)
    shift
    if [[ ${1:-} == version ]]; then exit 0; fi
    action=''
    recovery=0
    while (( $# )); do
      case $1 in
        --project-directory|--env-file|-p) shift 2 ;;
        -f)
          [[ ${2:-} != *compose.auto-recovery.yaml ]] || recovery=1
          shift 2
          ;;
        config|build|up|ps) action=$1; shift; break ;;
        *) exit 80 ;;
      esac
    done
    case $action in
      config)
        no_env_resolution=0
        if [[ ${1:-} == --no-env-resolution ]]; then
          no_env_resolution=1
          shift
        fi
        if [[ ${1:-} == --format && ${2:-} == json ]]; then
          if (( recovery == 1 )); then
            image=$(<"$FAKE_STATE/recovery_tag")
            volume=$(<"$FAKE_STATE/recovery_volume")
            printf '{"services":{"quota-watch":{"image":"%s","pull_policy":"never","volumes":[{"type":"volume","source":"quota-watch-restore","target":"/data"}]}},"volumes":{"quota-watch-restore":{"name":"%s"}}}\n' "$image" "$volume"
            exit 0
          fi
          master=existing-master-key
          volume=original-project_quota-watch-data
          external=''
          [[ ! -f $FAKE_STATE/environment_drift ]] || master=changed-master-key
          [[ ! -f $FAKE_STATE/mount_drift ]] || volume=different-volume
          if (( no_env_resolution == 1 )) && [[ -f $FAKE_STATE/external_env_file ]]; then
            external=',"env_file":[{"path":"external.env","required":true}]'
          fi
          cat <<COMPOSE_JSON
{"services":{"quota-watch":{"environment":{"QUOTA_WATCH_USERNAME":"existing-user","QUOTA_WATCH_PASSWORD":"existing-password","QUOTA_WATCH_MASTER_KEY":"$master","QUOTA_WATCH_DATA_DIR":"/data","QUOTA_WATCH_LISTEN":"0.0.0.0:8091","TZ":"Asia/Shanghai"},"volumes":[{"type":"volume","source":"quota-watch-data","target":"/data"}]$external}},"volumes":{"quota-watch-data":{"name":"$volume"}}}
COMPOSE_JSON
        fi
        ;;
      build) exit 0 ;;
      ps) printf '%s\n' "$current" ;;
      up)
        if (( recovery == 1 )); then
          printf 'restored-container\n' > "$FAKE_STATE/current_container"
          printf 'running\n' > "$FAKE_STATE/restored_status"
        elif [[ $mode == install ]]; then
          printf 'install-container\n' > "$FAKE_STATE/current_container"
        else
          printf 'new-container\n' > "$FAKE_STATE/current_container"
          printf 'running\n' > "$FAKE_STATE/new_status"
          if [[ -f $FAKE_STATE/rollout_up_fail ]]; then exit 92; fi
        fi
        ;;
      *) exit 81 ;;
    esac
    ;;
  ps)
    if [[ $mode == update ]]; then
      printf '%s\n' "$current"
    elif [[ $current == install-container ]]; then
      printf 'install-container\n'
    fi
    ;;
  volume)
    case ${2:-} in
      ls) exit 0 ;;
      inspect)
        if [[ ${3:-} == --format ]]; then
          format=${4:-}
          volume=${5:-}
          case $format in
            '{{index .Labels "quota-watch.recovery"}}')
              if [[ $volume == original-project_quota-watch-data && ! -f $FAKE_STATE/recovery_label ]]; then
                printf '<no value>\n'
              else
                printf 'true\n'
              fi
              ;;
            '{{index .Labels "quota-watch.backup"}}')
              [[ -f $FAKE_STATE/recovery_volume && $volume == "$(<"$FAKE_STATE/recovery_volume")" ]] || exit 1
              cat "$FAKE_STATE/recovery_backup"
              ;;
            *) exit 82 ;;
          esac
        elif [[ -f $FAKE_STATE/recovery_volume && ${3:-} == "$(<"$FAKE_STATE/recovery_volume")" ]]; then
          exit 0
        else
          exit 1
        fi
        ;;
      create)
        [[ ! -f $FAKE_STATE/recovery_volume_create_fail ]] || exit 93
        volume=${*: -1}
        backup=''
        for arg in "$@"; do
          case $arg in quota-watch.backup=*) backup=${arg#quota-watch.backup=} ;; esac
        done
        [[ $* == *quota-watch.recovery=true* && -n $backup ]] || exit 82
        printf '%s\n' "$volume" > "$FAKE_STATE/recovery_volume"
        printf '%s\n' "$backup" > "$FAKE_STATE/recovery_backup"
        printf '%s\n' "$volume"
        ;;
      *) exit 82 ;;
    esac
    ;;
  inspect)
    [[ ${2:-} == --format ]] || exit 83
    format=${3:-}
    container=${4:-}
    case $format in
      '{{index .Config.Labels "com.docker.compose.project.working_dir"}}') printf '%s\n' "$DEPLOY_DIR" ;;
      '{{index .Config.Labels "com.docker.compose.project.config_files"}}') printf '%s,%s\n' "$DEPLOY_DIR/compose.yaml" "$DEPLOY_DIR/override.yaml" ;;
      '{{index .Config.Labels "com.docker.compose.project"}}')
        if [[ $mode == install ]]; then printf 'quota-watch\n'; else printf 'original-project\n'; fi
        ;;
      '{{.State.Status}}')
        case $container in
          old-container) cat "$FAKE_STATE/old_status" ;;
          new-container) cat "$FAKE_STATE/new_status" ;;
          restored-container) cat "$FAKE_STATE/restored_status" ;;
          *) printf 'running\n' ;;
        esac
        ;;
      '{{.State.Running}}')
        status=running
        case $container in
          old-container) status=$(<"$FAKE_STATE/old_status") ;;
          new-container) status=$(<"$FAKE_STATE/new_status") ;;
          restored-container) status=$(<"$FAKE_STATE/restored_status") ;;
        esac
        if [[ $status == running ]]; then printf 'true\n'; else printf 'false\n'; fi
        ;;
      '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}') printf 'healthy\n' ;;
      '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}')
        if [[ $container == new-container && -f $FAKE_STATE/rollout_unhealthy ]]; then
          printf 'unhealthy\n'
        else
          printf 'healthy\n'
        fi
        ;;
      '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Type}}{{end}}{{end}}') printf 'volume\n' ;;
      '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}')
        if [[ $container == restored-container ]]; then cat "$FAKE_STATE/recovery_volume"; else printf 'original-project_quota-watch-data\n'; fi
        ;;
      '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Source}}{{end}}{{end}}') printf '/var/lib/docker/volumes/original-project_quota-watch-data/_data\n' ;;
      '{{range .Mounts}}{{println .Destination}}{{end}}') printf '/data\n' ;;
      '{{.Config.Image}}')
        if [[ -f $FAKE_STATE/rollback_image ]]; then
          printf 'quota-watch-backup:old\n'
        else
          printf 'quota-watch:old\n'
        fi
        ;;
      '{{.Image}}')
        if [[ $container == new-container ]]; then printf 'sha256:new-image\n'; else printf 'sha256:old-image\n'; fi
        ;;
      '{{json .Config.Env}}') printf '["QUOTA_WATCH_USERNAME=existing-user","QUOTA_WATCH_PASSWORD=existing-password","QUOTA_WATCH_MASTER_KEY=existing-master-key","QUOTA_WATCH_DATA_DIR=/data","QUOTA_WATCH_LISTEN=0.0.0.0:8091","TZ=Asia/Shanghai"]\n' ;;
      *) exit 84 ;;
    esac
    ;;
  image)
    case ${2:-} in
      inspect) exit 0 ;;
      tag)
        printf '%s\n' "${4:-}" > "$FAKE_STATE/recovery_tag"
        ;;
      save)
        [[ ${3:-} == -o ]] || exit 85
        printf 'mock image archive\n' > "$4"
        ;;
      *) exit 85 ;;
    esac
    ;;
  container)
    [[ ${2:-} == inspect && ${3:-} == old-container ]] || exit 86
    ;;
  stop)
    container=${*: -1}
    case $container in
      old-container) printf 'exited\n' > "$FAKE_STATE/old_status" ;;
      new-container) printf 'exited\n' > "$FAKE_STATE/new_status" ;;
      restored-container) printf 'exited\n' > "$FAKE_STATE/restored_status" ;;
    esac
    ;;
  start)
    [[ ${2:-} == old-container ]] || exit 87
    printf 'running\n' > "$FAKE_STATE/old_status"
    printf 'old-container\n'
    ;;
  run)
    if [[ -f $FAKE_STATE/backup_fail ]]; then exit 88; fi
    if [[ $* == *'tar -C /data -xpf -'* ]]; then
      mkdir -p "$FAKE_STATE/recovery-data"
      tar -C "$FAKE_STATE/recovery-data" -xpf -
    else
      cat "$FAKE_STATE/archive.tar"
    fi
    ;;
  *) exit 89 ;;
esac
MOCK_DOCKER

  cat > "$fixture/bin/openssl" <<'MOCK_OPENSSL'
#!/usr/bin/env bash
set -Eeuo pipefail
case "$*" in
  'rand -hex 8') printf '0011223344556677\n' ;;
  'rand -base64 24') printf '%032d\n' 0 ;;
  'rand -base64 32') printf '%043d=\n' 0 ;;
  *) exit 90 ;;
esac
MOCK_OPENSSL

  cat > "$fixture/bin/git" <<'MOCK_GIT'
#!/usr/bin/env bash
set -Eeuo pipefail
printf '%s\n' "$*" >> "$FAKE_STATE/git.calls"
case "$*" in
  'branch --show-current') printf 'main\n' ;;
  'status --porcelain') ;;
  'rev-parse HEAD') cat "$FAKE_STATE/git_rev" ;;
  'rev-parse --short HEAD') printf 'after123\n' ;;
  'pull --ff-only origin main') printf 'after-commit\n' > "$FAKE_STATE/git_rev" ;;
  *) exit 91 ;;
esac
MOCK_GIT

  cat > "$fixture/bin/sleep" <<'MOCK_SLEEP'
#!/usr/bin/env bash
set -Eeuo pipefail
printf '%s\n' "$*" >> "$FAKE_STATE/sleep.calls"
MOCK_SLEEP

  chmod +x "$fixture/bin/docker" "$fixture/bin/openssl" "$fixture/bin/git" "$fixture/bin/sleep"
  printf '%s\n' "$fixture"
}

run_install_tests() {
  local fixture output
  fixture=$(make_fixture install)
  output="$fixture/install.out"
  env PATH="$fixture/bin:$PATH" FAKE_STATE="$fixture/state" DEPLOY_DIR="$fixture" \
    bash "$fixture/install.sh" > "$output" 2>&1 || fail "first install failed: $(cat "$output")"

  assert_contains "$fixture/.env" '# Generated by quota-watch install.sh'
  assert_contains "$fixture/.env" 'COMPOSE_PROJECT_NAME=quota-watch'
  assert_contains "$fixture/.env" 'QUOTA_WATCH_USERNAME=qw_0011223344556677'
  assert_contains "$fixture/.env" 'QUOTA_WATCH_PASSWORD=00000000000000000000000000000000'
  assert_contains "$fixture/.env" 'QUOTA_WATCH_MASTER_KEY=0000000000000000000000000000000000000000000='
  assert_contains "$output" 'Quota Watch 已安装并通过健康检查'
  assert_contains "$output" '登录账号：qw_0011223344556677'
  assert_contains "$output" '登录密码：00000000000000000000000000000000'
  assert_contains "$fixture/state/docker.calls" 'compose --project-directory'
  assert_contains "$fixture/state/docker.calls" 'up -d --build quota-watch'
  [[ $(stat -f %Lp "$fixture/.env" 2>/dev/null || stat -c %a "$fixture/.env") == 600 ]] \
    || fail 'generated .env is not mode 600'

  cp "$fixture/.env" "$fixture/original.env"
  if env PATH="$fixture/bin:$PATH" FAKE_STATE="$fixture/state" DEPLOY_DIR="$fixture" \
    bash "$fixture/install.sh" > "$fixture/reinstall.out" 2>&1; then
    fail 'install overwrote an existing .env'
  fi
  cmp -s "$fixture/.env" "$fixture/original.env" || fail 'existing .env changed after rejected install'
  assert_contains "$fixture/reinstall.out" '已有 .env'

  env PATH="$fixture/bin:$PATH" FAKE_STATE="$fixture/state" DEPLOY_DIR="$fixture" \
    bash "$fixture/install.sh" --resume > "$fixture/resume.out" 2>&1 \
    || fail "install resume failed: $(cat "$fixture/resume.out")"
  cmp -s "$fixture/.env" "$fixture/original.env" || fail 'resume replaced existing credentials'
  assert_contains "$fixture/resume.out" '登录密码：00000000000000000000000000000000'
  printf 'PASS: first install, health and credentials, existing .env preservation\n'
}

make_update_fixture() {
  local name=$1 fixture
  fixture=$(make_fixture "$name")
  printf 'update\n' > "$fixture/state/mode"
  printf '# existing credentials\nCOMPOSE_PROJECT_NAME=wrong-project\nQUOTA_WATCH_USERNAME=existing-user\nQUOTA_WATCH_PASSWORD=existing-password\nQUOTA_WATCH_MASTER_KEY=existing-master-key\n' > "$fixture/.env"
  printf 'services:\n  quota-watch:\n    environment:\n      TEST_OVERRIDE: "1"\n' > "$fixture/override.yaml"
  printf '%s\n' "$fixture"
}

run_update_success_test() {
  local fixture output backup_dir
  fixture=$(make_update_fixture update_success)
  output="$fixture/update.out"
  cp "$fixture/.env" "$fixture/original.env"
  env PATH="$fixture/bin:$PATH" FAKE_STATE="$fixture/state" DEPLOY_DIR="$fixture" \
    QW_BACKUP_ROOT="$test_root/backups-update_success" bash "$fixture/update.sh" > "$output" 2>&1 \
    || fail "update failed: $(cat "$output")"

  cmp -s "$fixture/.env" "$fixture/original.env" || fail 'update replaced existing .env'
  assert_contains "$output" 'Quota Watch 已更新并通过健康检查'
  assert_contains "$fixture/state/docker.calls" "-p original-project -f $fixture/compose.yaml -f $fixture/override.yaml config --quiet"
  assert_contains "$fixture/state/docker.calls" "-p original-project -f $fixture/compose.yaml -f $fixture/override.yaml up -d --no-build --force-recreate --no-deps quota-watch"
  assert_contains "$fixture/state/docker.calls" '--mount type=volume,src=original-project_quota-watch-data,dst=/data,readonly'
  assert_absent "$fixture/state/docker.calls" 'volume rm'
  backup_dir=$(find "$test_root/backups-update_success" -mindepth 1 -maxdepth 1 -type d -print -quit)
  [[ -n $backup_dir && -s $backup_dir/data.tar && -s $backup_dir/old-image.tar ]] || fail 'data or old image backup was not saved'
  tar -tf "$backup_dir/data.tar" | grep -Fq './settings.db' || fail 'backup omitted existing data'
  cmp -s "$fixture/.env" "$backup_dir/.env" || fail 'backup omitted original .env'
  [[ -s $backup_dir/container-env.json ]] || fail 'backup omitted effective container environment'
  assert_contains "$backup_dir/compose-files.txt" "$fixture/override.yaml"
  [[ $(<"$fixture/state/current_container") == new-container ]] || fail 'update did not recreate the service'
  printf 'PASS: update preserves original Compose project, configuration and /data\n'
}

run_preflight_rejection_tests() {
  local fixture output kind
  for kind in environment_drift mount_drift external_env_file recovery_label rollback_image; do
    fixture=$(make_update_fixture "$kind")
    output="$fixture/update.out"
    touch "$fixture/state/$kind"
    if [[ $kind == external_env_file ]]; then
      printf '    env_file:\n      - ./external.env\n' >> "$fixture/override.yaml"
      printf 'EXTERNAL_SECRET=not-backed-up\n' > "$fixture/external.env"
    fi
    if env PATH="$fixture/bin:$PATH" FAKE_STATE="$fixture/state" DEPLOY_DIR="$fixture" \
      QW_BACKUP_ROOT="$test_root/backups-$kind" bash "$fixture/update.sh" > "$output" 2>&1; then
      fail "update accepted $kind"
    fi
    if [[ $kind == external_env_file ]]; then
      assert_contains "$fixture/state/docker.calls" 'config --no-env-resolution --format json'
      assert_contains "$output" '无法独立备份原 Compose 的外部凭据或配置文件'
      assert_absent "$fixture/state/git.calls" 'pull --ff-only'
    elif [[ $kind == recovery_label ]]; then
      assert_contains "$output" '当前服务运行在自动恢复卷上'
      assert_absent "$fixture/state/git.calls" 'pull --ff-only'
    elif [[ $kind == rollback_image ]]; then
      assert_contains "$output" '当前服务使用回滚镜像'
      assert_absent "$fixture/state/git.calls" 'pull --ff-only'
    fi
    assert_absent "$fixture/state/docker.calls" 'stop --time 45'
    assert_absent "$fixture/state/docker.calls" 'up -d --no-build --force-recreate'
    [[ $(<"$fixture/state/old_status") == running ]] || fail "preflight $kind stopped the old service"
  done
  printf 'PASS: credential drift, data mount drift, external env_file and restored deployments are rejected before downtime\n'
}

run_backup_recovery_test() {
  local fixture output
  fixture=$(make_update_fixture backup_failure)
  output="$fixture/update.out"
  touch "$fixture/state/backup_fail"
  cp "$fixture/.env" "$fixture/original.env"
  if env PATH="$fixture/bin:$PATH" FAKE_STATE="$fixture/state" DEPLOY_DIR="$fixture" \
    QW_BACKUP_ROOT="$test_root/backups-backup_failure" bash "$fixture/update.sh" > "$output" 2>&1; then
    fail 'update continued after data backup failed'
  fi
  assert_contains "$output" '数据备份失败'
  assert_contains "$fixture/state/docker.calls" 'stop --time 45 old-container'
  assert_contains "$fixture/state/docker.calls" 'start old-container'
  assert_absent "$fixture/state/docker.calls" 'up -d --no-build --force-recreate'
  [[ $(<"$fixture/state/old_status") == running ]] || fail 'old container was not restarted'
  cmp -s "$fixture/.env" "$fixture/original.env" || fail 'failed update changed credentials'
  printf 'PASS: failed backup restarts the old container without recreating it\n'
}

run_automatic_recovery_tests() {
  local kind fixture output backup_dir recovery_volume recovery_tag
  for kind in rollout_up_fail rollout_unhealthy; do
    fixture=$(make_update_fixture "$kind")
    output="$fixture/update.out"
    touch "$fixture/state/$kind"
    cp "$fixture/.env" "$fixture/original.env"
    if env PATH="$fixture/bin:$PATH" FAKE_STATE="$fixture/state" DEPLOY_DIR="$fixture" \
      QW_BACKUP_ROOT="$test_root/backups-$kind" bash "$fixture/update.sh" > "$output" 2>&1; then
      fail "update reported success after $kind: $(cat "$output"); calls: $(tail -n 8 "$fixture/state/docker.calls")"
    fi

    assert_contains "$output" '旧服务已自动恢复并通过健康检查'
    assert_contains "$output" '升级期间已对外执行的操作无法撤销'
    if [[ $kind == rollout_up_fail ]]; then
      assert_contains "$output" '新容器启动失败'
    else
      assert_contains "$output" '新容器未在 3 分钟内通过健康检查'
      [[ $(wc -l < "$fixture/state/sleep.calls") -ge 90 ]] \
        || fail 'unhealthy rollout did not exhaust the health-check window'
    fi

    backup_dir=$(find "$test_root/backups-$kind" -mindepth 1 -maxdepth 1 -type d -print -quit)
    [[ -n $backup_dir && -s $backup_dir/data.tar && -s $backup_dir/old-image.tar ]] \
      || fail "missing rollback backup after $kind"
    recovery_volume=$(<"$fixture/state/recovery_volume")
    recovery_tag=$(<"$fixture/state/recovery_tag")
    [[ $recovery_volume == quota-watch-restore-* && $recovery_volume != original-project_quota-watch-data ]] \
      || fail "rollback did not create an independent volume after $kind"
    [[ $recovery_tag == quota-watch-backup:* ]] || fail "rollback did not pin the old image after $kind"
    [[ $(<"$fixture/state/current_container") == restored-container && $(<"$fixture/state/restored_status") == running ]] \
      || fail "rollback container did not start after $kind"
    [[ $(<"$fixture/state/new_status") == exited ]] || fail "failed new container kept running after $kind"

    cmp -s "$fixture/state/data/settings.db" "$fixture/state/recovery-data/settings.db" \
      || fail "rollback did not restore the complete /data backup after $kind"
    cmp -s "$fixture/.env" "$fixture/original.env" || fail "rollback changed original credentials after $kind"
    assert_contains "$backup_dir/compose.auto-recovery.yaml" "image: $recovery_tag"
    assert_contains "$backup_dir/compose.auto-recovery.yaml" 'pull_policy: never'
    assert_contains "$backup_dir/compose.auto-recovery.yaml" "name: $recovery_volume"
    assert_contains "$fixture/state/docker.calls" 'volume create --label quota-watch.recovery=true'
    assert_contains "$fixture/state/docker.calls" "--mount type=volume,src=$recovery_volume,dst=/data"
    assert_contains "$fixture/state/docker.calls" "inspect --format {{.Image}} restored-container"
    assert_contains "$fixture/state/docker.calls" 'compose.auto-recovery.yaml up -d --no-build --force-recreate --no-deps quota-watch'
    assert_absent "$fixture/state/docker.calls" 'start old-container'
  done
  printf 'PASS: failed rollout and unhealthy rollout restore the old image to an independent healthy volume\n'
}

run_recovery_failure_test() {
  local fixture output backup_dir
  fixture=$(make_update_fixture recovery_failure)
  output="$fixture/update.out"
  touch "$fixture/state/rollout_up_fail" "$fixture/state/recovery_volume_create_fail"
  cp "$fixture/.env" "$fixture/original.env"
  if env PATH="$fixture/bin:$PATH" FAKE_STATE="$fixture/state" DEPLOY_DIR="$fixture" \
    QW_BACKUP_ROOT="$test_root/backups-recovery_failure" bash "$fixture/update.sh" > "$output" 2>&1; then
    fail 'update reported success when automatic recovery failed'
  fi

  assert_contains "$output" '无法创建独立恢复卷'
  assert_contains "$output" '自动恢复失败；请按 UPDATING.md 手动恢复'
  assert_absent "$output" '旧服务已自动恢复并通过健康检查'
  assert_contains "$fixture/state/docker.calls" 'stop --time 45 new-container'
  [[ $(<"$fixture/state/new_status") == exited ]] || fail 'failed recovery left the new container running'
  [[ ! -f $fixture/state/recovery_volume ]] || fail 'failed recovery unexpectedly created a volume'
  backup_dir=$(find "$test_root/backups-recovery_failure" -mindepth 1 -maxdepth 1 -type d -print -quit)
  [[ -n $backup_dir && -s $backup_dir/data.tar && -s $backup_dir/old-image.tar ]] \
    || fail 'failed recovery did not preserve the backup files'
  cmp -s "$fixture/.env" "$fixture/original.env" || fail 'failed recovery changed original credentials'
  [[ -s $fixture/state/data/settings.db ]] || fail 'failed recovery changed original data'
  printf 'PASS: failed automatic recovery exits nonzero with manual recovery guidance and intact backups\n'
}

run_install_tests
run_update_success_test
run_preflight_rejection_tests
run_backup_recovery_test
run_automatic_recovery_tests
run_recovery_failure_test
