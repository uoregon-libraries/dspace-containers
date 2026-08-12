# DSpace Compose Wrapper Thing

This is a compose setup with various Dockerfiles for recreating a fairly close
approximation of our production setup for running DSpace. This is *mostly* for
development, but we're trying to make it more production-ready.

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

Next, you'll want to get an export and import it locally:

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

## Create local admin

You'll probably want a local admin for easier access:

```bash
docker compose run --rm -it rest /usr/local/dspace/bin/dspace create-administrator -e admin@example.org -p adm -f Ad -l Min
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

## Start it up!

Finally, start up the stack and browse to `http://localhost:8080`
