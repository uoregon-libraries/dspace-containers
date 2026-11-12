# Backups

## The backup tool

`backup` (Go, source in `tools/backup`) backs up a production install. It has
two jobs, each run nightly:

- `backup main`: the database, assetstore, volume dirs, and config
- `backup solr`: the Solr cores holding data the database can't rebuild
  (`statistics`, `qaevent`, and `suggestion`). The other cores get rebuilt by
  the index jobs.

Solr is separate for reasons I can no longer recall. It hadn't been backed up
in the past, so it's a new process and okay to keep isolated for now.

Both jobs must run as root, not the rendered crontab, allowing the backup dir
to be read-only (or inaccessible) to the podman user.

### Installing it

To build it, run `make bin/backup`. Like `render-crontab`, this needs Go, but
the result is a static binary, so you can build it elsewhere and copy it to
the server. Then install it where only root can write, e.g.:

```bash
sudo install -o root -g root -m 755 backup /usr/local/sbin/scholarsbank-backup
```

The tool refuses to run unless root owns it and every dir above it, and nobody
else can write to any of them. Because it isn't in the checkout, `-c` tells it
which repo to look at for backup info.

`backup version` shows the commit a binary was built from, and every run puts
that in its log and `BACKUP_INFO.txt`.

## Snapshots

Each run writes a snapshot dir named for its start time, `<date>_<time>/`, and
`latest` points at the newest one. Files unchanged since the previous snapshot
are hard links to avoid tons of disk usage.

Whatever `latest` points at when a run starts is also used for hard links,
even a dir the tool didn't make (e.g., a backup from another script), as long
as it has the same layout (`assetstore/`, `volumes/`, etc.). Only dirs named
`<date>_<time>` count as snapshots otherwise, so retention never deletes
anything else.

`backup main` writes snapshots to `<dest>/`:

- `db/dspace.sql.gz`: `pg_dump` of the database
- `assetstore/`: the assetstore
- `volumes/`: everything under `/path/to/scholarsbank/volumes` except `db`,
  `solr`, and `assetstore` (live files that can't be copied safely, or are
  copied above). This is what covers the handle server's keys.
- `config/`: `.env`, `compose.override.yml`, and the SAML key pair

`backup solr` writes snapshots to `<dest>/solr/`, one dir per core.

Every snapshot also has a `BACKUP_INFO.txt`: where and when it came from, and
how each part went.

## When something fails

Each dir in a snapshot (`db`, `assetstore`, a Solr core, etc.) is a *part*,
backed up on its own:

- A part that fails doesn't stop the others.
- `main`'s database and assetstore are *required*: if either fails, the
  backup is a failure, and the snapshot isn't published (see below). Config
  can be regenerated, and losing volumes would be annoying but survivable,
  so their failures still leave a published snapshot.
- Otherwise the snapshot is published, and becomes `latest`, as long as any
  part succeeded.
- In a published snapshot, a failed part's dir, if it got that far, is
  renamed `<part>.incomplete` (e.g., `config.incomplete/`). Don't restore
  from those!
- `BACKUP_INFO.txt` lists each part as ok, FAILED (with the reason), or
  skipped (e.g., there's no `volumes` dir).
- Any failure makes the job exit 1 and print its log, which cron mails. A job
  that succeeds prints nothing.
- If a required part failed, nothing succeeded, or the job was interrupted
  (Ctrl-C or SIGTERM), the run is left as `<date>_<time>.partial/`, `latest`
  stays put, and retention doesn't run. The next run deletes the partial dir.

So `latest` always has a good database and assetstore, but it can be missing
other parts: see "To restore" below.

## Retention

Snapshots older than `-k` days get deleted, **except the newest good copy of
each part**, however old. If the config copy fails for three weeks straight,
the last good one stays until a new one succeeds. (A failing database or
assetstore means no snapshot is published, so retention doesn't run at all.)

Every run's log stays in `<dest>/logs` (or `<dest>/solr/logs`) for a year, but
this may need to be revisited (these aren't DSpace logs, though, so shouldn't
be awful).

## Flags

- `-c` (required): the install's checkout, e.g., `/path/to/scholarsbank/app`
- `-u` (required): the podman user
- `-a` (`main` only, required): host path of the assetstore, i.e., what the
  override mounts into `rest` and `cli`
- `-d` (required): backup destination, which must already exist. Give both
  jobs the same one.
- `-k`: days of snapshots to keep (default 14)

## Crontab

Install crontabs as root, e.g., in `/etc/cron.d/scholarsbank-backup`. Run
Solr's after `stats-util` has pruned spiders, and the main backup after the
rest of the nightly DSpace jobs. Example:

```
MAILTO=dspace-admins@example.org
15 2 * * *	root	/usr/local/sbin/scholarsbank-backup solr -c /path/to/scholarsbank/app -u dspace -d /path/to/backups
0 5 * * *	root	/usr/local/sbin/scholarsbank-backup main -c /path/to/scholarsbank/app -u dspace -a /path/to/assetstore -d /path/to/backups
```

## Notes

- `main` won't copy an empty assetstore (a NetApp `.snapshot` dir doesn't
  count), to guard against a mount dropping: the empty copy would count as
  good, and retention would then delete the real ones. The assetstore is
  required, so this fails the whole backup.
- The containers a part needs (db or solr) must be running. If the stack is
  down, the job fails rather than starting it, and for `main` that means no
  snapshot, since the database is required.
- Solr writes each snapshot inside the solr volume before it's copied out, so
  that volume needs free space roughly equal to the `statistics` index. It's
  removed after each core, whether or not the core's backup worked.
- Config files are copied with rsync, not `cp -p`, which also copies ACLs and
  fails outright when the backup mount can't store them.
- A failed run's log is printed from memory, so it still gets mailed when the
  backup dir is full or broken.

## To restore

- Find the newest good copy of each part. The database and assetstore are
  always good in `latest`. Other parts usually are too, but if `latest` has a
  `<part>.incomplete`, look back: e.g., `ls -d <dest>/*/config` lists every
  good config copy. `BACKUP_INFO.txt` says how each part went.
- Database: copy `db/dspace.sql.gz` into a temporary `db` mount and follow "Get
  data" in the README.md's dev section (works fine in prod, it's just normally
  a dev-only concern). The copy has to be world-readable for the container to
  import it.
- Assetstore and volumes: `rsync -aH --numeric-ids` them back, with the stack
  stopped.
- Solr: copy the core's snapshot into the solr volume, then have Solr restore
  it, e.g., for statistics:

  ```bash
  tar -C <dest>/solr/<snapshot>/statistics -c . | podman exec -i <solr container> \
    sh -c 'mkdir -p /var/solr/data/restore/snapshot.statistics &&
      tar -C /var/solr/data/restore/snapshot.statistics -x'
  podman exec <solr container> curl -sS \
    'http://localhost:8983/solr/statistics/replication?command=restore&location=/var/solr/data/restore&name=statistics'
  ```

  Check progress with `command=restorestatus`, then delete
  `/var/solr/data/restore`.
