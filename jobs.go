package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Job is a long-running background task whose log is polled by the UI.
type Job struct {
	ID      string
	Kind    string
	mu      sync.Mutex
	lines   []string
	done    bool
	err     string
	started time.Time
	partial string
}

var (
	jobsMu sync.Mutex
	jobs   = map[string]*Job{}
)

const maxJobLines = 20000

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newJob(kind string, fn func(j *Job) error) *Job {
	j := &Job{ID: randHex(8), Kind: kind, started: time.Now()}
	jobsMu.Lock()
	jobs[j.ID] = j
	jobsMu.Unlock()
	go func() {
		defer func() {
			if r := recover(); r != nil {
				j.finish(fmt.Errorf("internal error: %v", r))
			}
		}()
		j.finish(fn(j))
	}()
	return j
}

func getJob(id string) *Job {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	return jobs[id]
}

func (j *Job) Logf(format string, a ...any) {
	j.addLine(fmt.Sprintf(format, a...))
}

func (j *Job) addLine(s string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.lines) >= maxJobLines {
		j.lines = j.lines[len(j.lines)-maxJobLines/2:]
	}
	j.lines = append(j.lines, s)
}

// Write implements io.Writer, splitting output into lines (\n or \r).
func (j *Job) Write(p []byte) (int, error) {
	j.mu.Lock()
	s := j.partial + string(p)
	j.mu.Unlock()
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	parts := strings.Split(s, "\n")
	for _, l := range parts[:len(parts)-1] {
		j.addLine(stripANSI(l))
	}
	j.mu.Lock()
	j.partial = parts[len(parts)-1]
	j.mu.Unlock()
	return len(p), nil
}

func (j *Job) finish(err error) {
	j.mu.Lock()
	if j.partial != "" {
		j.lines = append(j.lines, stripANSI(j.partial))
		j.partial = ""
	}
	j.done = true
	if err != nil {
		j.err = err.Error()
		j.lines = append(j.lines, "ERROR: "+err.Error())
	} else {
		j.lines = append(j.lines, fmt.Sprintf("Finished in %s.", time.Since(j.started).Round(time.Second)))
	}
	j.mu.Unlock()
}

func (j *Job) snapshot(from int) map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	if from < 0 || from > len(j.lines) {
		from = 0
	}
	out := append([]string(nil), j.lines[from:]...)
	return map[string]any{"id": j.ID, "kind": j.Kind, "lines": out, "next": len(j.lines), "done": j.done, "error": j.err}
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			// skip CSI sequence ESC [ ... letter
			if i+1 < len(s) && s[i+1] == '[' {
				i += 2
				for i < len(s) && !((s[i] >= 'A' && s[i] <= 'Z') || (s[i] >= 'a' && s[i] <= 'z')) {
					i++
				}
				continue
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
