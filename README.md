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
to look and one place to back up. See
[docs/prod-layout.md](docs/prod-layout.md) for details.

## Handle server

Production needs a handle server so `hdl.handle.net/1794/...` links resolve.
It's the `handle` service (off unless its profile is enabled), and it needs
one-time setup plus registration with handle.net: see
[docs/handle-server.md](docs/handle-server.md), and our server-docs repo's
"handle server" documentation for internal details.

## Scheduled jobs

Compose makes cron hard, and DSpace needs a *lot* of cron love. We had to put
together some weird magic to make it work in a semi-nice way. See
[docs/cron.md](docs/cron.md) for details.

## Backups

DSpace is a bit complex where backups are concerned, so we have two custom
scripts for doing backups. See [docs/backups.md](docs/backups.md) for details.

## Container logs

Each service writes its stdout/stderr to its own file in `CONTAINER_LOG_DIR`.
Production needs logrotate set up to avoid disk exhaustion. See
[docs/container-logs.md](docs/container-logs.md) for details.

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
