// render-crontab turns conf/crontab into a real crontab: conf/crontab is a
// stub of sorts, and this script's job is to fill in the path to the project,
// MAILTO, user to run cron as, full path to the cron running script, etc.
//
// It reads conf/crontab from the current directory, but everything it fills
// in comes from flags, never the local machine, so a crontab can be rendered
// on a workstation and shipped to a server with different users and paths.
//
// It also rejects mistakes cron would otherwise act on silently: malformed
// schedules, and commands with syntax the *host's* shell would handle (";",
// pipes, redirects, "$", etc.) instead of passing it along to the container.
//
// Usage:
//
//     render-crontab -dir <project dir> -user <user> -mailto <address> [-compose-dir <dir>]
//
// -dir, -user, and -mailto are required, but -user and -mailto may be
// explicitly empty: -user "" renders a user crontab, -mailto "" omits MAILTO.

package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

// options holds everything render needs besides the schedule itself
type options struct {
	projectDir string // absolute path of the checkout on the target machine
	user       string // run-as user for a system crontab; empty for a user crontab
	path       string // PATH for the jobs
	mailto     string // may be empty
}

// systemPath is where cron looks for podman-compose unless -compose-dir says
// otherwise
const systemPath = "/usr/local/bin:/usr/bin:/bin"

func main() {
	dir := flag.String("dir", "", "required: absolute path of the project checkout on the machine that will run the jobs")
	user := flag.String("user", "", "required: render a system crontab (e.g., for /etc/cron.d) whose jobs run as this user;\nuse -user \"\" to render a user crontab (e.g., for \"crontab -e\")")
	mailto := flag.String("mailto", "", "required: where cron mails failed jobs; use -mailto \"\" to leave MAILTO out\n(cron then mails the local account running the jobs)")
	composeDir := flag.String("compose-dir", "", "absolute path of the directory holding podman-compose on the machine that will run the jobs,\nif it isn't in "+systemPath)
	flag.Parse()
	if flag.NArg() > 0 {
		flag.Usage()
		os.Exit(2)
	}

	var opts options
	var problems []string
	complain := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	// "Required" means given at all, even if empty, so nobody gets a user
	// crontab or a MAILTO-less one by accident
	given := make(map[string]bool)
	flag.Visit(func(f *flag.Flag) { given[f.Name] = true })
	for _, name := range []string{"dir", "user", "mailto"} {
		if !given[name] {
			complain("-%s is required", name)
		}
	}

	// The project dir is on another machine, so it can't be checked beyond its
	// shape. It ends up unquoted in a shell command line, so keep it boring.
	opts.projectDir = *dir
	if given["dir"] {
		checkDir(complain, "-dir", opts.projectDir)
	}

	opts.user = *user
	if opts.user != "" && !safeUser.MatchString(opts.user) {
		complain("%q doesn't look like a user name", opts.user)
	}

	// A newline here would let MAILTO inject lines into the crontab
	opts.mailto = *mailto
	if strings.ContainsFunc(opts.mailto, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		complain("-mailto %q may not contain spaces or control characters", opts.mailto)
	}

	opts.path = systemPath
	if *composeDir != "" {
		checkDir(complain, "-compose-dir", *composeDir)
		opts.path = *composeDir + ":" + systemPath
	}

	// Check the schedule even if something above failed, so one run reports
	// everything that needs fixing
	var out string
	schedulePath := filepath.Join("conf", "crontab")
	schedule, err := os.ReadFile(schedulePath)
	if err != nil {
		complain("%s (run this from the project directory)", err)
	} else {
		var errs []error
		out, errs = render(schedule, opts)
		for _, e := range errs {
			complain("%s:%s", schedulePath, e)
		}
	}

	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, "render-crontab:", p)
		}
		os.Exit(1)
	}

	fmt.Print(out)
}

var (
	safePath = regexp.MustCompile(`^[A-Za-z0-9/._-]+$`)
	safeUser = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9._-]*$`)
	envVar   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\s*=`)
)

// checkDir complains unless dir is a clean absolute path that's safe to put
// in a crontab unquoted. It may name a directory on another machine, so it's
// never looked up.
func checkDir(complain func(string, ...any), flagName, dir string) {
	switch {
	case !safePath.MatchString(dir):
		complain("%s %q may only contain letters, digits, and /._-", flagName, dir)
	case !path.IsAbs(dir):
		complain("%s %q must be an absolute path", flagName, dir)
	case path.Clean(dir) != dir:
		complain("%s %q should be written as %q", flagName, dir, path.Clean(dir))
	}
}

// render builds the crontab from the schedule file's contents. Problems are
// returned as "<line number>: <message>" errors, and the output is only
// meaningful if there are none.
//
// Line by line:
//
//   - The schedule's opening comment block (and the blank lines after it) is
//     dropped: it's about editing that file, and would be misleading here
//   - Other comments, blank lines, and variable assignments pass through as-is
//   - Everything else is a job: a schedule followed by a cli command, which
//     becomes "<schedule> [<user>] <project>/scripts/cron-job <command>"
func render(schedule []byte, opts options) (string, []error) {
	var out bytes.Buffer
	var errs []error
	marker := "DSpace jobs for " + opts.projectDir

	fmt.Fprintf(&out, "# BEGIN %s\n", marker)
	fmt.Fprintf(&out, "# Generated by render-crontab from %s/conf/crontab.\n", opts.projectDir)
	fmt.Fprintf(&out, "# Don't edit this block: change that file and render it again.\n")
	if opts.user != "" {
		fmt.Fprintf(&out, "# Format: system crontab (/etc/cron.d); jobs run as %q\n", opts.user)
	} else {
		fmt.Fprintf(&out, "# Format: user crontab; jobs run as the crontab's owner\n")
	}
	fmt.Fprintf(&out, "\nPATH=%s\n", opts.path)
	if opts.mailto != "" {
		fmt.Fprintf(&out, "MAILTO=%s\n", opts.mailto)
	}
	out.WriteString("\n")

	runner := path.Join(opts.projectDir, "scripts", "cron-job")
	// The header is the leading comment block plus the blank lines after it;
	// the first comment after that gap belongs to the schedule
	inHeader, pastHeaderComments := true, false
	scanner := bufio.NewScanner(bytes.NewReader(schedule))
	for n := 1; scanner.Scan(); n++ {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		isComment := strings.HasPrefix(trimmed, "#")

		if inHeader {
			switch {
			case trimmed == "":
				pastHeaderComments = true
				continue
			case isComment && !pastHeaderComments:
				continue
			}
			inHeader = false
		}

		switch {
		case trimmed == "":
			out.WriteString("\n")
		case isComment, envVar.MatchString(trimmed):
			out.WriteString(line)
			out.WriteByte('\n')
		default:
			sched, cmd, err := parseJob(trimmed)
			if err != nil {
				errs = append(errs, fmt.Errorf("%d: %w", n, err))
				continue
			}
			if opts.user != "" {
				fmt.Fprintf(&out, "%s\t%s\t%s %s\n", sched, opts.user, runner, cmd)
			} else {
				fmt.Fprintf(&out, "%s\t%s %s\n", sched, runner, cmd)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		errs = append(errs, fmt.Errorf("0: %w", err))
	}

	fmt.Fprintf(&out, "\n# END %s\n", marker)
	return out.String(), errs
}

// parseJob splits a job line into its schedule and command, and checks both.
// The schedule is five fields, or a single "@" keyword like "@daily".
func parseJob(line string) (schedule, command string, err error) {
	fields := strings.Fields(line)
	n := 5
	if strings.HasPrefix(line, "@") {
		n = 1
	}
	if len(fields) <= n {
		return "", "", errors.New("no command after the schedule")
	}
	if err := checkSchedule(fields[:n]); err != nil {
		return "", "", err
	}

	// Cut the schedule fields off the front of the line one at a time, so the
	// command keeps its original spacing and quoting
	command = line
	for range n {
		command = strings.TrimLeft(command, " \t")
		command = strings.TrimLeft(command[strings.IndexAny(command, " \t"):], " \t")
	}
	if err := checkCommand(command); err != nil {
		return "", "", err
	}
	return strings.Join(fields[:n], " "), command, nil
}

var keywords = map[string]bool{
	"@reboot": true, "@yearly": true, "@annually": true, "@monthly": true,
	"@weekly": true, "@daily": true, "@midnight": true, "@hourly": true,
}

var months = names("jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec")
var days = names("sun", "mon", "tue", "wed", "thu", "fri", "sat")

func names(list ...string) map[string]bool {
	m := make(map[string]bool)
	for _, s := range list {
		m[s] = true
	}
	return m
}

// checkSchedule catches schedules with the wrong shape: most often a missing
// field, which shifts part of the command into the schedule. It doesn't
// check numeric ranges (e.g., minute 75); cron itself logs those.
func checkSchedule(fields []string) error {
	if len(fields) == 1 {
		if !keywords[fields[0]] {
			return fmt.Errorf("unknown schedule keyword %q", fields[0])
		}
		return nil
	}

	fieldNames := []string{"minute", "hour", "day of month", "month", "day of week"}
	allowed := []map[string]bool{nil, nil, nil, months, days}
	for i, f := range fields {
		if !validField(f, allowed[i]) {
			return fmt.Errorf("invalid %s %q in schedule (is a field missing?)", fieldNames[i], f)
		}
	}
	return nil
}

// validField reports whether f is a cron field: a comma-separated list of
// "*", a value, or a range "a-b", each optionally followed by "/step". Values
// are numbers or, where allowed, three-letter month or day names.
func validField(f string, allowedNames map[string]bool) bool {
	isValue := func(s string) bool {
		return isNumber(s) || allowedNames[strings.ToLower(s)]
	}
	for item := range strings.SplitSeq(f, ",") {
		rng, step, hasStep := strings.Cut(item, "/")
		if hasStep && !isNumber(step) {
			return false
		}
		if rng == "*" {
			continue
		}
		lo, hi, isRange := strings.Cut(rng, "-")
		if !isValue(lo) || (isRange && !isValue(hi)) {
			return false
		}
	}
	return true
}

func isNumber(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// checkCommand rejects anything in a job's command that wouldn't make it to
// the container intact. Cron hands the whole line to the host's /bin/sh, so
// shell syntax like ";", pipes, or redirects runs on the host, around
// scripts/cron-job, rather than in the container. (A redirect is the sneaky
// one: "> /dev/null" would silently throw away the failure report cron
// mails.) Quoted text is passed along literally, which is how to chain
// commands in the container: sh -c 'dspace a && dspace b'.
//
// Cron also turns a bare "%" into a newline before the shell ever sees it,
// quoted or not, so that must be written as "\%".
func checkCommand(cmd string) error {
	hostShell := func(c byte) error {
		return fmt.Errorf("%q would be handled by the host's shell, not the container (see \"Modifying jobs\" in README.md)", c)
	}

	var quote byte // the open quote character, or 0 if not in quotes
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if c == '%' && (i == 0 || cmd[i-1] != '\\') {
			return errors.New(`bare "%" (cron turns it into a newline); write it as "\%"`)
		}

		switch {
		// Everything in single quotes is literal
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}

		// A backslash escapes the next character, outside quotes or in
		// double quotes
		case c == '\\':
			i++

		// Double quotes still allow "$" and backtick expansion
		case quote == '"':
			switch c {
			case '"':
				quote = 0
			case '$', '`':
				return hostShell(c)
			}

		case c == '\'' || c == '"':
			quote = c

		case strings.IndexByte(";&|<>()$`", c) >= 0:
			return hostShell(c)
		}
	}

	if quote != 0 {
		return fmt.Errorf("unclosed %c quote", quote)
	}
	return nil
}
