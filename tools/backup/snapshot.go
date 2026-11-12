package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"strings"
	"time"
)

// part is one piece of a snapshot, backed up on its own: it succeeds or fails
// separately from the others, and gets its own dir in the snapshot
type part struct {
	name     string // its dir in the snapshot
	required bool   // the snapshot is only published if this part works
	backup   func(ctx context.Context) error
}

// skipError means a part had nothing to back up, which isn't a failure
type skipError struct{ reason string }

func (e skipError) Error() string { return e.reason }

type status int

const (
	statusOK status = iota
	statusFailed
	statusSkipped
)

type result struct {
	part     string
	required bool
	status   status
	detail   string // why it failed or was skipped
}

const (
	// incompleteSuffix marks a failed part's dir in a published snapshot
	incompleteSuffix = ".incomplete"

	// maxIncompleteLinks caps how many incomplete copies of a part get used
	// as --link-dest bases, on top of its newest good copy
	maxIncompleteLinks = 3
)

var snapshotName = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}_\d{6}$`)

// execute takes a snapshot: it backs up each part, publishes the snapshot if
// it can (see unpublishable), and applies retention. info is extra lines for
// BACKUP_INFO.txt.
func (j *job) execute(ctx context.Context, parts []part, info []string) {
	if err := j.begin(); err != nil {
		j.problem("can't start a snapshot: %s", err)
		return
	}

	results := j.backupParts(ctx, parts)
	if ctx.Err() != nil {
		j.problem("interrupted; leaving %s unpublished (the next run deletes it)", j.run)
		return
	}
	if why := unpublishable(results); why != "" {
		j.problem("%s; leaving %s for inspection (the next run deletes it)", why, j.run)
		return
	}

	if err := j.publish(results, info); err != nil {
		j.problem("can't publish the snapshot: %s", err)
		return
	}

	var names []string
	for _, p := range parts {
		names = append(names, p.name)
	}
	j.applyRetention(names)
}

// begin clears out whatever earlier runs left behind and creates this run's
// snapshot dir. We hold the lock, so no other run is using any of it.
func (j *job) begin() error {
	entries, err := os.ReadDir(j.dest)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !(strings.HasSuffix(name, ".partial") || strings.HasSuffix(name, ".deleting")) {
			continue
		}
		j.log.Printf("removing %s, left by an earlier run", name)
		if err := os.RemoveAll(filepath.Join(j.dest, name)); err != nil {
			// Wasted space, but no reason not to back up
			j.problem("can't remove %s: %s", name, err)
		}
	}

	if target, err := os.Readlink(filepath.Join(j.dest, "latest")); err == nil {
		if !filepath.IsAbs(target) {
			target = filepath.Join(j.dest, target)
		}
		if isDir(target) {
			j.prev = target
			j.log.Printf("previous snapshot: %s", target)
		}
	}

	j.run = filepath.Join(j.dest, j.name+".partial")
	return os.Mkdir(j.run, 0o700)
}

// unpublishable returns why a snapshot with these results can't be
// published, or "" if it can be: every required part has to have worked, and
// at least one part has to have backed something up
func unpublishable(results []result) string {
	var missing []string
	good := false
	for _, r := range results {
		switch {
		case r.status == statusOK:
			good = true
		case r.required:
			missing = append(missing, r.part)
		}
	}
	switch {
	case len(missing) > 0:
		return fmt.Sprintf("a snapshot can't be published without %s", strings.Join(missing, " and "))
	case !good:
		return "nothing was backed up"
	}
	return ""
}

// backupParts backs up each part, carrying on past failures, even a required
// part's, so the log covers everything that's wrong. Only an interruption
// stops it.
func (j *job) backupParts(ctx context.Context, parts []part) []result {
	var results []result
	for _, p := range parts {
		r := result{part: p.name, required: p.required}
		if ctx.Err() != nil {
			r.status, r.detail = statusFailed, "never started: interrupted"
			results = append(results, r)
			j.failed = true
			continue
		}

		j.log.Printf("%s: starting", p.name)
		err := j.backupPart(ctx, p)
		var skip skipError
		switch {
		case err == nil:
			r.status = statusOK
			j.log.Printf("%s: done", p.name)
		case errors.As(err, &skip):
			r.status, r.detail = statusSkipped, skip.reason
			j.log.Printf("%s: skipped: %s", p.name, skip.reason)
		default:
			r.status, r.detail = statusFailed, err.Error()
			j.failed = true
			j.log.Printf("%s: FAILED: %s", p.name, err)
		}
		results = append(results, r)
	}
	return results
}

// backupPart backs up one part, turning a panic into an error so a bug in
// one part can't take the others down with it
func (j *job) backupPart(ctx context.Context, p part) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
		}
	}()
	return p.backup(ctx)
}

// publish marks failed parts incomplete, writes BACKUP_INFO.txt, and moves
// the snapshot into place as the new "latest"
func (j *job) publish(results []result, info []string) error {
	for _, r := range results {
		dir := filepath.Join(j.run, r.part)
		if r.status != statusFailed || !exists(dir) {
			continue
		}
		if err := os.Rename(dir, dir+incompleteSuffix); err != nil {
			// A bad copy passing for a good one is worse than no snapshot
			return fmt.Errorf("can't mark %s incomplete: %w", r.part, err)
		}
		j.log.Printf("%s: left as %s%s", r.part, r.part, incompleteSuffix)
	}

	if err := j.writeInfo(results, info); err != nil {
		j.problem("can't write BACKUP_INFO.txt: %s", err)
	}

	final := filepath.Join(j.dest, j.name)
	if err := os.Rename(j.run, final); err != nil {
		return err
	}
	j.run = final

	// Relative, so the link survives the backup dir being mounted elsewhere
	tmp := filepath.Join(j.dest, ".latest.tmp")
	os.Remove(tmp)
	if err := os.Symlink(j.name, tmp); err != nil {
		return fmt.Errorf("published, but can't point latest at it: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(j.dest, "latest")); err != nil {
		return fmt.Errorf("published, but can't point latest at it: %w", err)
	}
	j.log.Printf("latest -> %s", j.name)
	return nil
}

func (j *job) writeInfo(results []result, info []string) error {
	var b strings.Builder
	host, _ := os.Hostname()
	prev := j.prev
	if prev == "" {
		prev = "none"
	}
	fmt.Fprintf(&b, "Started: %s\n", j.name)
	fmt.Fprintf(&b, "Finished: %s\n", time.Now().Format("2006-01-02 15:04:05 MST"))
	fmt.Fprintf(&b, "Host: %s\n", host)
	fmt.Fprintf(&b, "Checkout: %s\n", j.checkout)
	fmt.Fprintf(&b, "Compose project: %s\n", j.project)
	for _, line := range info {
		fmt.Fprintln(&b, line)
	}
	fmt.Fprintf(&b, "Previous snapshot: %s\n", prev)
	fmt.Fprintf(&b, "Backup tool: %s\n", buildInfo())
	fmt.Fprintf(&b, "\nParts:\n")
	for _, r := range results {
		detail := strings.ReplaceAll(r.detail, "\n", "; ")
		switch r.status {
		case statusOK:
			fmt.Fprintf(&b, "  %s: ok\n", r.part)
		case statusSkipped:
			fmt.Fprintf(&b, "  %s: skipped: %s\n", r.part, detail)
		case statusFailed:
			fmt.Fprintf(&b, "  %s: FAILED: %s\n", r.part, detail)
		}
	}
	if slices.ContainsFunc(results, func(r result) bool { return r.status == statusFailed }) {
		fmt.Fprintf(&b, "\nDetails are in the run's log, %s\n", j.logPath)
	}
	return os.WriteFile(filepath.Join(j.run, "BACKUP_INFO.txt"), []byte(b.String()), 0o600)
}

// applyRetention removes snapshots more than keepDays old, except the newest
// good copy of each part. A part that keeps failing can never age out its
// last good copy.
func (j *job) applyRetention(parts []string) {
	cutoff := j.started.AddDate(0, 0, -j.keepDays).Format("2006-01-02")
	j.log.Printf("removing snapshots from before %s", cutoff)
	remove, kept, err := planRetention(j.dest, parts, j.name, cutoff)
	if err != nil {
		j.problem("retention: %s", err)
		return
	}
	for _, snap := range slices.Sorted(maps.Keys(kept)) {
		j.log.Printf("keeping %s: it has the newest good copy of %s", snap, strings.Join(kept[snap], ", "))
	}
	for _, snap := range remove {
		j.log.Printf("removing %s", snap)
		// Renamed first, so a removal that dies halfway doesn't leave
		// something that passes for a snapshot
		path := filepath.Join(j.dest, snap)
		if err := os.Rename(path, path+".deleting"); err != nil {
			j.problem("can't remove %s: %s", snap, err)
			continue
		}
		if err := os.RemoveAll(path + ".deleting"); err != nil {
			j.problem("can't remove %s: %s", snap, err)
		}
	}
	j.pruneLogs()
}

// planRetention lists the snapshots in dest that retention should remove:
// those dated before cutoff (YYYY-MM-DD), except the current one and the
// newest good copy of each part. kept maps each old snapshot that stays
// anyway to the parts it's kept for.
func planRetention(dest string, parts []string, current, cutoff string) (remove []string, kept map[string][]string, err error) {
	snaps, err := listSnapshots(dest)
	if err != nil {
		return nil, nil, err
	}
	protect := make(map[string][]string)
	for _, p := range parts {
		if s := newestWith(dest, snaps, p); s != "" {
			protect[s] = append(protect[s], p)
		}
	}

	kept = make(map[string][]string)
	for _, s := range snaps {
		if s == current || s[:len("2006-01-02")] >= cutoff {
			continue
		}
		if why := protect[s]; len(why) > 0 {
			kept[s] = why
			continue
		}
		remove = append(remove, s)
	}
	return remove, kept, nil
}

// listSnapshots returns the names of dest's published snapshots, oldest
// first
func listSnapshots(dest string) ([]string, error) {
	entries, err := os.ReadDir(dest)
	if err != nil {
		return nil, err
	}
	var snaps []string
	for _, e := range entries {
		if e.IsDir() && snapshotName.MatchString(e.Name()) {
			snaps = append(snaps, e.Name())
		}
	}
	// ReadDir sorts by name, which for snapshots is chronological
	return snaps, nil
}

// newestWith returns the newest of snaps with a good copy of part, or ""
func newestWith(dest string, snaps []string, part string) string {
	for i := len(snaps) - 1; i >= 0; i-- {
		if isDir(filepath.Join(dest, snaps[i], part)) {
			return snaps[i]
		}
	}
	return ""
}

// linkDests returns the dirs rsync should hard-link unchanged files from when
// copying part: its newest good copy, and any incomplete copies newer than
// that, so what a failed run did copy isn't copied again
func (j *job) linkDests(part string) []string {
	snaps, err := listSnapshots(j.dest)
	if err != nil {
		j.log.Printf("%s: can't look for earlier snapshots, so copying everything: %s", part, err)
		return nil
	}
	var dirs []string
	incomplete := 0
	for i := len(snaps) - 1; i >= 0; i-- {
		dir := filepath.Join(j.dest, snaps[i], part)
		if isDir(dir) {
			return append(dirs, dir)
		}
		if incomplete < maxIncompleteLinks && isDir(dir+incompleteSuffix) {
			dirs = append(dirs, dir+incompleteSuffix)
			incomplete++
		}
	}
	return dirs
}

// snapshotDir copies a directory into the snapshot as sub, hard-linking
// whatever is unchanged since earlier snapshots. Extra args go to rsync.
func (j *job) snapshotDir(ctx context.Context, src, sub string, extra ...string) error {
	args := append([]string{"-aH", "--numeric-ids", "--stats"}, extra...)
	for _, dir := range j.linkDests(sub) {
		args = append(args, "--link-dest="+dir)
	}
	dst := filepath.Join(j.run, sub)
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	args = append(args, src+"/", dst+"/")
	j.log.Printf("$ rsync %s", strings.Join(args, " "))

	err := j.command(ctx, "rsync", args...).Run()
	// 24 means files vanished mid-copy, which is expected of a live system
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 24 {
		j.log.Printf("%s: some files vanished during the copy, which is fine", sub)
		return nil
	}
	if err != nil {
		return fmt.Errorf("rsync: %w", err)
	}
	return nil
}

// pruneLogs deletes run logs more than a year old
func (j *job) pruneLogs() {
	dir := filepath.Join(j.dest, "logs")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		j.problem("can't prune old logs: %s", err)
		return
	}
	cutoff := j.started.AddDate(-1, 0, 0)
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		fi, err := e.Info()
		if err == nil && fi.ModTime().Before(cutoff) {
			err = os.Remove(filepath.Join(dir, e.Name()))
		}
		if err != nil {
			j.problem("can't prune old log %s: %s", e.Name(), err)
		}
	}
}

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
