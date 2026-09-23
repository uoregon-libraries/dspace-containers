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

## Get data

If you're doing dev or standing up a staging server, you'll want to get an
export from production and import it locally:

*(Note: once we're in containers for prod, we'll have to revisit this!)*

1. Stop the stack if it's running
1. `ssh` into the server that runs your database
1. Execute `pg_dump -U dspace dspace > /tmp/pg.sql`
1. `scp` or `rsync` the export into `exports/db`, e.g., `scp server@university.edu:/tmp/pg.sql ./exports/db`
1. Get your `exports/db` into the db container, e.g., with a compose override
   that adds a volume: `./exports/db:/docker-entrypoint-initdb.d`
1. *Remove* your current database volume, e.g., `docker volume rm dspace_db`
1. Start the stack up again, and postgres will import the SQL fairly quickly
   (faster than the angular side boots up)
1. Reindex: `docker compose exec rest /usr/local/dspace/bin/dspace index-discovery -b`

For statistics data:
1. `ssh` into the server running DSpace
1. Execute `[dspace]/bin/dspace solr-export-statistics`
1. `scp` or `rsync` the exported csvs into `exports/solr`
1. Get your `exports/solr` into the rest container, e.g., with a compose override
   that adds a volume: `./exports/solr:/usr/local/dspace/solr-export`
1. *Remove* your current solr volume, e.g., `docker volume rm dspace_solr`
1. Restart the stack
1. Import statistics index: `docker compose exec rest /usr/local/dspace/bin/dspace solr-import-statistics`
1. Reindex search index: `docker compose exec rest /usr/local/dspace/bin/dspace index-discovery -b`
1. Generate site-wide statistics files: `docker compose exec rest /usr/local/dspace/bin/update-stats`

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

## Create local admin

You'll probably want a local admin for easier access. Use the `cli` service:

```bash
docker compose --profile tools run --rm cli create-administrator -e admin@example.org -p adm -f Ad -l Min
```

## Configure

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

## Start it up!

Finally, start up the stack and browse to `http://localhost:8080`
