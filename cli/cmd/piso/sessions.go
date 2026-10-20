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

// pi roles observed from the process cmdline. Only a user attach or an
// unattended task run holds work a container recreate would destroy. The
// supervisor's rpc session is worker infrastructure that the entrypoint
// respawns, exactly like the monitor's wake loop, so it must not block updates.
const (
	roleUser       = "user"
	roleTask       = "task"
	roleSupervisor = "supervisor"
)

// activePiSession describes a worker that currently has one or more live pi
// processes. PIDs are the outermost PID-namespace ids (the host-side pid as
// Linux exposes it in /proc/<pid>/status NSpid), not container-relative ids,
// so they name a process on a native Linux host. On Docker Desktop the
// outermost pid belongs to the Linux VM; the worker name is the usable handle.
type activePiSession struct {
	Worker string   `json:"worker"`
	Slug   string   `json:"slug,omitempty"`
	PIDs   []string `json:"pids"`
	Uptime string   `json:"uptime"` // human interval, e.g. "12m", "3h2m"
	Roles  []string `json:"roles,omitempty"`
}

// blocksUpdate reports whether recreating the worker would kill real work: a
// user attach or an unattended task run. A worker whose only live pi is the
// supervisor rpc session does not block.
func (s activePiSession) blocksUpdate() bool {
	for _, role := range s.Roles {
		if role == roleUser || role == roleTask {
			return true
		}
	}
	return false
}

// isMonitorWorker reports the shared project-manager container. It lives in
// the gateway compose project (no per-project Dir) and runs pi -p on a wake
// loop — not a user attach session.
func isMonitorWorker(name, slug string) bool {
	return slug == monitorSlug || name == monitorWorkerName
}

// blockingSessions are sessions that must stop `piso update`: user attaches
// and unattended task runs. The monitor worker and the supervisor rpc session
// are worker infrastructure — rolled with the shared image and respawned by
// the entrypoint — so they are excluded.
func blockingSessions(sessions []activePiSession) []activePiSession {
	var out []activePiSession
	for _, s := range sessions {
		if isMonitorWorker(s.Worker, s.Slug) || !s.blocksUpdate() {
			continue
		}
		out = append(out, s)
	}
	return out
}

// infraSessions are non-monitor workers whose only live pi is infrastructure
// (the supervisor rpc session). Their pi is killed by the recreate and comes
// back from the entrypoint respawner, like the monitor's.
func infraSessions(sessions []activePiSession) []activePiSession {
	var out []activePiSession
	for _, s := range sessions {
		if isMonitorWorker(s.Worker, s.Slug) || s.blocksUpdate() {
			continue
		}
		out = append(out, s)
	}
	return out
}

// printSession renders one session line for `piso update`. Roles are sorted to
// keep the output stable regardless of /proc iteration order.
func printSession(s activePiSession) {
	role := strings.Join(s.Roles, "+")
	if role == "" {
		role = "pi"
	}
	fmt.Printf("  %s (%s): %s, host-side PIDs %s, up %s\n", s.Worker, s.Slug, role, strings.Join(s.PIDs, ","), s.Uptime)
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
# reject infra that merely contains piso-/bin/pi substrings. This text also
# contains the strings below, so the scanner excludes its own python process.
bad = re.compile(r'piso-entrypoint|piso-planning-watch|docker-init|sleep infinity')
hz = os.sysconf('SC_CLK_TCK'); up = time.monotonic()
def host_pid(pid):
    # outermost PID namespace id = host-side pid (native Linux); on Docker
    # Desktop it is the Linux VM pid. Fall back to the container pid.
    try:
        for line in open('/proc/%s/status' % pid):
            if line.startswith('NSpid:'):
                ids = line.split()[1:]
                return ids[0] if ids else pid
    except OSError:
        pass
    return pid
def role_of(cmd, name):
    if re.search(r'--mode\s+rpc', cmd) or name.startswith('piso-'):
        return 'supervisor'
    if re.search(r'(^|\s)-p(\s|$)', cmd):
        return 'task'
    return 'user'
for p in os.listdir('/proc'):
    if not p.isdigit(): continue
    try:
        raw = open('/proc/%s/cmdline' % p, 'rb').read().replace(b'\0', b' ')
        cmd = raw.decode('utf-8', 'replace')
        if bad.search(cmd): continue
        if not (want1.search(cmd) or want2.search(cmd)): continue
        name = ''
        m = re.search(r'--name\s+(\S+)', cmd)
        if m: name = m.group(1)
        st = open('/proc/%s/stat' % p).read()
        st = st[st.rfind(')') + 2:]
        f = st.split()
        start = int(f[19])
        secs = int(up - start / hz)
        print('%s\t%s\t%s\t%d' % (host_pid(p), role_of(cmd, name), name, secs))
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
		var pids, roles []string
		seenRole := map[string]bool{}
		var maxSecs int
		for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
			fields := strings.Split(line, "\t")
			if len(fields) < 4 {
				continue
			}
			pids = append(pids, fields[0])
			if !seenRole[fields[1]] {
				seenRole[fields[1]] = true
				roles = append(roles, fields[1])
			}
			if secs, err := strconv.Atoi(fields[3]); err == nil && secs > maxSecs {
				maxSecs = secs
			}
		}
		if len(pids) == 0 {
			continue
		}
		sort.Strings(pids)
		sort.Strings(roles)
		out = append(out, activePiSession{
			Worker: w.Name, Slug: w.Slug, PIDs: pids, Uptime: fmtInterval(maxSecs), Roles: roles,
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
