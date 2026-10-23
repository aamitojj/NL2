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
	// progress is 0-100, or -1 when unknown; stage is a short friendly label.
	progress       int
	stage          string
	steps          []string // friendly step names shown as a checklist
	stepIdx        int
	spanLo, spanHi int // SetProgress maps 0-100 into this range (default 0-100)
	// onLine, if set, sees every log line (used to derive progress).
	onLine func(j *Job, line string)
	// result is optional structured output for the UI (e.g. an import id).
	result any
}

// SetResult attaches structured output shown to the UI when the job ends.
func (j *Job) SetResult(v any) {
	j.mu.Lock()
	j.result = v
	j.mu.Unlock()
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
	j := &Job{ID: randHex(8), Kind: kind, started: time.Now(), progress: -1, spanHi: 100}
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
	if len(j.lines) >= maxJobLines {
		j.lines = j.lines[len(j.lines)-maxJobLines/2:]
	}
	j.lines = append(j.lines, s)
	cb := j.onLine
	j.mu.Unlock()
	if cb != nil {
		cb(j, s)
	}
}

// SetProgress updates the progress bar (pct < 0 keeps the current value).
func (j *Job) SetProgress(pct int, stage string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if pct >= 0 {
		if pct > 100 {
			pct = 100
		}
		j.progress = j.spanLo + pct*(j.spanHi-j.spanLo)/100
	}
	if stage != "" {
		j.stage = stage
	}
}

// Span makes later SetProgress calls fill only lo..hi of the bar (for jobs
// made of several phases).
func (j *Job) Span(lo, hi int) {
	j.mu.Lock()
	j.spanLo, j.spanHi = lo, hi
	j.mu.Unlock()
}

// SetSteps sets the friendly checklist; Step marks step i (0-based) as current.
func (j *Job) SetSteps(steps ...string) {
	j.mu.Lock()
	j.steps = steps
	j.stepIdx = 0
	j.mu.Unlock()
}

func (j *Job) Step(i int) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if i >= j.stepIdx {
		j.stepIdx = i
	}
	if len(j.steps) > 0 && i < len(j.steps) {
		j.stage = j.steps[i]
		if p := j.spanLo + i*(j.spanHi-j.spanLo)/len(j.steps); p > j.progress {
			j.progress = p
		}
	}
}

// Lines returns a copy of the last n log lines.
func (j *Job) Lines(n int) []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	from := 0
	if len(j.lines) > n {
		from = len(j.lines) - n
	}
	return append([]string(nil), j.lines[from:]...)
}

func anyJobRunning() bool {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	for _, j := range jobs {
		j.mu.Lock()
		d := j.done
		j.mu.Unlock()
		if !d {
			return true
		}
	}
	return false
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
	if err == nil {
		j.progress = 100
		if len(j.steps) > 0 {
			j.stepIdx = len(j.steps)
		}
	}
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
	return map[string]any{"id": j.ID, "kind": j.Kind, "lines": out, "next": len(j.lines), "done": j.done, "error": j.err,
		"progress": j.progress, "stage": j.stage, "steps": j.steps, "step": j.stepIdx, "started": j.started, "result": j.result}
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
