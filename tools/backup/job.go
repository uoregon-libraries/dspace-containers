package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"syscall"
	"time"
	"unicode"
)

// job is one run of a backup job
type job struct {
	kind     string // "backup" or "backup-solr": names the lock, and starts the log
	checkout string
	env      []byte // the checkout's .env
	project  string // compose project name
	dest     string // where snapshots go
	keepDays int
	started  time.Time
	name     string // the snapshot's name, from its start time
	run      string // the snapshot's dir: <dest>/<name>.partial until it's published
	prev     string // the snapshot "latest" pointed at when the run began, if any

	log     *runLog
	logPath string
	lock    *os.File

	// podman builds a podman command that runs as the podman user. Tests swap
	// in a fake.
	podman func(ctx context.Context, args ...string) *exec.Cmd

	// failed means something went wrong, even if a snapshot was published, so
	// the job has to exit 1 and print its log
	failed bool
}

// snapshotLayout is the time format of snapshot names, which sort
// chronologically
const snapshotLayout = "2006-01-02_150405"

// cronPath replaces cron's minimal PATH; some distros keep runuser in sbin
const cronPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

var errLocked = errors.New("skipped; the previous run is still going")

// setup does everything a job needs before it can back anything up, and
// returns with the job's lock held and its log open. Its errors have to be
// printed by the caller, since there's no log yet.
func setup(kind, podmanUser, checkout, dest, subdir string, keepDays int) (*job, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("must run as root")
	}
	if err := checkInstall(); err != nil {
		return nil, err
	}
	os.Setenv("PATH", cronPath)

	// Backups hold password hashes and private keys
	syscall.Umask(0o077)

	checkout, err := checkoutDir(checkout)
	if err != nil {
		return nil, err
	}
	env, err := os.ReadFile(filepath.Join(checkout, ".env"))
	if err != nil {
		return nil, err
	}

	dest, err = resolveDir(dest)
	if err != nil {
		return nil, fmt.Errorf("backup dir: %w", err)
	}
	if subdir != "" {
		dest = filepath.Join(dest, subdir)
		if err := os.MkdirAll(dest, 0o700); err != nil {
			return nil, err
		}
	}

	// Everything uses absolute paths, and the podman user may not be able to
	// read cron's working directory (root's home)
	if err := os.Chdir("/"); err != nil {
		return nil, err
	}

	u, err := lookupUser(podmanUser)
	if err != nil {
		return nil, err
	}

	lock, err := takeLock(kind, checkout)
	if err != nil {
		return nil, err
	}

	j := newJob(kind, checkout, env, dest, keepDays, time.Now())
	j.lock = lock
	j.podman = j.podmanAs(u)

	logDir := filepath.Join(dest, "logs")
	j.logPath = filepath.Join(logDir, j.name+".log")
	err = os.MkdirAll(logDir, 0o700)
	if err == nil {
		j.log.file, err = os.OpenFile(j.logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	}
	if err != nil {
		lock.Close()
		return nil, fmt.Errorf("can't create the log: %w", err)
	}

	j.log.Printf("%s: backing up %s to %s (backup tool %s)", kind, checkout, dest, buildInfo())
	return j, nil
}

// newJob fills in a job's basics. Tests use it in place of setup, which
// needs root.
func newJob(kind, checkout string, env []byte, dest string, keepDays int, started time.Time) *job {
	project := envSetting(env, "COMPOSE_PROJECT_NAME")
	if project == "" {
		project = strings.ToLower(filepath.Base(checkout))
	}
	return &job{
		kind:     kind,
		checkout: checkout,
		env:      env,
		project:  project,
		dest:     dest,
		keepDays: keepDays,
		started:  started,
		name:     started.Format(snapshotLayout),
		log:      &runLog{},
	}
}

// close releases the log and the lock. It's the last thing a job does.
func (j *job) close() {
	j.log.close()
	if j.lock != nil {
		j.lock.Close()
	}
}

// problem records a failure that isn't any one part's
func (j *job) problem(format string, args ...any) {
	j.failed = true
	j.log.Printf("ERROR: "+format, args...)
}

// guard runs f, turning a panic into a logged problem, so a bug still gets
// the log printed for cron to mail
func (j *job) guard(f func()) {
	defer func() {
		if r := recover(); r != nil {
			j.problem("panic: %v\n%s", r, debug.Stack())
		}
	}()
	f()
}

// finish returns the job's exit status. A job that failed in any way prints
// its log for cron to mail.
func (j *job) finish() int {
	if !j.failed {
		j.log.Printf("done")
		return 0
	}
	j.log.Printf("%s FAILED; see above", j.kind)
	os.Stdout.WriteString(j.log.contents(j.logPath))
	return 1
}

// checkInstall makes sure nobody but root can replace the binary, since root
// runs it: anybody who could would get root. That rules out a checkout's bin/
// dir, which the podman user can usually write to.
func checkInstall() error {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return err
	}
	if err := rootOnly(exe); err != nil {
		return fmt.Errorf("%w; install the binary where only root can write, e.g., /usr/local/sbin (see docs/backups.md)", err)
	}
	return nil
}

// rootOnly returns an error unless path, and every dir above it, is owned by
// root and can't be written by anybody else
func rootOnly(path string) error {
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		fi, err := os.Stat(p)
		if err != nil {
			return err
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("can't tell who owns %s", p)
		}
		perm := fi.Mode().Perm()
		if st.Uid != 0 {
			return fmt.Errorf("%s is owned by uid %d, not root", p, st.Uid)
		}
		if perm&0o002 != 0 || perm&0o020 != 0 && st.Gid != 0 {
			return fmt.Errorf("users other than root can write to %s", p)
		}
		if p == filepath.Dir(p) {
			return nil
		}
	}
}

// checkoutDir resolves the checkout to back up, making sure it is one
func checkoutDir(dir string) (string, error) {
	dir, err := resolveDir(dir)
	if err != nil {
		return "", fmt.Errorf("checkout: %w", err)
	}
	for _, f := range []string{"compose.yml", ".env"} {
		if !exists(filepath.Join(dir, f)) {
			return "", fmt.Errorf("%s isn't a checkout: there's no %s", dir, f)
		}
	}
	return dir, nil
}

// resolveDir returns dir as an absolute path with symlinks resolved, after
// making sure it's a directory
func resolveDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err == nil {
		abs, err = filepath.EvalSymlinks(abs)
	}
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s isn't a directory", abs)
	}
	return abs, nil
}

type podmanUser struct {
	name, uid, home string
}

// lookupUser finds the podman user with getent, which knows about LDAP and
// the like; Go's own lookup needs cgo for that, and this binary is static
func lookupUser(name string) (podmanUser, error) {
	out, err := exec.Command("getent", "passwd", name).Output()
	if err != nil {
		return podmanUser{}, fmt.Errorf("can't find user %q: %w", name, err)
	}
	line, _, _ := strings.Cut(string(out), "\n")
	fields := strings.Split(line, ":")
	if len(fields) < 7 {
		return podmanUser{}, fmt.Errorf("can't parse getent's entry for %q: %q", name, line)
	}
	return podmanUser{name: fields[0], uid: fields[2], home: fields[5]}, nil
}

// takeLock takes one lock per job, per install, as scripts/cron-job does.
// Installs are told apart by their full path. The lock goes away when the
// process does.
func takeLock(kind, checkout string) (*os.File, error) {
	path := "/run/lock/" + kind + lockSafe(checkout) + ".lock"
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLocked
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return f, nil
}

// lockSafe turns a path into something usable in a file name, the way
// scripts/cron-job does with tr
func lockSafe(s string) string {
	b := []byte(s)
	for i, c := range b {
		ok := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-'
		if !ok {
			b[i] = '_'
		}
	}
	return string(b)
}

// envSetting reads a setting from .env the way scripts/compose does: the last
// assignment wins, an inline comment (whitespace, then "#") is dropped, and
// so are quotes and whitespace
func envSetting(env []byte, name string) string {
	re := regexp.MustCompile(`^\s*` + regexp.QuoteMeta(name) + `\s*=(.*)$`)
	var value string
	for line := range strings.SplitSeq(string(env), "\n") {
		if m := re.FindStringSubmatch(line); m != nil {
			value = m[1]
		}
	}
	for i := 0; i+1 < len(value); i++ {
		if unicode.IsSpace(rune(value[i])) && value[i+1] == '#' {
			value = value[:i]
			break
		}
	}
	return strings.Map(func(r rune) rune {
		if r == '"' || r == '\'' || unicode.IsSpace(r) {
			return -1
		}
		return r
	}, value)
}

// command builds a command whose output goes to the log. If ctx is canceled,
// the command gets a SIGTERM, and a SIGKILL a minute later if it's still
// going.
func (j *job) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = time.Minute
	cmd.Stdout, cmd.Stderr = j.log, j.log
	return cmd
}

// podmanAs returns a podman command builder that runs as u, with the
// environment rootless podman needs to find u's containers
func (j *job) podmanAs(u podmanUser) func(context.Context, ...string) *exec.Cmd {
	return func(ctx context.Context, args ...string) *exec.Cmd {
		full := append([]string{
			"-u", u.name, "--", "env",
			"HOME=" + u.home,
			"XDG_RUNTIME_DIR=/run/user/" + u.uid,
			"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/" + u.uid + "/bus",
			"podman",
		}, args...)
		return j.command(ctx, "runuser", full...)
	}
}

// output runs cmd and returns its stdout. Its stderr still goes to the log.
func output(cmd *exec.Cmd) (string, error) {
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	return out.String(), err
}

// containerFor returns the name of a compose service's running container
func (j *job) containerFor(ctx context.Context, service string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := output(j.podman(ctx, "ps", "--format", "{{.Names}}",
		"--filter", "label=io.podman.compose.project="+j.project,
		"--filter", "label=com.docker.compose.service="+service))
	if err != nil {
		return "", fmt.Errorf("listing %s containers: %w", service, err)
	}
	names := strings.Fields(out)
	if len(names) != 1 {
		found := "none"
		if len(names) > 0 {
			found = strings.Join(names, ", ")
		}
		return "", fmt.Errorf("expected one running %s container in project %s, found: %s", service, j.project, found)
	}
	return names[0], nil
}
