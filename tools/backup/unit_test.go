package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestEnvSetting(t *testing.T) {
	var env = strings.Join([]string{
		"# COMMENTED=no",
		"PLAIN=value",
		"  SPACED  =  spaced value  ",
		`QUOTED="quoted"`,
		"SINGLE='single'",
		"COMMENT=value # a comment",
		"HASH=val#ue",
		"TWICE=first",
		"TWICE=second",
		"CRLF=crlf\r",
		"PREFIX_PLAIN=wrong",
		"EMPTY=",
	}, "\n")

	var tests = map[string]string{
		"PLAIN":     "value",
		"SPACED":    "spacedvalue",
		"QUOTED":    "quoted",
		"SINGLE":    "single",
		"COMMENT":   "value",
		"HASH":      "val#ue",
		"TWICE":     "second",
		"CRLF":      "crlf",
		"COMMENTED": "",
		"MISSING":   "",
		"EMPTY":     "",
		"PLAI":      "",
	}
	for name, want := range tests {
		if got := envSetting([]byte(env), name); got != want {
			t.Errorf("envSetting(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestLockSafe(t *testing.T) {
	if got, want := lockSafe("/srv/my app/ok.-_"), "_srv_my_app_ok.-_"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestHeadTail(t *testing.T) {
	var h headTail
	h.write([]byte("short"))
	if got := h.String("x.log"); got != "short" {
		t.Errorf("short log: got %q", got)
	}

	// Enough to overflow both halves, written in odd-sized pieces
	h = headTail{}
	var all strings.Builder
	for i := 0; all.Len() < 3*(memHead+memTail); i++ {
		line := strings.Repeat(string(rune('a'+i%26)), 1+i%977) + "\n"
		all.WriteString(line)
		h.write([]byte(line))
	}
	full := all.String()
	got := h.String("x.log")
	head, tail, found := strings.Cut(got, "\n[... ")
	if !found {
		t.Fatal("long log has no note about what's left out")
	}
	_, tail, _ = strings.Cut(tail, "...]\n")
	if head != full[:memHead] {
		t.Error("head doesn't match the start of the log")
	}
	if tail != full[len(full)-memTail:] {
		t.Error("tail doesn't match the end of the log")
	}
	if !strings.Contains(got, "see x.log") {
		t.Error("note doesn't point at the log file")
	}
}

func TestParseBackupDetails(t *testing.T) {
	details, err := parseBackupDetails([]byte(`{"status":"OK","details":{"indexSize":"1 KB"}}`))
	if err != nil || details != nil {
		t.Errorf("no backup yet: got %v, %v", details, err)
	}

	details, err = parseBackupDetails([]byte(`{"details":{"backup":{"startTime":"2026-11-11T17:30:24Z","status":"success"}}}`))
	if err != nil || details["status"] != "success" {
		t.Errorf("backup: got %v, %v", details, err)
	}

	if _, err := parseBackupDetails([]byte(`{"error":{"msg":"nope"}}`)); err == nil {
		t.Error("no details: want an error")
	}
	if _, err := parseBackupDetails([]byte(`<html>`)); err == nil {
		t.Error("not JSON: want an error")
	}
}

func TestBackupFinished(t *testing.T) {
	old := map[string]any{"startTime": "t1", "status": "success"}
	var tests = map[string]struct {
		before, now map[string]any
		done        bool
		wantErr     bool
	}{
		"first ever, still going": {before: nil, now: nil},
		"still going":             {before: old, now: map[string]any{"startTime": "t1", "status": "success"}},
		"first ever, done":        {before: nil, now: old, done: true},
		"done":                    {before: old, now: map[string]any{"startTime": "t2", "status": "success"}, done: true},
		"failed":                  {before: old, now: map[string]any{"exception": "/var/solr/data/backup/snapshot.x"}, done: true, wantErr: true},
		"solr restarted":          {before: old, now: nil, done: true, wantErr: true},
	}
	for name, tc := range tests {
		done, err := backupFinished(tc.before, tc.now)
		if done != tc.done || (err != nil) != tc.wantErr {
			t.Errorf("%s: got (%v, %v), want done=%v, error=%v", name, done, err, tc.done, tc.wantErr)
		}
	}
}

// mkSnapshots creates snapshot dirs in dest, each with the given entries
func mkSnapshots(t *testing.T, dest string, snaps map[string][]string) {
	t.Helper()
	for snap, entries := range snaps {
		for _, e := range entries {
			if err := os.MkdirAll(filepath.Join(dest, snap, e), 0o700); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestPlanRetention(t *testing.T) {
	dest := t.TempDir()
	mkSnapshots(t, dest, map[string][]string{
		"2026-10-01_050000": {"db", "files"},
		"2026-10-02_050000": {"db", "files"},
		"2026-10-03_050000": {"db.incomplete", "files"},
		"2026-10-04_050000": {"db.incomplete", "files"},
		"2026-10-20_050000": {"db.incomplete", "files"},
		"2026-10-21_050000": {"db.incomplete", "files.incomplete"},
	})
	// Things that aren't snapshots
	for _, d := range []string{"logs", "solr", "2026-10-01_050000.partial", "2026-09-01"} {
		os.MkdirAll(filepath.Join(dest, d), 0o700)
	}
	os.Symlink("2026-10-21_050000", filepath.Join(dest, "latest"))

	remove, kept, err := planRetention(dest, []string{"db", "files"}, "2026-10-21_050000", "2026-10-07")
	if err != nil {
		t.Fatal(err)
	}
	// 10-02 has the newest good db; 10-20, the newest good files, is too new
	// to remove anyway
	if want := []string{"2026-10-01_050000", "2026-10-03_050000", "2026-10-04_050000"}; !slices.Equal(remove, want) {
		t.Errorf("remove = %v, want %v", remove, want)
	}
	if len(kept) != 1 || !slices.Equal(kept["2026-10-02_050000"], []string{"db"}) {
		t.Errorf("kept = %v", kept)
	}

	// The current snapshot is never removed, even when it's "old"
	remove, _, _ = planRetention(dest, []string{"db", "files"}, "2026-10-21_050000", "2026-12-01")
	if slices.Contains(remove, "2026-10-21_050000") {
		t.Errorf("current snapshot would be removed: %v", remove)
	}
}

func TestLinkDests(t *testing.T) {
	dest := t.TempDir()
	mkSnapshots(t, dest, map[string][]string{
		"2026-10-01_050000": {"files"},
		"2026-10-02_050000": {"files"},
		"2026-10-03_050000": {"files.incomplete"},
		"2026-10-04_050000": {"other"},
		"2026-10-05_050000": {"files.incomplete"},
	})
	j := newJob("backup", "/x/app", nil, dest, 14, testTime(t, "2026-10-06_050000"))
	var want = []string{
		filepath.Join(dest, "2026-10-05_050000", "files.incomplete"),
		filepath.Join(dest, "2026-10-03_050000", "files.incomplete"),
		filepath.Join(dest, "2026-10-02_050000", "files"),
	}
	if got := j.linkDests("files"); !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := j.linkDests("nothing"); len(got) != 0 {
		t.Errorf("part with no copies: got %v", got)
	}

	// The cap on incomplete copies doesn't stop the search for a good one
	for _, s := range []string{"2026-10-06_050000", "2026-10-07_050000", "2026-10-08_050000"} {
		mkSnapshots(t, dest, map[string][]string{s: {"files.incomplete"}})
	}
	got := j.linkDests("files")
	if len(got) != maxIncompleteLinks+1 || got[len(got)-1] != filepath.Join(dest, "2026-10-02_050000", "files") {
		t.Errorf("with many incomplete copies: got %v", got)
	}
}

func TestRootOnly(t *testing.T) {
	if err := rootOnly("/"); err != nil {
		t.Errorf("rootOnly(/): %v", err)
	}

	dir := t.TempDir()
	exe := filepath.Join(dir, "backup")
	write(t, exe, "#!/bin/sh\n")
	if os.Getuid() != 0 {
		os.Chmod(exe, 0o755)
		if err := rootOnly(exe); err == nil || !strings.Contains(err.Error(), "not root") {
			t.Errorf("rootOnly on a file owned by uid %d: got %v", os.Getuid(), err)
		}
	}
	os.Chmod(exe, 0o777)
	if err := rootOnly(exe); err == nil {
		t.Errorf("rootOnly on a world-writable file: no error")
	}
}

func TestCheckoutDir(t *testing.T) {
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}

	write(t, filepath.Join(dir, "compose.yml"), "services: {}\n")
	if _, err := checkoutDir(link); err == nil || !strings.Contains(err.Error(), ".env") {
		t.Errorf("checkoutDir without .env: got %v", err)
	}
	write(t, filepath.Join(dir, ".env"), "")
	got, err := checkoutDir(link)
	if err != nil || got != real {
		t.Errorf("checkoutDir(%s) = %q, %v; want %q", link, got, err, real)
	}
	if _, err := checkoutDir(filepath.Join(dir, ".env")); err == nil {
		t.Errorf("checkoutDir on a file: no error")
	}
}

func TestUnpublishable(t *testing.T) {
	ok := func(name string, required bool) result {
		return result{part: name, required: required, status: statusOK}
	}
	failed := func(name string, required bool) result {
		return result{part: name, required: required, status: statusFailed}
	}
	skipped := func(name string) result { return result{part: name, status: statusSkipped} }

	for _, tc := range []struct {
		name    string
		results []result
		want    string // a substring of the reason, or "" if it can be published
	}{
		{"all good", []result{ok("db", true), ok("assetstore", true), ok("config", false)}, ""},
		{"optional parts fail", []result{ok("db", true), ok("assetstore", true), failed("config", false), skipped("volumes")}, ""},
		{"db fails", []result{failed("db", true), ok("assetstore", true), ok("config", false)}, "without db"},
		{"both fail", []result{failed("db", true), failed("assetstore", true), ok("config", false)}, "without db and assetstore"},
		{"no required parts, one good", []result{failed("qaevent", false), ok("statistics", false)}, ""},
		{"no required parts, none good", []result{failed("qaevent", false), failed("statistics", false)}, "nothing was backed up"},
		{"only skips", []result{skipped("volumes")}, "nothing was backed up"},
	} {
		got := unpublishable(tc.results)
		if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}
