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
  and the logs roll over daily via some kind of built-in DSpace magic. Mount
  them on the host or use the `cli` service to read them (e.g., via an
  in-container `tail` or `cat` of `/usr/local/dspace/log/dspace-cli.log`)

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
