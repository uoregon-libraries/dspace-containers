# Shared by scripts/backup and scripts/backup-solr; see "Backups" in
# README.md. Not meant to be run directly.
#
# A job sets podman_user, dest, and keep_days, then calls:
#
#   backup_init <job> [dir]  checks, locking, and logging; snapshots go in
#                            <dest>/<dir> if given
#   snapshot_begin           creates the run dir ($run); $prev is the last one
#   snapshot_finish          publishes $run as "latest", applies retention

step() {
  printf '[%s] %s\n' "$(date '+%F %T')" "$*"
}

backup_init() {
  local job=$1 subdir=${2:-}

  if [ "$(id -u)" -ne 0 ]; then
    echo "$0: must run as root" >&2
    exit 1
  fi

  # cron's PATH is minimal, and some distros keep runuser in sbin
  export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

  checkout=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)
  dest=$(cd "$dest" && pwd -P)
  if [ -n "$subdir" ]; then
    dest=$dest/$subdir
    mkdir -p "$dest"
  fi

  # Everything uses absolute paths, and the podman user may not be able to
  # read cron's working directory (root's home)
  cd /

  # Backups hold password hashes and private keys
  umask 077

  podman_uid=$(id -u "$podman_user")
  podman_home=$(getent passwd "$podman_user" | cut -d: -f6)

  # One lock per job, per install, as in scripts/cron-job
  local install
  install=$(printf '%s' "$checkout" | tr -c 'A-Za-z0-9._-' '_')
  exec 9>"/run/lock/$job$install.lock"
  if ! flock -n 9; then
    echo "$0: skipped; the previous run is still going"
    exit 1
  fi

  name=$(date +%F_%H%M%S)
  mkdir -p "$dest/logs"
  log=$dest/logs/$name.log

  # From here on, output goes to the log, which cron only sees on failure
  exec 3>&1 >"$log" 2>&1
  trap backup_on_exit EXIT

  project=$(env_setting COMPOSE_PROJECT_NAME)
  if [ -z "$project" ]; then
    project=$(basename "$checkout" | tr '[:upper:]' '[:lower:]')
  fi

  step "$job: backing up $checkout to $dest/$name"
}

backup_on_exit() {
  local rc=$?
  if [ "$rc" -ne 0 ]; then
    echo "backup failed: exit status $rc"
    cat "$log" >&3
  fi
}

snapshot_begin() {
  # Anything still partial is from a run that died; we hold the lock, so no
  # other run is using it
  find "$dest" -mindepth 1 -maxdepth 1 -type d -name '*.partial' -exec rm -rf -- {} +
  run=$dest/$name.partial
  mkdir "$run"

  prev=
  if [ -L "$dest/latest" ] && [ -d "$dest/latest" ]; then
    prev=$(readlink -f "$dest/latest")
    step "previous snapshot: $prev"
  fi
}

snapshot_finish() {
  mv "$run" "$dest/$name"
  run=$dest/$name

  # Relative, so the link survives the backup dir being mounted elsewhere
  ln -sfn "$name" "$dest/.latest.tmp"
  mv -Tf "$dest/.latest.tmp" "$dest/latest"
  step "latest -> $name"

  step "removing snapshots older than $keep_days days"
  local cutoff dir base
  cutoff=$(date -d "-$keep_days days" +%F)
  for dir in "$dest"/[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]_*; do
    base=${dir##*/}
    if [ -d "$dir" ] && [[ ${base%%_*} < $cutoff ]] && [ "$base" != "$name" ]; then
      step "removing $base"
      rm -rf -- "$dir"
    fi
  done
  find "$dest/logs" -type f -name '*.log' -mtime +365 -delete

  step "done"
}

# Runs a command as the podman user, with the environment rootless podman
# needs to find that user's containers
as_podman() {
  runuser -u "$podman_user" -- env HOME="$podman_home" \
    XDG_RUNTIME_DIR="/run/user/$podman_uid" \
    DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$podman_uid/bus" \
    "$@"
}

# Reads a setting from .env the way scripts/compose does: last assignment
# wins, inline comments and quotes dropped
env_setting() {
  sed -n "s/^[[:space:]]*$1[[:space:]]*=//p" "$checkout/.env" |
    tail -n 1 | sed 's/[[:space:]]#.*//' | tr -d "\"'[:space:]"
}

# Prints the name of a compose service's running container
container_for() {
  local names
  names=$(as_podman podman ps --format '{{.Names}}' \
    --filter "label=io.podman.compose.project=$project" \
    --filter "label=com.docker.compose.service=$1") || true
  if [ -z "$names" ] || [ "$(wc -l <<<"$names")" -ne 1 ]; then
    echo "expected one running $1 container in project $project, found: ${names:-none}" >&2
    return 1
  fi
  echo "$names"
}

# Copies a directory into the snapshot, hard-linking whatever is unchanged
# since the previous one. Extra args go to rsync. Exit 24 (files vanished
# mid-copy) isn't a failure.
snapshot_dir() {
  local src=$1 sub=$2 rc=0
  shift 2
  local args=(-aH --numeric-ids --stats "$@")
  if [ -n "$prev" ] && [ -d "$prev/$sub" ]; then
    args+=(--link-dest="$prev/$sub")
  fi
  mkdir -p "$run/$sub"
  rsync "${args[@]}" "$src/" "$run/$sub/" || rc=$?
  if [ "$rc" -ne 0 ] && [ "$rc" -ne 24 ]; then
    return "$rc"
  fi
}
