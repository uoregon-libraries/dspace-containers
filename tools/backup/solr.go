package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// solrCores are the cores holding data that isn't in the database. The others
// (search, oai, authority) don't need backups: the index jobs in conf/crontab
// rebuild them from the database.
var solrCores = []string{"statistics", "qaevent", "suggestion"}

// solrBackupDir is where Solr writes snapshots, inside its container. It's in
// the solr volume because Solr only writes backups under its home dir.
const solrBackupDir = "/var/solr/data/backup"

// stagingDir holds Solr snapshots inside our snapshot while they're copied
// in. It's never published.
const stagingDir = ".staging"

// How long and how often to check on a Solr backup; tests shorten them
var (
	solrBackupTimeout = 2 * time.Hour
	solrPollInterval  = 10 * time.Second
)

// runSolr backs up each core in solrCores
func (j *job) runSolr(ctx context.Context) {
	// Every part needs the container, so there's no point starting without it
	solr, err := j.containerFor(ctx, "solr")
	if err != nil {
		j.problem("%s", err)
		return
	}

	var parts []part
	for _, core := range solrCores {
		parts = append(parts, part{name: core, backup: func(ctx context.Context) error {
			return j.snapshotCore(ctx, solr, core)
		}})
	}
	j.execute(ctx, parts, []string{"Cores: " + strings.Join(solrCores, " ")})
}

// snapshotCore has Solr snapshot one core inside its container, then copies
// the snapshot out
func (j *job) snapshotCore(ctx context.Context, solr, core string) error {
	before, err := j.backupDetails(ctx, solr, core)
	if err != nil {
		return err
	}

	snap := solrBackupDir + "/snapshot." + core
	if err := j.inSolr(ctx, solr, "rm", "-rf", snap); err != nil {
		return err
	}
	if err := j.inSolr(ctx, solr, "mkdir", "-p", solrBackupDir); err != nil {
		return err
	}
	// Always clean up: the snapshot is as big as the core, in a volume we
	// don't want to fill. Not with ctx, which may be why we're stopping.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := j.inSolr(cleanup, solr, "rm", "-rf", snap); err != nil {
			j.problem("%s: can't remove %s from the solr container: %s", core, snap, err)
		}
	}()

	j.log.Printf("%s: asking solr for a backup", core)
	_, err = j.solrAPI(ctx, solr, core+"/replication?command=backup&location="+solrBackupDir+"&name="+core)
	if err != nil {
		return err
	}
	if err := j.waitForBackup(ctx, solr, core, before); err != nil {
		return err
	}

	// Extracted to a staging dir first, so snapshotDir can hard-link segment
	// files that are unchanged since the previous snapshot. Solr gives every
	// snapshot fresh mtimes, so compare by size and ignore times: Lucene never
	// rewrites a file, so a name and size match is the same file.
	j.log.Printf("%s: copying solr's snapshot out of the container", core)
	staging := filepath.Join(j.run, stagingDir, core)
	defer func() {
		if err := os.RemoveAll(filepath.Join(j.run, stagingDir)); err != nil {
			j.problem("%s: can't remove %s: %s", core, staging, err)
		}
	}()
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return err
	}
	if err := j.copyOut(ctx, solr, snap, staging); err != nil {
		return err
	}
	return j.snapshotDir(ctx, staging, core, "--size-only", "--no-times")
}

// waitForBackup polls a core's backup details until Solr reports on the
// backup we asked for. Solr starts a backup and returns right away.
func (j *job) waitForBackup(ctx context.Context, solr, core string, before map[string]any) error {
	deadline := time.Now().Add(solrBackupTimeout)
	var errs int
	var last map[string]any
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(solrPollInterval):
		}

		now, err := j.backupDetails(ctx, solr, core)
		if err != nil {
			errs++
			j.log.Printf("%s: checking on the backup: %s", core, err)
			if errs >= 5 {
				return fmt.Errorf("giving up after %d failed checks on solr's backup: %w", errs, err)
			}
		} else {
			errs = 0
			last = now
			if done, err := backupFinished(before, now); done {
				return err
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("solr's backup didn't finish within %s; its last details: %s", solrBackupTimeout, describe(last))
		}
	}
}

// backupFinished compares a core's backup details from before we asked for a
// backup with its details now. Solr replaces them only when a backup ends:
// with a status of "success", or with just an "exception" on failure.
func backupFinished(before, now map[string]any) (bool, error) {
	if reflect.DeepEqual(before, now) {
		return false, nil
	}
	if now["status"] == "success" {
		return true, nil
	}
	return true, fmt.Errorf("solr's backup failed: %s", describe(now))
}

func describe(details map[string]any) string {
	b, err := json.Marshal(details)
	if err != nil {
		return fmt.Sprint(details)
	}
	return string(b)
}

// backupDetails returns the "backup" section of a core's replication
// details: Solr's report on the core's latest backup, or nil if there hasn't
// been one since Solr started
func (j *job) backupDetails(ctx context.Context, solr, core string) (map[string]any, error) {
	// json.nl=map keeps Solr from turning some objects into flat arrays
	body, err := j.solrAPI(ctx, solr, core+"/replication?command=details&wt=json&json.nl=map")
	if err != nil {
		return nil, err
	}
	return parseBackupDetails(body)
}

func parseBackupDetails(body []byte) (map[string]any, error) {
	var resp struct {
		Details *struct {
			Backup map[string]any `json:"backup"`
		} `json:"details"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("can't parse solr's replication details: %w", err)
	}
	if resp.Details == nil {
		return nil, fmt.Errorf("solr's replication details have no details: %s", truncate(body, 500))
	}
	return resp.Details.Backup, nil
}

// solrAPI calls Solr's API from inside its container, and returns the
// response body
func (j *job) solrAPI(ctx context.Context, solr, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := j.podman(ctx, "exec", solr, "curl", "-sS", "--fail-with-body", "--max-time", "60",
		"http://localhost:8983/solr/"+path)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("solr API call %s: %w: %s", path, err, truncate(out.Bytes(), 500))
	}
	return out.Bytes(), nil
}

// inSolr runs a quick command in the solr container
func (j *job) inSolr(ctx context.Context, solr string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := j.podman(ctx, append([]string{"exec", solr}, args...)...).Run(); err != nil {
		return fmt.Errorf("%s in the solr container: %w", strings.Join(args, " "), err)
	}
	return nil
}

// copyOut copies a dir out of the solr container through a tar pipe. tar
// extracts as root, since the podman user can't write to the backup dir.
func (j *job) copyOut(ctx context.Context, solr, src, dst string) error {
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	create := j.podman(ctx, "exec", solr, "tar", "-C", src, "-c", ".")
	create.Stdout = pw
	extract := j.command(ctx, "tar", "-C", dst, "-x", "--numeric-owner")
	extract.Stdin = pr

	if err := extract.Start(); err != nil {
		pr.Close()
		pw.Close()
		return err
	}
	pr.Close()
	err = create.Start()
	pw.Close()
	if err != nil {
		extract.Wait()
		return err
	}

	createErr, extractErr := create.Wait(), extract.Wait()
	if createErr != nil {
		return fmt.Errorf("tar in the solr container: %w", createErr)
	}
	if extractErr != nil {
		return fmt.Errorf("extracting solr's snapshot: %w", extractErr)
	}
	return nil
}

func truncate(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
