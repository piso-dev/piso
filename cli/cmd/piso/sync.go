package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"piso/cli/internal/pisoconfig"
	"piso/internal/hostauth"
)

const hostsSyncInterval = 2 * time.Second

// A timeout bounds both connection setup and body reads. Stream requests have
// a separate lifetime so they can never hold up the snapshot reconciler.
func syncRequest(ctx context.Context, url string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	// Local installs don't require authentication. When available, use the host
	// identity so forcing dashboard authentication doesn't break hosts sync.
	if id, err := currentIdentity(); err == nil {
		if err := hostauth.Apply(req, id.SigningPrivateKey, time.Now()); err != nil {
			return nil, err
		}
	}
	return req, nil
}

func syncIngressHostsAt(ctx context.Context, gw string) error {
	return pisoconfig.ReconcileIngressHostsFrom(func() ([]string, error) {
		reqCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		req, err := syncRequest(reqCtx, gw+"/api/v1/routes")
		if err != nil {
			return nil, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("routes: gateway returned %s", resp.Status)
		}
		var routes []pisoconfig.RouteRec
		if err := json.NewDecoder(resp.Body).Decode(&routes); err != nil {
			return nil, err
		}
		names := []string{"mcp-oauth", "github-app"}
		for _, r := range routes {
			if r.Name != "" {
				names = append(names, r.Name)
			}
		}
		return names, nil
	})
}

func syncIngressHosts() error {
	return syncIngressHostsAt(context.Background(), pisoconfig.GatewayURL())
}

// runHostsSync treats SSE only as an immediacy hint. Polling never waits for
// stream setup, scanning, reconnects, or missed notifications. One goroutine
// owns reconciliation; a buffered hint coalesces bursts.
func runHostsSync(ctx context.Context, gw string, interval time.Duration, report func(error)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	hints := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() { defer close(done); watchHostsHints(ctx, gw, hints) }()
	defer func() { cancel(); <-done }()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	reconcile := func() { report(syncIngressHostsAt(ctx, gw)) }
	reconcile()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			reconcile()
		case <-hints:
			reconcile()
		}
	}
}

func watchHostsHints(ctx context.Context, gw string, hints chan<- struct{}) {
	for ctx.Err() == nil {
		// Periodically rotate even apparently live streams. This bounds a silent
		// half-open connection, including one that never produces response headers.
		streamCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		req, err := syncRequest(streamCtx, gw+"/api/v1/requests/stream")
		if err == nil {
			var resp *http.Response
			resp, err = http.DefaultClient.Do(req)
			if err == nil {
				if resp.StatusCode == http.StatusOK {
					select {
					case hints <- struct{}{}:
					default:
					}
					sc := bufio.NewScanner(resp.Body)
					sc.Buffer(make([]byte, 4096), 1<<20)
					for sc.Scan() {
						line := sc.Text()
						if strings.HasPrefix(line, "event: route") || strings.HasPrefix(line, "event: planning") {
							select {
							case hints <- struct{}{}:
							default:
							}
						}
					}
				}
				resp.Body.Close()
			}
		}
		cancel()
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func syncWatchLoop(gw string) error {
	var previous string
	return runHostsSync(context.Background(), gw, hostsSyncInterval, func(err error) {
		message := ""
		if err != nil {
			message = err.Error()
		}
		if message != previous {
			if err != nil {
				fmt.Fprintf(os.Stderr, "piso: hosts sync: %v\n", err)
			} else {
				fmt.Fprintln(os.Stderr, "piso: hosts sync recovered")
			}
			previous = message
		}
	})
}

func syncDaemonLoop(gw string) error {
	var previous string
	return runHostsSync(context.Background(), gw, hostsSyncInterval, func(err error) {
		if statusErr := pisoconfig.RecordSyncResult(err); statusErr != nil {
			fmt.Fprintf(os.Stderr, "piso: cannot record hosts sync status: %v\n", statusErr)
		}
		message := ""
		if err != nil {
			message = err.Error()
		}
		if message != previous {
			if err != nil {
				fmt.Fprintf(os.Stderr, "piso: hosts sync: %v\n", err)
			} else {
				fmt.Fprintln(os.Stderr, "piso: hosts sync recovered")
			}
			previous = message
		}
	})
}
