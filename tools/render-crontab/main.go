// render-crontab turns conf/crontab into a real crontab: conf/crontab is a
// stub of sorts, and this script's job is to fill in the path to the project,
// MAILTO, user to run cron as, full path to the cron running script, etc.
//
// It also rejects mistakes cron would otherwise act on silently: malformed
// schedules, and commands with syntax the *host's* shell would handle (";",
// pipes, redirects, "$", etc.) instead of passing it along to the container.
//
// Usage:
//
//     render-crontab [-dir <project dir>] [-user <user>]

package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// options holds everything render needs besides the schedule itself
type options struct {
	projectDir string // absolute path of the checkout
	user       string // run-as user for a system crontab; empty for a user crontab
	path       string // PATH for the jobs
	mailto     string // may be empty
}

func main() {
	dir := flag.String("dir", ".", "the project checkout the jobs will run in")
	user := flag.String("user", "", "render a system crontab (e.g., for /etc/cron.d) whose jobs run as this user;\nomit to render a user crontab (e.g., for \"crontab -e\")")
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

	var err error
	opts.projectDir, err = filepath.Abs(*dir)
	if err != nil {
		complain("%s", err)
	}
	// The path ends up unquoted in a shell command line, so keep it boring
	if !safePath.MatchString(opts.projectDir) {
		complain("project dir %q may only contain letters, digits, and /._-", opts.projectDir)
	}

	opts.user = *user
	if opts.user != "" && !safeUser.MatchString(opts.user) {
		complain("%q doesn't look like a user name", opts.user)
	}

	runner := filepath.Join(opts.projectDir, "scripts", "cron-job")
	if info, err := os.Stat(runner); err != nil {
		complain("%s", err)
	} else if info.Mode()&0111 == 0 {
		complain("%s isn't executable", runner)
	}

	opts.path, err = cronPath()
	if err != nil {
		complain("%s", err)
	}

	opts.mailto, err = readMailto(filepath.Join(opts.projectDir, ".env"))
	if err != nil {
		complain("%s", err)
	}

	// Check the schedule even if something above failed, so one run reports
	// everything that needs fixing
	var out string
	schedulePath := filepath.Join(opts.projectDir, "conf", "crontab")
	schedule, err := os.ReadFile(schedulePath)
	if err != nil {
		complain("%s (run this from the project directory, or use -dir)", err)
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

	if opts.mailto == "" {
		fmt.Fprintln(os.Stderr, "render-crontab: warning: CRON_MAILTO isn't set in .env, so cron will mail failures to the local account running the jobs")
	}
	fmt.Print(out)
}

var (
	safePath = regexp.MustCompile(`^[A-Za-z0-9/._-]+$`)
	safeUser = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9._-]*$`)
	envVar   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\s*=`)
)

// cronPath returns a PATH for the jobs: the usual system dirs, plus wherever
// podman-compose is installed if that's somewhere else (e.g., ~/.local/bin)
func cronPath() (string, error) {
	dirs := []string{"/usr/local/bin", "/usr/bin", "/bin"}
	compose, err := exec.LookPath("podman-compose")
	if err != nil {
		return "", errors.New("podman-compose isn't on your PATH")
	}
	if composeDir := filepath.Dir(compose); !slices.Contains(dirs, composeDir) {
		dirs = append([]string{composeDir}, dirs...)
	}
	return strings.Join(dirs, ":"), nil
}

// readMailto returns CRON_MAILTO from the given .env file, or "" if the file
// or the setting is missing. .env belongs to compose, which supports quoting
// and ${VAR} interpolation; we only accept a plain value (optionally quoted)
// rather than half-implement compose's rules.
func readMailto(envFile string) (string, error) {
	data, err := os.ReadFile(envFile)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}

	// Like compose, the last assignment wins
	var val string
	for line := range strings.SplitSeq(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "CRON_MAILTO="); ok {
			val = strings.TrimSpace(v)
		}
	}
	for _, q := range []string{`"`, `'`} {
		if len(val) >= 2 && strings.HasPrefix(val, q) && strings.HasSuffix(val, q) {
			val = val[1 : len(val)-1]
		}
	}

	if strings.ContainsAny(val, "$ \t") {
		return "", fmt.Errorf("CRON_MAILTO in %s must be a plain value: no ${...} or spaces (or trailing comments)", envFile)
	}
	return val, nil
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

	runner := filepath.Join(opts.projectDir, "scripts", "cron-job")
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
