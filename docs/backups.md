# Backups

## Scripts

Two jobs back up a production install, each nightly:

- `scripts/backup`: the database, assetstore, volume dirs, and config
- `scripts/backup-solr`: the Solr cores holding data the database can't
  rebuild (`statistics`, `qaevent`, and `suggestion`). The other cores get
  rebuilt by the index jobs.

Solr is separate for reasons I can no longer recall. It hadn't been backed up
in the past, so it's a new process and okay to keep isolated for now.

Both scripts must run as root, not the rendered crontab, allowing the backup
dir to be read-only (or inaccessible) to the podman user.

Each run writes a snapshot dir named for its start time, `<date>_<time>/`, and
`latest` points at the newest complete one. Files unchanged since the previous
snapshot are hard links to avoid tons of disk usage.

`scripts/backup` writes snapshots to `<dest>/`:

- `db/dspace.sql.gz`: `pg_dump` of the database
- `assetstore/`: the assetstore
- `volumes/`: everything under `/path/to/scholarsbank/volumes` except `db`,
  `solr`, and `assetstore` (live files that can't be copied safely, or are
  copied above). This is what covers the handle server's keys.
- `config/`: `.env`, `compose.override.yml`, and the SAML key pair

`scripts/backup-solr` writes snapshots to `<dest>/solr/`, one dir per core.

Both scripts share some flags:

- `-u` (required): the podman user
- `-a` (`scripts/backup` only, required): host path of the assetstore, i.e.,
  what the override mounts into `rest` and `cli`
- `-d` (required): backup destination, which must already exist. Give both
  jobs the same one.
- `-k`: days of snapshots to keep (default 14)

## Crontab

Install crontabs as root, e.g., in `/etc/cron.d/scholarsbank-backup`. Run
Solr's after `stats-util` has pruned spiders, and the main backup after the
rest of the nightly DSpace jobs. Example:

```
MAILTO=dspace-admins@example.org
30 1 * * *	root	/path/to/scholarsbank/app/scripts/backup-solr -u dspace -d /path/to/backups
0 5 * * *	root	/path/to/scholarsbank/app/scripts/backup -u dspace -a /path/to/assetstore -d /path/to/backups
```

## Notes

- Like `scripts/cron-job`, they print nothing on success. On failure they print
  their log, which cron emails. Every run's log stays in `<dest>/logs` (or
  `<dest>/solr/logs`) for a year, but this may need to be revisited (these
  aren't DSpace logs, though, so shouldn't be awful).
- A run that fails leaves a `.partial` dir and leaves `latest` alone. The next
  run deletes the partial dir.
- `scripts/backup` refuses to run if the assetstore is empty to guard against a
  mount dropping: snapshots would be empty and retention rules could delete
  real backups.
- The containers a job needs (db or solr) must be running. If the stack is
  down, the job fails rather than starting it.
- Solr writes each snapshot inside the solr volume before it's copied out, so
  that volume needs free space roughly equal to the `statistics` index.

## To restore

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
