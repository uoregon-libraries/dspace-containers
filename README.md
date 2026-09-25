# DSpace Compose Wrapper Thing

This is a compose setup with various Dockerfiles for running DSpace. This is
meant to work for both development and production, but as such does require
careful per-environment setup.

## Get projects

To use this, you must first check out a copy of both the REST and Angular
projects. In our case, it looks a bit like this:

```bash
git checkout git@github.com:uoregon-libraries/scholarsbank-angular.git ./dspace-angular
git checkout git@github.com:uoregon-libraries/scholarsbank-rest.git ./dspace-rest
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
docker compose --profile tools run --rm cli <command>
```

Notes:

- Yes, this is unweildy, but it ensures the `cli` container never starts up
  with other services
  - A bash alias can help: `alias dspace-cli='docker compose --profile tools run --rm cli'`
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

1. Set `CRON_MAILTO` in `.env` to where job failures should be emailed
1. Build the renderer: `make bin/render-crontab`. This needs Go, but the
   result is a static binary, so you can build it elsewhere and copy it to
   `bin/` on the server.
1. As the podman user, from the project dir, render and install the crontab
   file (see below). Always render to a file first; if rendering fails, it
   prints nothing.
1. You *must* rerender if you change `conf/crontab` or `CRON_MAILTO`, move the
   project to a new dir, or move `podman-compose`

To set up a system crontab:

```bash
bin/render-crontab -user "$(whoami)" > /tmp/sb.cron
sudo install -m 644 -o root -g root /tmp/sb.cron /etc/cron.d/scholarsbank
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
  status, which cron emails to `CRON_MAILTO`
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
work: `bin/render-crontab > /dev/null`.

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
1. Reindex: `docker compose --profile tools run --rm cli index-discovery -b`
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
1. Import statistics index: `docker compose --profile tools run --rm solr-import-statistics`
1. Reindex search index: `docker compose --profile tools run --rm index-discovery -b`
1. Generate site-wide statistics files: `docker compose --profile tools run --rm update-stats`

### Create local admin

You'll probably want a local admin for easier access. Use the `cli` service:

```bash
docker compose --profile tools run --rm cli create-administrator -e admin@example.org -p adm -f Ad -l Min
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
service (seen in the example compose override):

- Enable the `smtpdebug` service in your compose override
- Mount the `smtp-debug-logs` volume both in the `smtpdebug` service *and* the
  web service! If you don't add the volume to `web`, you won't be able to
  easily see the captured emails.
- Set `MAIL_SERVER=smtpdebug` in your `.env` file, and any dummy from / admin
  emails you like.
- Start the stack with the smtpdebug service, not just web, e.g., `podman
  compose up -d web smtpdebug`
- Send an email and view it: any generated emails will be visible under the URL
  path `/.smtp-debug`

A quick way to test: log in as admin and reset somebody's password.
