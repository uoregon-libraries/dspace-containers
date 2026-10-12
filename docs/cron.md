# Cron / scheduled jobs

This doc is probably only for production. Dev definitely doesn't need cron
jobs, and staging *probably* doesn't.

High-level info:

- The host's cron runs jobs in a throwaway container (using the `cli` service)
- `conf/crontab` lists all jobs in a sort of "pseudo-crontab" format. It is
  *not usable* as-is.
- `bin/render-crontab` fills in `conf/crontab` and prints a usable crontab to
  stdout (suitable for dropping into `/etc/cron.d/`, for instance)

## Setup

1. Build the renderer: `make bin/render-crontab`. This needs Go, but the result
   is a static binary, so you can build it elsewhere and copy it to `bin/` on
   the server, or even just run it locally and only ship the output.
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

## How jobs run

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

## Modifying jobs

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
