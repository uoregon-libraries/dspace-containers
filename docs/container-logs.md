# Container logs

*Paths below use `/path/to/scholarsbank` in place of the real directory.*

Every service writes its stdout/stderr to its own file in `CONTAINER_LOG_DIR`:
`web.log`, `rest.log`, etc. These are podman's normal `k8s-file` logs, just
kept somewhere stable instead of in each container's storage.

Why not podman's defaults?

- Podman keeps `k8s-file` logs inside the container's storage, so they're
  deleted along with the container. The systemd unit runs `compose down` every
  time it stops, so a restart would wipe every log.
- The `journald` driver needs per-user journals, which a rootless podman user
  only gets with persistent journal storage and a non-system UID. Without
  those, the user can't read the logs (and `podman logs` fails). Either fix is
  a system-wide journald change we don't want to deal with when there's
  user-level improvements we can make.

## Setup

Set `CONTAINER_LOG_DIR` in `.env`. Two hard rules:

- It must be absolute: Podman stores a relative path as-is and resolves it
  against whatever directory podman runs from later, so `podman logs` or a
  restart from somewhere else reads or writes the wrong file. You can use
  `${PWD}/...` in place of `./...`.
- It must already exist, otherwise conmon can't open the file and the container
  won't start (`conmon failed: exit status 1`).

In production, use a `logs/` dir (alongside `app/` and `volumes/`):

```bash
mkdir /path/to/scholarsbank/logs
```

It's *not* under `volumes/`, so backups skip it.

Dev should use the `.env.example` value, `${PWD}/logs/`.

## Reading logs

- `podman logs` and `podman compose logs` still work. One quirk: a recreated
  container's `podman logs` also shows older containers' output, since it
  reads the whole file.
- Or read the files directly. Each line is
  `<timestamp> <stdout|stderr> <F|P> <message>`, where `P` means a partial line
  continued on the next one.

## Rotation

On dev, just delete files as needed. In prod, the podman user needs a logrotate
config and cron job. e.g., `/path/to/scholarsbank/logrotate.conf`:

```
/path/to/scholarsbank/logs/*.log {
  daily
  maxsize 100M
  rotate 30
  missingok
  notifempty
  copytruncate
  compress
}
```

And an hourly cron job, e.g., in `/etc/cron.d/scholarsbank-logrotate`:

```
0 * * * *	dspace	/usr/sbin/logrotate -s /path/to/scholarsbank/logrotate.status /path/to/scholarsbank/logrotate.conf
```

Notes:

- **Keep the `daily`** (or another interval). Without one, logrotate uses its
  default of rotating at 1 MiB, and `maxsize` never kicks in: `maxsize` only
  rotates *sooner* than the interval would. With an hourly cron, this config
  rotates once a day, or sooner if a file passes 100M.
- `copytruncate` is safe here: conmon opens the file for appending, so after a
  truncate it writes from the start, with no gap of null bytes. A few lines
  written between the copy and the truncate can be lost.
- Don't also set podman's `log_size_max` (`containers.conf`) or a `max-size`
  log option. When a file hits that size, conmon just empties it, which fights
  logrotate and loses the history.

## Why `rest`, `cli`, and `handle` use YAML anchors

`cli` and `handle` used to `extends` `rest`. podman-compose (through at least
1.5.0) shallow-copies the base service for `extends`, so nested maps like
`logging.options` end up shared: whichever service sets its log path last wins
for all three. `!override` doesn't help, since it crashes on nested maps in
those versions.

So the shared settings live in `x-rest` and `x-rest-env` in `compose.yml`,
pulled in with YAML merge keys (`<<: *rest`). The YAML parser handles merge
keys, not podman-compose, so they work the same on old versions.

The catch: `<<:` merges only one level deep. Any key a service sets *replaces*
the shared value outright:

- A service that sets `environment:` must start it with `<<: *rest-env`, or it
  loses every shared DSpace setting.
- A service that sets `volumes:` gets only what it lists. `handle` relies on
  this: it mounts only `handle-server`, which holds the handle server's
  private keys and shouldn't be visible to `rest` or `cli`.
