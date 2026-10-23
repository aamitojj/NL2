package main

import (
	"regexp"
	"strconv"
	"strings"
)

// Friendly names for the steps of Coolify's official installer
// (https://cdn.coollabs.io/coolify/install.sh prints "N/9  Title ... ✓").
var installFriendly = []struct{ key, label string }{
	{"required packages", "Installing basic tools"},
	{"openssh", "Checking remote access"},
	{"installing docker", "Installing Docker (runs your sites in containers)"},
	{"checking docker", "Checking Docker"},
	{"configuring docker", "Configuring Docker"},
	{"downloading required files", "Downloading the hosting engine"},
	{"environment variables", "Creating settings and secret keys"},
	{"ssh key", "Letting the engine manage this server"},
	{"pulling images", "Downloading engine components"},
	{"starting coolify", "Starting the hosting engine"},
}

var installStepRe = regexp.MustCompile(`^\s*(?:Step\s+)?(\d{1,2})\s*/\s*(\d{1,2})\b[:\s]+(.+)$`)

func friendlyInstallLabel(title string) string {
	t := strings.ToLower(title)
	for _, f := range installFriendly {
		if strings.Contains(t, f.key) {
			return f.label
		}
	}
	return strings.TrimSpace(title)
}

// installProgress turns installer output lines into progress. done reports
// whether the line marks the step as finished (the ✓ line) or started.
func parseInstallLine(line string) (step, total int, title string, done, ok bool) {
	m := installStepRe.FindStringSubmatch(stripANSI(line))
	if m == nil {
		return
	}
	step, _ = strconv.Atoi(m[1])
	total, _ = strconv.Atoi(m[2])
	if total < 3 || total > 30 || step < 1 || step > total {
		return 0, 0, "", false, false
	}
	rest := m[3]
	done = strings.Contains(rest, "✓") || strings.Contains(strings.ToLower(rest), "completed")
	// Title is the text before the first run of 2+ spaces (detail/duration follow).
	title = strings.TrimSpace(rest)
	if i := strings.Index(title, "  "); i > 0 {
		title = title[:i]
	}
	title = strings.TrimSuffix(strings.TrimSpace(strings.TrimSuffix(title, "✓")), ":")
	return step, total, title, done, true
}

func installLineHook(j *Job, line string) {
	step, total, title, done, ok := parseInstallLine(line)
	if !ok {
		if strings.Contains(line, "http://") && strings.Contains(line, ":8000") {
			j.SetProgress(98, "Almost done: the dashboard is starting")
		}
		return
	}
	pct := (step - 1) * 100 / total
	label := friendlyInstallLabel(title)
	if done {
		pct = step * 100 / total
		if step < total {
			label = "Finished: " + label
		}
	}
	if pct > 97 {
		pct = 97
	}
	j.SetProgress(pct, label)
}
