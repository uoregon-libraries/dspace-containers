// backup takes the nightly backups of a production install. It runs as root,
// from root's crontab, and has two jobs:
//
//	backup main -c <checkout> -u <podman user> -a <assetstore dir> -d <backup dir> [-k <days>]
//	backup solr -c <checkout> -u <podman user> -d <backup dir> [-k <days>]
//
// "main" backs up the database, assetstore, volume dirs, and config. "solr"
// backs up the Solr cores the database can't rebuild. "backup version" shows
// which commit the binary was built from. See docs/backups.md.
//
// A job backs up each part of its snapshot (e.g., the database, or one Solr
// core) on its own: one part failing doesn't stop the others. The snapshot is
// published if every required part (main's database and assetstore)
// succeeded, and so did at least one part. A failed part's dir is renamed
// "<part>.incomplete", and retention never deletes the newest good copy of a
// part, however old it is. Any failure still makes the job print its log and
// exit 1, so cron mails it; a job that succeeds prints nothing.
//
// Since root runs it, the binary refuses to run unless only root can replace
// it, which rules out a checkout's bin/ dir. Install it somewhere like
// /usr/local/sbin instead.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"syscall"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	// Ctrl-C or SIGTERM stops the current part, and the job stops without
	// publishing anything. A second one kills it outright.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop()
	}()

	switch os.Args[1] {
	case "main":
		os.Exit(cmdMain(ctx, os.Args[2:]))
	case "solr":
		os.Exit(cmdSolr(ctx, os.Args[2:]))
	case "version":
		fmt.Println(buildInfo())
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  backup main -c <checkout> -u <podman user> -a <assetstore dir> -d <backup dir> [-k <days>]
  backup solr -c <checkout> -u <podman user> -d <backup dir> [-k <days>]
  backup version

"main" backs up the database, assetstore, volume dirs, and config. "solr"
backs up the Solr cores the database can't rebuild. Both must run as root,
from a binary only root can replace.
Run "backup main -h" or "backup solr -h" for details on flags, and see
docs/backups.md for everything else.
`)
}

func cmdMain(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("backup main", flag.ContinueOnError)
	checkout := fs.String("c", "", "required: the checkout of the install to back up")
	user := fs.String("u", "", "required: the user that runs the stack (rootless podman)")
	assetstore := fs.String("a", "", "required: host path of the assetstore, i.e., what rest and cli mount")
	dest := fs.String("d", "", "required: backup destination; must already exist")
	keep := fs.Int("k", 14, "days of snapshots to keep")
	if !parseFlags(fs, args, keep, flagValue{"c", checkout}, flagValue{"u", user}, flagValue{"a", assetstore}, flagValue{"d", dest}) {
		return 2
	}

	// Resolved now, since setup changes to /
	asset, err := filepath.Abs(*assetstore)
	if err != nil {
		fmt.Fprintln(os.Stderr, "backup main:", err)
		return 1
	}

	j, err := setup("backup", *user, *checkout, *dest, "", *keep)
	if err != nil {
		fmt.Fprintln(os.Stderr, "backup main:", err)
		return 1
	}
	defer j.close()
	j.guard(func() { j.runMain(ctx, asset) })
	return j.finish()
}

func cmdSolr(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("backup solr", flag.ContinueOnError)
	checkout := fs.String("c", "", "required: the checkout of the install to back up")
	user := fs.String("u", "", "required: the user that runs the stack (rootless podman)")
	dest := fs.String("d", "", "required: backup destination, the same as main's; snapshots go in its solr/ subdir")
	keep := fs.Int("k", 14, "days of snapshots to keep")
	if !parseFlags(fs, args, keep, flagValue{"c", checkout}, flagValue{"u", user}, flagValue{"d", dest}) {
		return 2
	}

	j, err := setup("backup-solr", *user, *checkout, *dest, "solr", *keep)
	if err != nil {
		fmt.Fprintln(os.Stderr, "backup solr:", err)
		return 1
	}
	defer j.close()
	j.guard(func() { j.runSolr(ctx) })
	return j.finish()
}

type flagValue struct {
	name  string
	value *string
}

// parseFlags parses a job's flags and reports every problem with them at
// once. required lists the flags that must be non-empty.
func parseFlags(fs *flag.FlagSet, args []string, keep *int, required ...flagValue) bool {
	if err := fs.Parse(args); err != nil {
		return false
	}
	var problems []string
	if fs.NArg() > 0 {
		problems = append(problems, "unexpected arguments: "+strings.Join(fs.Args(), " "))
	}
	for _, f := range required {
		if *f.value == "" {
			problems = append(problems, "-"+f.name+" is required")
		}
	}
	if *keep < 0 {
		problems = append(problems, "-k can't be negative")
	}
	if len(problems) == 0 {
		return true
	}
	for _, p := range problems {
		fmt.Fprintf(os.Stderr, "%s: %s\n", fs.Name(), p)
	}
	fs.Usage()
	return false
}

// buildInfo describes the commit the binary was built from. The binary is
// built and installed separately from the checkout, so it can be older than
// the checkout it backs up; every run logs this to make that easy to spot.
func buildInfo() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown build"
	}
	settings := make(map[string]string)
	for _, s := range bi.Settings {
		settings[s.Key] = s.Value
	}
	rev := settings["vcs.revision"]
	if rev == "" {
		return "unknown commit (built without VCS info)"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	s := "commit " + rev
	if t := settings["vcs.time"]; t != "" {
		s += " (" + t + ")"
	}
	if settings["vcs.modified"] == "true" {
		s += ", plus uncommitted changes"
	}
	return s
}
