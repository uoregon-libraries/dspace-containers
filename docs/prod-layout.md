# Production Layout

*Paths below use `/path/to/scholarsbank` in place of the real directory.*

Layout at a high level:

- `/path/to/scholarsbank/app/`: this checkout, including `.env`,
  `compose.override.yml`, `exports/`, and `bin/`
- `/path/to/scholarsbank/volumes/<volume>/`: host directories backing named
  volumes, e.g., `volumes/handle-server` and `volumes/caddy-conf`
- *Not* the systemd unit (see below)

## Notes:

- **Set `COMPOSE_PROJECT_NAME` in `.env`** (e.g., `scholarsbank`). Otherwise
  the project name comes from the checkout's directory, `app`, and so does
  every container and volume name (`app_db`).
- Volumes
  - Volume data never goes inside the checkout. Keeping them side by side means
    nothing can be committed by accident, and nobody gets the idea that a
    `./volumes/...` bind in the override is best-practice.
  - Volumes are only for data we *regularly need to read and write*. Podman
    permissions get *ugly* if you don't run the right commands first.
  - Always use the long form for bind mounts, never the one-liners. The long form
    keeps things like read-only rules intact, avoids accidentally having two
    places for one mount (e.g., multiple services need to have the *exact* same
    statistics and assetstore volumes)
  - Use absolute `device:` paths in the production override, not `${PWD}`.
    `driver_opts` are baked in when a volume is created, so a bad path sticks
    until you remove the volume.
- With SELinux, containers can only read the volume directories if they're
  labeled for it. A persistent rule (survives relabels, unlike `chcon`):

  ```bash
  sudo semanage fcontext -a -t container_file_t '/path/to/scholarsbank/volumes(/.*)?'
  sudo restorecon -R /path/to/scholarsbank/volumes
  ```

## Running under systemd

The stack runs as a rootless podman *user* unit belonging to the podman user
(e.g., `dspace`). That user needs lingering (`sudo loginctl enable-linger
dspace`) so the unit starts at boot without anybody logging in.

`conf/scholarsbank@.service` is a template unit, which keeps host paths out of
the repo: the instance name is the checkout's path, escaped, and the unit uses
that as its working directory. `/path/to/scholarsbank/app` becomes
`scholarsbank@path-to-scholarsbank-app.service`.

Install it as the podman user, from the checkout:

```bash
mkdir -p ~/.config/systemd/user
cp conf/scholarsbank@.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now "scholarsbank@$(systemd-escape --path "$PWD").service"

# Day to day (tab completion fills in the instance name)
systemctl --user status scholarsbank@path-to-scholarsbank-app
journalctl --user -u scholarsbank@path-to-scholarsbank-app
```

Notes:

- Copy the unit; don't `systemctl link` it. Linked units outside systemd's own
  directories can run afoul of SELinux, and `disable` deletes the link. After
  changing the unit in the repo, copy it again and `daemon-reload`.
- `systemctl --user` needs the user's session bus. `ssh` in as the user, don't
  `sudo` to switch users. It gets messy.
- The unit runs `scripts/compose up`, so it starts every service in the
  profiles `COMPOSE_PROFILES` enables, e.g., `handle` once that profile is on.
  `scripts/compose` is `podman compose` with `COMPOSE_PROFILES` passed as
  `--profile` flags, because podman-compose before 1.6.0 (such as a distro
  package) silently ignores it. Use it instead of a bare `podman compose`
  anywhere profiled services matter (`down`, `ps`, `logs`, ...).
