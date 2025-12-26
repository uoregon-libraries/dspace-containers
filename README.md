# DSpace Compose Wrapper Thing

This is a compose setup with various Dockerfiles for recreating a fairly close
approximation of our production setup for running DSpace. This is for
*development*, not to stand up a new environment in compose.

## Get projects

To use this, you must first check out a copy of both the REST and Angular
projects. In our case, it looks a bit like this:

```bash
git checkout git@github.com:uoregon-libraries/scholarsbank-angular.git ./dspace-angular
git checkout git@github.com:uoregon-libraries/scholarsbank-rest.git ./dspace-rest
```

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

## Customize Compose Config

Use `compose.override.example.yml` to create a compose override. Two very minor
notes *you must not ignore*:

- This is mandatory! If you don't provide an override, you won't have the app
  exposed to the host machine, DSpace won't know its URLs, etc.
- Do not just copy and paste! Some of the example settings only make sense in
  certain contexts!

## Start it up!

Finally, start up the stack and browse to `http://localhost:8080`
