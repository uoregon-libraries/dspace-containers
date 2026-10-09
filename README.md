# DSpace Compose Wrapper Thing

This is a compose setup with various Dockerfiles for running DSpace. This is
meant to work for both development and production, but as such does require
careful per-environment setup.

## Get projects

To use this, you must first check out a copy of both the REST and Angular
projects. In our case, it looks a bit like this:

```bash
git clone git@github.com:uoregon-libraries/scholarsbank-angular.git ./dspace-angular
git clone git@github.com:uoregon-libraries/scholarsbank-rest.git ./dspace-rest
```

**Note**: on each image build (e.g., `docker compose build`), you will be
adding most of the files from *your local copy* of those two repositories. If
you aren't careful, this can be a **huge debugging nightmare**!

**Do not use `local.cfg`**: this file is replaced with one that forces the
stack to behave a certain way.

If you're seeing odd issues when you build and run the stack, consider forcibly
resetting the state of those repos (e.g., `git clean`, `git reset --hard`,
etc.) and doing a full image rebuild.

## Build images

Build the images, e.g., `docker compose build`. This can take a long time....

## CLI / cron jobs / one-offs

The compose setup has a `cli` service under the "tools" profile. This allows
running cron jobs and any other short-lived commands in a CLI-optimized
container:

```bash
docker compose --profile tools run --rm --no-deps cli <command>
```

Notes:

- Yes, this is unweildy, but it ensures the `cli` container never starts up
  with other services
  - A bash alias can help: `alias dspace-cli='docker compose --profile tools run --rm --no-deps cli'`
- **Never drop `--no-deps`**. Without it, every command, even a simple `ls`,
  will restart the entire stack!
- The image adds `/usr/local/dspace/bin` to `$PATH` so that any commands there
  can be specified without a full path, which hopefully makes it slightly less
  unweildy.
- Anything that is *not* a known command / binary (in the image's path) will
  automatically be treated as a DSpace command, which means we "route" it
  through `/usr/local/dspace/bin/dspace`. e.g., running `foo` will actually
  invoke `/usr/local/dspace/bin/dspace foo`.
- DSpace's logs go to files in the `cli-logs` volume (`/usr/local/dspace/log`),
  one per run, named for the command and its start time (UTC), e.g.,
  `index-discovery_2026-09-24_030000.log`. Mount them on the host or use the
  `cli` service to read them (e.g., via an in-container `tail` or `cat`)

## Production host layout

Everything a deployment needs lives under one directory, so there's one place
to look and one place to back up. Paths below use `/path/to/scholarsbank` for
wherever that is (e.g., `/srv/sb`).

- `/path/to/scholarsbank/app/`: this checkout, including `.env`,
  `compose.override.yml`, `exports/`, and `bin/`
- `/path/to/scholarsbank/volumes/<volume>/`: host directories backing named
  volumes, e.g., `volumes/handle-server` and `volumes/caddy-conf`
- *Not* the systemd unit (see below)

Notes:

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

### Running under systemd

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
- To add a service to a running stack without restarting the site, enable its
  profile and run `scripts/compose up -d <service>`.

## Handle server

Production needs a handle server so `hdl.handle.net/1794/...` links resolve.
It's the `handle` service (off unless its profile is enabled), and it needs
one-time setup plus registration with handle.net: see
[docs/handle-server.md](docs/handle-server.md), and our server-docs repo's
"handle server" documentation for internal details.

## Scheduled jobs

Compose makes cron hard, and DSpace needs a *lot* of cron love. We had to put
together some weird magic to make it work in a semi-nice way.

High-level info:

- The host's cron runs jobs in a throwaway container (using the `cli` service)
- `conf/crontab` lists all jobs in a sort of "pseudo-crontab" format. It is
  *not usable* as-is.
- `bin/render-crontab` fills in `conf/crontab` and prints a usable crontab to
  stdout (suitable for dropping into `/etc/cron.d/`, for instance)

### Setup

Probably only for production. Dev definitely doesn't need cron jobs, and
staging *probably* doesn't.

1. Build the renderer: `make bin/render-crontab`. This needs Go, but the
   result is a static binary, so you can build it elsewhere and copy it to
   `bin/` on the server.
1. Render the crontab to a file (see below). Always render to a file first; if
   rendering fails, it prints nothing.
1. Install the file on the server
1. You *must* rerender if you change `conf/crontab`, the mail address, the
   podman user, or move the project or `podman-compose` on the server

The renderer takes everything about *the production server* as flags:

- `-dir` (required): absolute path the project is deployed
- `-user` (required): the podman user the jobs run as (use `-user ""` to render
  a user crontab instead of a system crontab)
- `-mailto` (required): where cron emails job failures (use `-mailto ""` to
  leave MAILTO out of the file)
- `-compose-dir` (optional): the directory holding `podman-compose` on
  production; unnecessary if it's in `/usr/local/bin`, `/usr/bin`, or `/bin`

e.g.:

```bash
bin/render-crontab -dir /path/to/scholarsbank/app -user dspace \
  -mailto dspace-admins@example.org -compose-dir /home/dspace/.local/bin \
  > sb.cron
```

Then copy `sb.cron` to the server and install it:

```bash
sudo install -m 644 -o root -g root sb.cron /etc/cron.d/scholarsbank
```

The file must be owned by root and not group- or world-writable

*Note*: setting up a user crontab needs a bit of care and isn't directly
supported. This info can help, but you'll be mostly on your own.

### How jobs run

Jobs run through a wrapper (`scripts/cron-job`). You can also run that manually
to get an exact test of how cron will behave.

Notes about this wrapper:

- It uses the `cli` service, so only the dspace subcommand or binary is
  specified, e.g., `scripts/cron-job index-discovery`
- It runs compose from the project root; podman compose needs this to load `.env`
  and `compose.override.yml`
- A successful job prints nothing while a failed job prints its output and exit
  status, which cron emails to the renderer's `-mailto` address
- If the same command is still running from its last scheduled run, the new
  run is skipped and reported as a failure
- Jobs never start dependencies - if the stack is down, they will fail (no db,
  no Solr, etc.) and email you
- It calls `podman-compose` directly (not `podman compose`), and it expects the
  stack's user to have lingering enabled; this is required for rootless podman
  anyway, so should be a non-issue

### Modifying jobs

If you edit `conf/crontab`, a few things to keep in mind:

- Don't put user, path, mailto, etc. here: the renderer does this
- Don't try to set environment variables here unless you actually know what
  they do: they will affect the compose run, *not* the container, or at least
  not directly. You'll get confused. Don't do it.
- Each entry is a cron schedule followed by a command sent to the `cli`
  service. Understand the cli service (see above) before you touch this file.
- One command per line, and don't get fancy! ";", "&&", pipes, and redirects
  run on the host, *not the container*. To chain commands in the container,
  quote them: `sh -c 'dspace a && dspace b'`. Better yet, don't do this. Write
  a script instead.
- Cron treats a bare "%" as a newline, so write it as "\%"
- Again, times use the host clock/timezone. In practice this usually matches
  our containers' times, but be aware just in case.

The renderer refuses to print anything if a schedule is malformed (e.g., a
missing field) or a command has host-shell syntax (`;`, `&`, `|`, redirects,
`$`, a bare `%`, ...) outside of quotes. Run it after editing to check your
work: `bin/render-crontab -dir /x -user x -mailto "" > /dev/null` (the
flag values don't matter just to test rendering's success).

## Dev / test / staging

### Get data

If you're doing dev or standing up a staging server, you'll want to get an
export from production and import it locally:

1. Stop the stack if it's running
1. `ssh` into the server that runs your database
1. Execute `pg_dump -U dspace dspace > /tmp/pg.sql`
1. `scp` or `rsync` the export into `exports/db`, e.g., `scp server@university.edu:/tmp/pg.sql ./exports/db`
1. Get your `exports/db` into the db container, e.g., with a compose override
   that adds a volume: `./exports/db:/docker-entrypoint-initdb.d`
1. *Remove* your current database volume, e.g., `docker volume rm dspace_db`
1. Start the stack up again, and postgres will import the SQL fairly quickly
   (faster than the angular side boots up)
1. Reindex: `docker compose --profile tools run --rm --no-deps cli index-discovery -b`
1. Note: if you aren't mirroring bitstreams, you will see a *lot* of errors
   while DSpace tries and fails to index full-text data from PDFs and other
   documents. You can safely ignore these.

For statistics data:
1. `ssh` into the server running DSpace
1. Execute `[dspace]/bin/dspace solr-export-statistics`
1. `scp` or `rsync` the exported csvs into `exports/solr`
1. Get your `exports/solr` into the rest container, e.g., with a compose override
   that adds a volume: `./exports/solr:/usr/local/dspace/solr-export`
1. *Remove* your current solr volume, e.g., `docker volume rm dspace_solr`
1. Restart the stack
1. Import statistics index: `docker compose --profile tools run --rm --no-deps cli solr-import-statistics`
1. Reindex search index: `docker compose --profile tools run --rm --no-deps cli index-discovery -b`
1. Generate site-wide statistics files: `docker compose --profile tools run --rm --no-deps cli update-stats`

### IdP

For dev, we use [go-saml][go-saml-github] (the `idp` service in the `local-dev`
profile) and a local key pair. Set `COMPOSE_PROFILES=local-dev` and
`DEV_IDP_URL` (a URL your browser can reach on port 8081) in `.env`, publish
the port and make `rest` wait for `idp` in your compose override (see
`compose.override.example.yml`), and start the stack as usual.
`SAML_RELYING_PARTY_ID` must be `sso` for the dev IdP.

On startup the IdP registers the `DEV_IDP_USERS` (default `alice,bob`; each
user's password is their name), then waits for DSpace and registers it as a
service provider. Choose the SSO login option in DSpace and sign in as one of
those users. The first login creates the DSpace account (`SAML_AUTOREGISTER`).

**Note 1**: Browse via `localhost` or HTTPS, not a plain-HTTP IP or hostname
(e.g., `http://192.168.56.100:8080`). Use an ssh tunnel (see below) if you have
to request DSpace via IP address or non-https hostname (e.g., running on a VM
instead of your desktop).

(*Why? DSpace hard-codes the SAML cookie to always be secure. Usually a good
idea for SAML, but not so great for those of us doing dev on VMs. Browsers
silently drop `Secure` cookies over plain HTTP everywhere except `localhost`.
The SAML login works, REST sees authentication, creates user, etc. But the UI
never sees the cookie.*)

As promised, tunnel help follows. Note that if you tunnel, you must also set
`PUBLIC_HOST=localhost` in `.env`.

```bash
ssh -L 8080:localhost:8080 -L 8081:localhost:8081 <vm>
```

**Note 2**: If new DSpace SAML keys need to be built, you just use `openssl` as
below, but make sure you use `podman compose down` to stop and remove all
containers. DSpace only reads the keys on startup, and podman secrets are funky
as it is.

```bash
openssl req -x509 -newkey rsa:3072 -nodes -days 3650 \
  -keyout conf/rest/dev-saml-sp.key -out conf/rest/dev-saml-sp.crt \
  -subj "/CN=scholarsbank-dev-sp"
```

**Note 3**: The IdP doesn't support single logout, so to switch users, log out
of DSpace and then either clear the browser's cookies for the IdP, or restart
it: `docker compose restart idp`. The IdP keeps everything in memory and
re-provisions itself when it starts.

[go-saml-github]: <https://github.com/uoregon-libraries/go-saml>

### Create local admin

You'll probably want a local admin for easier access. Use the `cli` service:

```bash
docker compose --profile tools run --rm --no-deps cli create-administrator -e admin@example.org -p adm -f Ad -l Min
```

### Configure

Copy `.env.example` to `.env` and edit it. This is **mandatory**. All
per-environment settings live in `.env`, and you need to understand them and
set them for your setup. **Note**: variables exported in your shell override
`.env` values silently. For dev systems where the environment can be easily
polluted, be careful to check for collisions.

A `compose.override.yml` is optional and only needed for structural changes:
angular's live-reload, volume overrides, etc. See
`compose.override.example.yml`.

One-off / temporary Caddy rules are dropped into the `caddy-conf` volume,
applied with a restart or reload of the `web` service. See
[docs/caddy-drop-ins.md](docs/caddy-drop-ins.md).

### Start it up!

Finally, start up the stack and browse to `http://localhost:8080`

### Emails

In dev or staging, you don't want emails being sent by mistake, but you still
probably want to test out the email-sending capabilities. Enter the `smtpdebug`
service (part of the `local-dev` profile):

- Enable the profile: `COMPOSE_PROFILES=local-dev` in your `.env` file. Note
  that this also enables the dev IdP (see above).
- Mount the `smtp-debug-logs` volume on the web service in your compose
  override (see `compose.override.example.yml`). If you don't, you won't be
  able to easily see the captured emails.
- Set `MAIL_SERVER=smtpdebug` in your `.env` file, and any dummy from / admin
  emails you like.
- Start the whole stack, not just web (nothing depends on smtpdebug), e.g.,
  `docker compose up -d`
- Send an email and view it: any generated emails will be visible under the URL
  path `/.smtp-debug`

A quick way to test: log in as admin and reset somebody's password.
