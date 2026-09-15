package main

import (
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"piso/cli/internal/dockernet"
	"piso/cli/internal/pisoconfig"
)

const (
	monitorWorkerName = "piso-worker-monitor"
	monitorSlug       = "monitor"
)

// activePiSession describes a worker that currently has a live pi process.
type activePiSession struct {
	Worker string   `json:"worker"`
	Slug   string   `json:"slug,omitempty"`
	PIDs   []string `json:"pids"`
	Uptime string   `json:"uptime"` // human interval, e.g. "12m", "3h2m"
}

// isMonitorWorker reports the shared project-manager container. It lives in
// the gateway compose project (no per-project Dir) and runs pi -p on a wake
// loop — not a user attach session.
func isMonitorWorker(name, slug string) bool {
	return slug == monitorSlug || name == monitorWorkerName
}

// blockingSessions are user attaches that must stop `piso update`.
// Monitor pi is excluded: the monitor is rolled with the shared image and
// its in-flight pi -p is killed by the recreate.
func blockingSessions(sessions []activePiSession) []activePiSession {
	var out []activePiSession
	for _, s := range sessions {
		if !isMonitorWorker(s.Worker, s.Slug) {
			out = append(out, s)
		}
	}
	return out
}

func monitorSessions(sessions []activePiSession) []activePiSession {
	var out []activePiSession
	for _, s := range sessions {
		if isMonitorWorker(s.Worker, s.Slug) {
			out = append(out, s)
		}
	}
	return out
}

// ensureMonitorRec appends the monitor registry entry if the gateway has not
// checkin'd it yet, so the session scan still sees a running monitor.
func ensureMonitorRec(recs []pisoconfig.WorkerRec) []pisoconfig.WorkerRec {
	for _, w := range recs {
		if isMonitorWorker(w.Name, w.Slug) {
			return recs
		}
	}
	return append(recs, pisoconfig.WorkerRec{Name: monitorWorkerName, Slug: monitorSlug})
}

// scanActivePiSessions probes each RUNNING worker from recs for live pi
// processes and returns one entry per worker that has any. The worker image
// has no pgrep/ps, so we exec a small python3 snippet that scans /proc
// cmdlines and reports "pid uptimeSeconds" per real pi process.
//
// A real pi process is one whose cmdline (argv joined with spaces) contains
// "pi-coding-agent" (the node dist path) or whose argv[0]/full line is
// exactly "pi" or starts/ends as the `pi` binary. We deliberately do NOT match
// the substring "bin/pi" — it also appears inside "piso-entrypoint" and
// "piso-planning-watch", the container's own always-running infra, which
// would make every worker look busy forever.
func scanActivePiSessions(recs []pisoconfig.WorkerRec) []activePiSession {
	py := `import os,re,time
# real pi: full dist path, or the bare pi arg (argv[0]=pi, or a lone pi token)
want1 = re.compile(r'pi-coding-agent')
want2 = re.compile(r'(^|\s)pi(\s|$)')        # token exactly 'pi'
# reject infra that merely contains piso-/bin/pi substrings
bad = re.compile(r'piso-entrypoint|piso-planning-watch|docker-init|sleep infinity')
hz = os.sysconf('SC_CLK_TCK'); up = time.monotonic()
for p in os.listdir('/proc'):
    if not p.isdigit(): continue
    try:
        raw = open('/proc/%s/cmdline' % p, 'rb').read().replace(b'\0', b' ')
        cmd = raw.decode('utf-8', 'replace')
        if bad.search(cmd): continue
        if not (want1.search(cmd) or want2.search(cmd)): continue
        st = open('/proc/%s/stat' % p).read()
        st = st[st.rfind(')') + 2:]
        f = st.split()
        start = int(f[19])
        secs = int(up - start / hz)
        print('%s %d' % (p, secs))
    except Exception:
        pass
`
	var out []activePiSession
	for _, w := range recs {
		if !containerRunning(w.Name) {
			continue
		}
		raw, err := dockerOutputLite("exec", w.Name, "python3", "-c", py)
		if err != nil || strings.TrimSpace(raw) == "" {
			continue
		}
		var pids []string
		var maxSecs int
		for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			pids = append(pids, fields[0])
			if secs, err := strconv.Atoi(fields[1]); err == nil && secs > maxSecs {
				maxSecs = secs
			}
		}
		if len(pids) == 0 {
			continue
		}
		sort.Strings(pids)
		out = append(out, activePiSession{
			Worker: w.Name, Slug: w.Slug, PIDs: pids, Uptime: fmtInterval(maxSecs),
		})
	}
	return out
}

// fmtInterval renders seconds as a compact human interval ("0s", "12m", "3h2m").
func fmtInterval(secs int) string {
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	if secs < 3600 {
		return fmt.Sprintf("%dm", secs/60)
	}
	return fmt.Sprintf("%dh%dm", secs/3600, (secs%3600)/60)
}

// dockerOutputLite is a small docker-output wrapper for quick checks.
func dockerOutputLite(args ...string) (string, error) {
	bin, err := dockernet.LookPath()
	if err != nil {
		return "", err
	}
	out, err := exec.Command(bin, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}