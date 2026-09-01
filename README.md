# DSpace Compose Wrapper Thing

This is a compose setup with various Dockerfiles for recreating a fairly close
approximation of our production setup for running DSpace. This is *mostly* for
development, but we're trying to make it more production-ready.

## Get projects

The REST and Angular sources (`dspace-rest/` and `dspace-angular/`) are git
submodules. On a fresh clone, get everything in one shot:

```bash
git clone --recurse-submodules git@github.com:uoregon-libraries/dspace-containers.git
```

If you already cloned without that flag (the submodule dirs will be empty):

```bash
git submodule update --init
```

### Submodules in sixty seconds

As much as I hate to put this much hand holding into documentation, none of us
really knows sub modules well, so here's a rundown (developed by AI, even, not
my own knowledge...):

- A submodule is just a git commit SHA (see `git submodule status`). The
  submodule directories are independent git repos.
- `git submodule update --init` means "make my submodule checkouts match the
  SHAs this repo records". It puts each submodule on a **detached HEAD** at the
  pinned commit. This can very easily get confusing, but it's just how
  submodules operate.
- Pulling or switching branches in this repo updates *which SHA is recorded*
  but does **not** automatically change the sub-repos. You must run `git
  submodule update --init` afterward.
- To automate submodule updates (not recommended for DSpace devs!), run `git
  config submodule.recurse true`. With that set, `git pull` and `git checkout`
  keep the submodules in sync automatically, and you can mostly stop thinking
  about them.

### Which workflow are you?

How much submodule knowledge you need depends on which of these you are:

**Infra dev**: you work on this repo and rarely touch the sub-repos:

- Run `git config submodule.recurse true` once and let git keep the submodules
  synced to the pins for you.
- *Always* stage files by name in this repo (you should be doing this anyway).
  If somebody is mid-hack in a submodule (or you are), `git add -A` will
  happily commit a pin pointing at half-done work.

**DSpace dev**: you primarily work in `dspace-rest/` / `dspace-angular/`:

- Keep `submodule.recurse` unset (or explicitly false). If it's true, `git
  pull` will implicitly mess with your in-progress work.
- *First thing whenever you start work in a sub-repo*: switch to a working
  branch! (`git switch ...`). Submodules are detached heads, and commits will
  "vanish" if you don't explicitly switch.
- While you're mid-work, this repo's `git status` will tell you high-level info
  about the sub-repos (new commits, local modifications).
- If you need a sub-repo you *aren't* hacking brought up to date: `git
  submodule update --init dspace-angular`.
- If a sub-repo ever "jumps back" to the pin and your work seems gone: it
  isn't. Your branch is intact; `cd` in and `git switch <your-branch>` brings
  it all back.

**Production**: nothing gets edited here, ever. Deploy is pretty simple:

```bash
git pull
git submodule update --init --force
git submodule foreach 'git clean -fdx'

# This will take a long time
podman compose build

# Restart the stack, e.g., "systemctl --user restart scholarsbank"
```

### Reading `git status`

Image builds (e.g., `docker compose build`) copy files from *your local working
tree* of the two sub-repositories, so a submodule that's dirty or on the wrong
commit silently builds different images than everyone else gets. This can be a
nightmare to debug, so get in the habit of using `git status`: it will surface
subrepos' info as mentioned above.

A clean tree means your image builds will be consistent with other devs on the
same dspace-containers branch.

### Working on the REST or Angular code

The pinned commit is checked out detached, so **create a branch before
committing** or your work will be easy to lose:

```bash
cd dspace-rest
git switch -c feature/my-fix    # or "git switch main" or whatever
# hack, commit as usual...
git push -u origin feature/my-fix
```

When the work is ready, pin this repo to a submodule's code:

```bash
git add dspace-rest
git commit -m "Pin dspace-rest to <whatever>"
```

**Push the submodule before you push the pin commit.** The wrapper only stores
the SHA; if that SHA isn't on GitHub yet, everyone else's `git submodule
update` fails with a "fatal: remote error" about a missing commit.

### Moving a pin to the latest upstream

```bash
git submodule update --remote dspace-angular   # fetch + check out origin/main
git add dspace-angular
git commit -m "Pin dspace-angular to latest main"
```

**Do not use `local.cfg`**: this file is replaced with one that forces the
stack to behave a certain way.

If you're seeing odd issues when you build and run the stack, consider forcibly
resetting the state of those repos (e.g., `git -C dspace-rest clean -fd`,
`git submodule update --force`) and doing a full image rebuild.

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

One-off / temporary Caddy rules are dropped into the `caddy-conf` volume,
applied with a restart or reload of the `web` service. See
[docs/caddy-drop-ins.md](docs/caddy-drop-ins.md).

## Start it up!

Finally, start up the stack and browse to `http://localhost:8080`
