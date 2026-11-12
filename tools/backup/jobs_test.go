package main

import (
	"compress/gzip"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakePodman stands in for podman, as the podman user would run it. Its
// behavior comes from files in $FAKE_DIR, and it logs each call to
// $FAKE_DIR/calls.
const fakePodman = `#!/usr/bin/env bash
set -u
d=$FAKE_DIR
echo "$*" >>"$d/calls"
case $1 in
  ps)
    svc=${@: -1}
    svc=${svc##*=}
    [ -e "$d/down-$svc" ] || echo "fake_${svc}_1"
    ;;
  exec)
    shift 2
    case $1 in
      pg_dump)
        if [ -e "$d/dump-fail" ]; then
          echo "pg_dump: error: connection to server failed" >&2
          exit 1
        fi
        cat "$d/dump.sql"
        ;;
      curl)
        url=${@: -1}
        core=${url#http://localhost:8983/solr/}
        core=${core%%/*}
        n=$(cat "$d/count-$core" 2>/dev/null || echo 0)
        case $url in
          *command=backup*)
            echo $((n + 1)) >"$d/count-$core"
            touch "$d/pending-$core"
            echo '{"status":"OK"}'
            ;;
          *command=details*)
            # Each backup takes one extra check to finish
            if [ -e "$d/pending-$core" ]; then
              rm "$d/pending-$core"
              n=$((n - 1))
            fi
            if [ "$n" -eq 0 ]; then
              echo '{"status":"OK","details":{"indexSize":"1 KB"}}'
            elif [ -e "$d/fail-$core" ]; then
              echo '{"details":{"backup":{"exception":"/var/solr/data/backup/snapshot.'"$core"'"}}}'
            else
              echo '{"details":{"backup":{"startTime":"t'"$n"'","status":"success"}}}'
            fi
            ;;
        esac
        ;;
      tar)
        snap=$3
        tar -C "$d/solr-${snap##*snapshot.}" -c .
        ;;
      rm | mkdir) ;;
      *)
        echo "fake podman: unexpected command: $*" >&2
        exit 1
        ;;
    esac
    ;;
  *)
    echo "fake podman: unexpected command: $*" >&2
    exit 1
    ;;
esac
`

// testEnv is a fake install: checkout, volume dirs, assetstore, and backup
// dir, all under one temp dir, plus the fake podman's state dir
type testEnv struct {
	root, checkout, assetstore, dest, fake string
	podman                                 string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	root := t.TempDir()
	e := &testEnv{
		root:       root,
		checkout:   filepath.Join(root, "app"),
		assetstore: filepath.Join(root, "assetstore"),
		dest:       filepath.Join(root, "backups"),
		fake:       filepath.Join(root, "fake"),
	}
	e.podman = filepath.Join(e.fake, "podman")

	write(t, filepath.Join(e.checkout, "compose.yml"), "services: {}\n")
	write(t, filepath.Join(e.checkout, ".env"), strings.Join([]string{
		"COMPOSE_PROJECT_NAME=fake",
		"SAML_SP_KEY_FILE=./saml/sp.key",
		"SAML_SP_CERT_FILE=" + filepath.Join(root, "sp.crt") + " # absolute",
	}, "\n"))
	write(t, filepath.Join(e.checkout, "saml", "sp.key"), "private key")
	write(t, filepath.Join(root, "sp.crt"), "cert")

	write(t, filepath.Join(e.assetstore, "12", "34", "56", "123456"), "bitstream one")
	write(t, filepath.Join(e.assetstore, "65", "43", "21", "654321"), "bitstream two")

	write(t, filepath.Join(root, "volumes", "handle-server", "privkey.bin"), "handle key")
	write(t, filepath.Join(root, "volumes", "db", "PG_VERSION"), "15")
	write(t, filepath.Join(root, "volumes", "solr", "data"), "live solr")

	if err := os.MkdirAll(e.dest, 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, e.podman, fakePodman)
	if err := os.Chmod(e.podman, 0o700); err != nil {
		t.Fatal(err)
	}
	e.setDump(t, true)

	t.Setenv("FAKE_DIR", e.fake)
	return e
}

// setDump sets what the fake pg_dump prints: a complete dump, or one that's
// cut off
func (e *testEnv) setDump(t *testing.T, complete bool) {
	dump := "CREATE TABLE item (id int);\nCOPY item FROM stdin;\n1\n\\.\n"
	if complete {
		dump += "\n--\n-- PostgreSQL database dump complete\n--\n\n\\unrestrict abc123\n\n"
	}
	write(t, filepath.Join(e.fake, "dump.sql"), dump)
}

// job returns a job for a run at the given time, as setup would make it
func (e *testEnv) job(t *testing.T, kind, dest, when string) *job {
	t.Helper()
	env, err := os.ReadFile(filepath.Join(e.checkout, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	j := newJob(kind, e.checkout, env, dest, 14, testTime(t, when))
	j.logPath = filepath.Join(t.TempDir(), "run.log")
	j.podman = func(ctx context.Context, args ...string) *exec.Cmd {
		return j.command(ctx, e.podman, args...)
	}
	return j
}

func (e *testEnv) runMain(t *testing.T, when string) *job {
	t.Helper()
	j := e.job(t, "backup", e.dest, when)
	j.guard(func() { j.runMain(context.Background(), e.assetstore) })
	t.Logf("log for %s:\n%s", when, j.log.contents(j.logPath))
	return j
}

func TestMainJob(t *testing.T) {
	e := newTestEnv(t)

	// A good run
	j := e.runMain(t, "2026-11-01_050000")
	if j.failed {
		t.Fatal("first run failed")
	}
	snap1 := filepath.Join(e.dest, "2026-11-01_050000")
	for _, f := range []string{
		"db/dspace.sql.gz",
		"assetstore/12/34/56/123456",
		"assetstore/65/43/21/654321",
		"volumes/handle-server/privkey.bin",
		"config/.env",
		"config/sp.key",
		"config/sp.crt",
		"BACKUP_INFO.txt",
	} {
		mustExist(t, filepath.Join(snap1, f))
	}
	for _, f := range []string{"volumes/db", "volumes/solr"} {
		mustNotExist(t, filepath.Join(snap1, f))
	}
	if dump := gunzip(t, filepath.Join(snap1, "db/dspace.sql.gz")); !strings.Contains(dump, "CREATE TABLE item") {
		t.Errorf("dump has the wrong contents: %q", dump)
	}
	mustLink(t, e.dest, "2026-11-01_050000")

	// Config fails, which isn't required: the snapshot is kept, with config
	// marked incomplete
	os.Rename(filepath.Join(e.checkout, "saml", "sp.key"), filepath.Join(e.root, "sp.key.moved"))
	j = e.runMain(t, "2026-11-02_050000")
	if !j.failed {
		t.Fatal("second run should have failed")
	}
	snap2 := filepath.Join(e.dest, "2026-11-02_050000")
	mustExist(t, filepath.Join(snap2, "config.incomplete", "sp.crt"))
	mustNotExist(t, filepath.Join(snap2, "config"))
	mustExist(t, filepath.Join(snap2, "db", "dspace.sql.gz"))
	mustLink(t, e.dest, "2026-11-02_050000")
	info := read(t, filepath.Join(snap2, "BACKUP_INFO.txt"))
	for _, want := range []string{"config: FAILED", "sp.key", "db: ok", "assetstore: ok", "volumes: ok"} {
		if !strings.Contains(info, want) {
			t.Errorf("BACKUP_INFO.txt doesn't mention %q:\n%s", want, info)
		}
	}
	// Unchanged files are hard links to the previous snapshot's
	sameFile(t, filepath.Join(snap1, "assetstore/12/34/56/123456"), filepath.Join(snap2, "assetstore/12/34/56/123456"))
	sameFile(t, filepath.Join(snap1, "volumes/handle-server/privkey.bin"), filepath.Join(snap2, "volumes/handle-server/privkey.bin"))

	// Weeks later, config still fails. Retention keeps the last good config
	// (snap1), but removes snap2, which has nothing that's newest.
	write(t, filepath.Join(e.assetstore, "11", "11", "11", "111111"), "a new bitstream")
	j = e.runMain(t, "2026-11-20_050000")
	if !j.failed {
		t.Fatal("third run should have failed")
	}
	snap3 := filepath.Join(e.dest, "2026-11-20_050000")
	mustExist(t, filepath.Join(snap1, "config", "sp.key"))
	mustNotExist(t, snap2)
	mustExist(t, filepath.Join(snap3, "config.incomplete"))
	mustExist(t, filepath.Join(snap3, "assetstore/11/11/11/111111"))
	sameFile(t, filepath.Join(snap1, "assetstore/12/34/56/123456"), filepath.Join(snap3, "assetstore/12/34/56/123456"))
	mustNotExist(t, filepath.Join(e.dest, "2026-11-02_050000.deleting"))

	// Once config is back, the old snapshot can go
	os.Rename(filepath.Join(e.root, "sp.key.moved"), filepath.Join(e.checkout, "saml", "sp.key"))
	if j = e.runMain(t, "2026-11-21_050000"); j.failed {
		t.Fatal("fourth run failed")
	}
	mustNotExist(t, snap1)
	mustExist(t, snap3)
}

// TestMainJobRequiredParts makes sure a snapshot isn't published without the
// database or assetstore, even when everything else worked
func TestMainJobRequiredParts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause func(e *testEnv)
		fix   func(e *testEnv)
		log   []string
	}{
		{
			name:  "truncated dump",
			cause: func(e *testEnv) { e.setDump(t, false) },
			fix:   func(e *testEnv) { e.setDump(t, true) },
			log:   []string{"truncated", "without db;"},
		},
		{
			name:  "no db container",
			cause: func(e *testEnv) { write(t, filepath.Join(e.fake, "down-db"), "") },
			fix:   func(e *testEnv) { os.Remove(filepath.Join(e.fake, "down-db")) },
			log:   []string{"found: none", "without db;"},
		},
		{
			name: "empty assetstore",
			cause: func(e *testEnv) {
				os.Rename(e.assetstore, e.assetstore+".moved")
				os.MkdirAll(filepath.Join(e.assetstore, ".snapshot", "hourly.0"), 0o700)
			},
			fix: func(e *testEnv) {
				os.RemoveAll(e.assetstore)
				os.Rename(e.assetstore+".moved", e.assetstore)
			},
			log: []string{"is it mounted?", "without assetstore;"},
		},
		{
			name: "everything",
			cause: func(e *testEnv) {
				e.setDump(t, false)
				os.Rename(e.assetstore, e.assetstore+".moved")
				os.Rename(filepath.Join(e.root, "volumes"), filepath.Join(e.root, "volumes.moved"))
				os.Rename(filepath.Join(e.root, "sp.crt"), filepath.Join(e.root, "sp.crt.moved"))
			},
			fix: func(e *testEnv) {
				e.setDump(t, true)
				os.Rename(e.assetstore+".moved", e.assetstore)
				os.Rename(filepath.Join(e.root, "volumes.moved"), filepath.Join(e.root, "volumes"))
				os.Rename(filepath.Join(e.root, "sp.crt.moved"), filepath.Join(e.root, "sp.crt"))
			},
			log: []string{"without db and assetstore;"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)

			// A good run first, so there's a "latest" to leave alone
			if j := e.runMain(t, "2026-11-01_050000"); j.failed {
				t.Fatal("first run failed")
			}

			tc.cause(e)
			j := e.runMain(t, "2026-11-02_050000")
			if !j.failed {
				t.Fatal("run should have failed")
			}
			mustExist(t, filepath.Join(e.dest, "2026-11-02_050000.partial"))
			mustNotExist(t, filepath.Join(e.dest, "2026-11-02_050000"))
			mustLink(t, e.dest, "2026-11-01_050000")
			log := j.log.contents("")
			for _, want := range append(tc.log, "for inspection") {
				if !strings.Contains(log, want) {
					t.Errorf("log doesn't mention %q", want)
				}
			}

			// The next good run cleans up the leftovers
			tc.fix(e)
			if j := e.runMain(t, "2026-11-03_050000"); j.failed {
				t.Fatal("third run failed")
			}
			mustNotExist(t, filepath.Join(e.dest, "2026-11-02_050000.partial"))
			mustLink(t, e.dest, "2026-11-03_050000")
		})
	}
}

// TestMainJobLinksToLatest makes sure a snapshot this tool didn't make, like
// one from an older backup script, is still hard-linked against when
// "latest" points at it
func TestMainJobLinksToLatest(t *testing.T) {
	e := newTestEnv(t)
	old := filepath.Join(e.dest, "old-2026-10-01")
	os.Mkdir(old, 0o700)
	if out, err := exec.Command("rsync", "-a", e.assetstore+"/", filepath.Join(old, "assetstore")+"/").CombinedOutput(); err != nil {
		t.Fatalf("copying the assetstore: %s: %s", err, out)
	}
	if err := os.Symlink("old-2026-10-01", filepath.Join(e.dest, "latest")); err != nil {
		t.Fatal(err)
	}

	if j := e.runMain(t, "2026-11-01_050000"); j.failed {
		t.Fatal("run failed")
	}
	snap := filepath.Join(e.dest, "2026-11-01_050000")
	sameFile(t, filepath.Join(old, "assetstore/12/34/56/123456"), filepath.Join(snap, "assetstore/12/34/56/123456"))
	mustLink(t, e.dest, "2026-11-01_050000")
	// Retention leaves alone what it didn't make
	if j := e.runMain(t, "2026-12-01_050000"); j.failed {
		t.Fatal("second run failed")
	}
	mustExist(t, old)
}

func TestInterrupted(t *testing.T) {
	e := newTestEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	j := e.job(t, "backup", e.dest, "2026-11-01_050000")
	j.runMain(ctx, e.assetstore)
	if !j.failed {
		t.Fatal("run should have failed")
	}
	mustNotExist(t, filepath.Join(e.dest, "2026-11-01_050000"))
	mustNotExist(t, filepath.Join(e.dest, "latest"))
}

func TestSolrJob(t *testing.T) {
	solrPollInterval = 10 * time.Millisecond
	e := newTestEnv(t)
	for _, core := range solrCores {
		write(t, filepath.Join(e.fake, "solr-"+core, "segments_1"), core+" segments")
		write(t, filepath.Join(e.fake, "solr-"+core, "_0.cfs"), core+" data")
	}
	write(t, filepath.Join(e.fake, "fail-qaevent"), "")
	dest := filepath.Join(e.dest, "solr")
	os.Mkdir(dest, 0o700)

	run := func(when string) *job {
		j := e.job(t, "backup-solr", dest, when)
		j.guard(func() { j.runSolr(context.Background()) })
		t.Logf("log for %s:\n%s", when, j.log.contents(j.logPath))
		return j
	}

	// qaevent's backup fails, the others are kept
	if j := run("2026-11-01_013000"); !j.failed {
		t.Fatal("first run should have failed")
	}
	snap1 := filepath.Join(dest, "2026-11-01_013000")
	mustExist(t, filepath.Join(snap1, "statistics", "_0.cfs"))
	mustExist(t, filepath.Join(snap1, "suggestion", "segments_1"))
	mustNotExist(t, filepath.Join(snap1, "qaevent"))
	mustNotExist(t, filepath.Join(snap1, stagingDir))
	if info := read(t, filepath.Join(snap1, "BACKUP_INFO.txt")); !strings.Contains(info, "qaevent: FAILED") {
		t.Errorf("BACKUP_INFO.txt doesn't report qaevent's failure:\n%s", info)
	}

	// All good. Solr gives every snapshot new mtimes, but unchanged segment
	// files are still hard-linked.
	os.Remove(filepath.Join(e.fake, "fail-qaevent"))
	future := time.Now().Add(time.Hour)
	os.Chtimes(filepath.Join(e.fake, "solr-statistics", "_0.cfs"), future, future)
	if j := run("2026-11-02_013000"); j.failed {
		t.Fatal("second run failed")
	}
	snap2 := filepath.Join(dest, "2026-11-02_013000")
	mustExist(t, filepath.Join(snap2, "qaevent", "_0.cfs"))
	sameFile(t, filepath.Join(snap1, "statistics", "_0.cfs"), filepath.Join(snap2, "statistics", "_0.cfs"))
	mustLink(t, dest, "2026-11-02_013000")

	// Solr isn't running: nothing to do but fail
	write(t, filepath.Join(e.fake, "down-solr"), "")
	if j := run("2026-11-03_013000"); !j.failed {
		t.Fatal("third run should have failed")
	}
	mustNotExist(t, filepath.Join(dest, "2026-11-03_013000.partial"))
	mustLink(t, dest, "2026-11-02_013000")
}

func testTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.ParseInLocation(snapshotLayout, s, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func gunzip(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	r, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("want %s to exist: %s", path, err)
	}
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		t.Errorf("want %s not to exist", path)
	}
}

func mustLink(t *testing.T, dest, want string) {
	t.Helper()
	got, err := os.Readlink(filepath.Join(dest, "latest"))
	if err != nil || got != want {
		t.Errorf("latest -> %q (%v), want %q", got, err, want)
	}
}

func sameFile(t *testing.T, a, b string) {
	t.Helper()
	fa, errA := os.Stat(a)
	fb, errB := os.Stat(b)
	if errA != nil || errB != nil || !os.SameFile(fa, fb) {
		t.Errorf("want %s and %s to be hard links to one file (%v, %v)", a, b, errA, errB)
	}
}
