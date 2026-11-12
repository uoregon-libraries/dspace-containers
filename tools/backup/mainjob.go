package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// runMain backs up the database, assetstore, volume dirs, and config. A
// snapshot without the database and assetstore isn't worth publishing, but
// the rest can be regenerated or rebuilt if need be, so their failures only
// mark them incomplete.
func (j *job) runMain(ctx context.Context, assetstore string) {
	volumes := filepath.Join(filepath.Dir(j.checkout), "volumes")
	parts := []part{
		// The database goes first: everything it references is then sure to be
		// in the assetstore copy, and anything newer is just an orphan file
		{name: "db", required: true, backup: j.dumpDB},
		{name: "assetstore", required: true, backup: func(ctx context.Context) error {
			if err := checkAssetstore(assetstore); err != nil {
				return err
			}
			return j.snapshotDir(ctx, assetstore, "assetstore", "--exclude=.snapshot")
		}},
		{name: "volumes", backup: func(ctx context.Context) error {
			if !isDir(volumes) {
				return skipError{"no volume dirs at " + volumes}
			}
			// The database and Solr have their own backups, and copying their
			// live files would give us something that can't be trusted
			return j.snapshotDir(ctx, volumes, "volumes", "--exclude=/db", "--exclude=/solr", "--exclude=/assetstore")
		}},
		{name: "config", backup: j.copyConfig},
	}
	j.execute(ctx, parts, []string{"Assetstore: " + assetstore})
}

// checkAssetstore makes sure the assetstore isn't empty. An unmounted one
// would give us an empty copy that counts as good, and retention would then
// age out every real one.
func checkAssetstore(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("can't read the assetstore: %w", err)
	}
	for _, e := range entries {
		if e.Name() != ".snapshot" {
			return nil
		}
	}
	return fmt.Errorf("assetstore %s is empty; is it mounted?", dir)
}

// dumpCompleteMarker is the comment pg_dump ends a plain-text dump with (but
// for a "\unrestrict" line in newer versions), so a dump without it near the
// end got cut off
const dumpCompleteMarker = "PostgreSQL database dump complete"

// dumpDB writes a gzipped pg_dump of the database to db/dspace.sql.gz
func (j *job) dumpDB(ctx context.Context) error {
	db, err := j.containerFor(ctx, "db")
	if err != nil {
		return err
	}

	dir := filepath.Join(j.run, "db")
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "dspace.sql.gz")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	gz := gzip.NewWriter(f)
	tail := &tailBuffer{max: 4096}
	out := &firstError{w: io.MultiWriter(gz, tail)}
	cmd := j.podman(ctx, "exec", db, "pg_dump", "-U", "dspace", "--no-owner", "--no-privileges", "dspace")
	cmd.Stdout = out
	j.log.Printf("db: dumping the database in container %s", db)
	runErr := cmd.Run()

	// If writing failed, pg_dump died of a broken pipe, which says less than
	// the write error does
	if out.err != nil {
		return fmt.Errorf("writing %s: %w", path, out.err)
	}
	if runErr != nil {
		return fmt.Errorf("pg_dump: %w", runErr)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	// Surfaces errors that a network filesystem may otherwise only report
	// later, if at all
	if err := f.Sync(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if !bytes.Contains(tail.buf, []byte(dumpCompleteMarker)) {
		return errors.New("the database dump looks truncated: it doesn't end with pg_dump's completion comment")
	}

	if fi, err := os.Stat(path); err == nil {
		j.log.Printf("db: wrote %s (%d bytes)", path, fi.Size())
	}
	return nil
}

// copyConfig copies .env, compose.override.yml, and the SAML key pair. It
// copies every file it can before reporting any that failed.
func (j *job) copyConfig(ctx context.Context) error {
	dir := filepath.Join(j.run, "config")
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}

	files := []string{filepath.Join(j.checkout, ".env")}
	if override := filepath.Join(j.checkout, "compose.override.yml"); exists(override) {
		files = append(files, override)
	}
	for _, name := range []string{"SAML_SP_KEY_FILE", "SAML_SP_CERT_FILE"} {
		file := envSetting(j.env, name)
		if file == "" {
			continue
		}
		if !filepath.IsAbs(file) {
			file = filepath.Join(j.checkout, file)
		}
		files = append(files, file)
	}

	// rsync rather than a plain copy: cp -p also copies ACLs, and fails
	// outright when the backup mount can't store them. --copy-links because
	// a link to a key isn't a backup of it.
	var errs []error
	for _, file := range files {
		err := j.command(ctx, "rsync", "-a", "--copy-links", "--numeric-ids", file, dir+"/").Run()
		if err != nil {
			errs = append(errs, fmt.Errorf("copying %s: %w", file, err))
		}
	}
	return errors.Join(errs...)
}

// tailBuffer keeps the last max bytes written to it
type tailBuffer struct {
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > 2*t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

// firstError remembers the first error from w, so it can be reported instead
// of whatever it caused downstream
type firstError struct {
	w   io.Writer
	err error
}

func (f *firstError) Write(p []byte) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	n, err := f.w.Write(p)
	f.err = err
	return n, err
}
