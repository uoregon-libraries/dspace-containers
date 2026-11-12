package main

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// runLog is a run's log. It goes to a file in the backup dir, and to memory
// so a failed run can still print it when the backup dir is what's failing.
// Writes never fail, since a command whose output can't be logged should
// still get to run.
type runLog struct {
	mu      sync.Mutex
	file    *os.File // nil until setup opens it
	fileErr error
	mem     headTail
}

func (l *runLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.mem.write(p)
	if l.file != nil && l.fileErr == nil {
		if _, err := l.file.Write(p); err != nil {
			l.fileErr = err
			l.mem.write(fmt.Appendf(nil, "[%s] can't write the log file (%s); the rest of the log is only here\n", stamp(), err))
		}
	}
	return len(p), nil
}

// Printf logs one line, with a timestamp
func (l *runLog) Printf(format string, args ...any) {
	fmt.Fprintf(l, "[%s] %s\n", stamp(), fmt.Sprintf(format, args...))
}

func stamp() string {
	return time.Now().Format("2006-01-02 15:04:05")
}

// contents returns everything logged, or as much of it as memory kept.
// path is the log file, for a note about anything left out.
func (l *runLog) contents(path string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.mem.String(path)
}

func (l *runLog) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		l.file.Close()
		l.file = nil
	}
}

// Most logs are short, but rsync can print a line per file when something's
// wrong, so memory only keeps the beginning and end of a log
const memHead, memTail = 256 << 10, 256 << 10

type headTail struct {
	head, tail []byte
	total      int64
}

func (h *headTail) write(p []byte) {
	h.total += int64(len(p))
	if n := min(memHead-len(h.head), len(p)); n > 0 {
		h.head = append(h.head, p[:n]...)
		p = p[n:]
	}
	h.tail = append(h.tail, p...)
	if len(h.tail) > 2*memTail {
		h.tail = append([]byte(nil), h.tail[len(h.tail)-memTail:]...)
	}
}

func (h *headTail) String(path string) string {
	tail := h.tail
	if len(tail) > memTail {
		tail = tail[len(tail)-memTail:]
	}
	omitted := h.total - int64(len(h.head)) - int64(len(tail))
	if omitted == 0 {
		return string(h.head) + string(tail)
	}
	return fmt.Sprintf("%s\n[... %d bytes left out here; see %s ...]\n%s", h.head, omitted, path, tail)
}
