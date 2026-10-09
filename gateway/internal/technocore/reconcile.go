package technocore

import (
	"log"
	"math/rand"
	"os"
	"time"

	"piso/gateway/internal/store"
	tc "piso/internal/technocore"
)

const (
	reconcileInterval   = 45 * time.Second
	reconcileMinBackoff = time.Second
	reconcileMaxBackoff = 2 * time.Minute
)

// Reconciler pulls secret snapshots and republishes local secrets on a
// timer, and when the websocket nudges it. Pushes from the coordinator are
// hints: a gateway that was down misses them, and a sync that fails during
// ready is not retried by the socket loop. This loop is the retry.
//
// It is single-flight (one goroutine) and never runs HTTP on the websocket
// read loop. A failed attempt backs off from 1s to 2m with jitter. Nudges
// during that backoff are coalesced, not a tight loop.
type Reconciler struct {
	st      *store.Store
	dataDir string
	nudge   chan struct{}
}

func newReconciler(st *store.Store, dataDir string) *Reconciler {
	return &Reconciler{
		st:      st,
		dataDir: dataDir,
		nudge:   make(chan struct{}, 1),
	}
}

// Nudge asks for a reconcile soon. Non-blocking: a pending nudge is enough.
func (r *Reconciler) Nudge() {
	if r == nil {
		return
	}
	select {
	case r.nudge <- struct{}{}:
	default:
	}
}

func (r *Reconciler) loop() {
	backoff := reconcileMinBackoff
	next := time.Now()
	for {
		wait := time.Until(next)
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		// While a failure backoff is in force, ignore nudges. Selecting the
		// nudge channel here would spin if the socket kept signaling a down
		// coordinator. One coalesced nudge stays buffered for the next success.
		if backoff > reconcileMinBackoff && time.Now().Before(next) {
			<-timer.C
		} else {
			select {
			case <-r.nudge:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			case <-timer.C:
			}
		}
		if err := r.once(); err != nil {
			log.Printf("technocore: reconcile: %v", err)
			setLive(func(s *Status) { s.LastError = err.Error() })
			delay := jitterDuration(backoff)
			next = time.Now().Add(delay)
			backoff = growBackoff(backoff)
			continue
		}
		backoff = reconcileMinBackoff
		next = time.Now().Add(reconcileInterval)
	}
}

func (r *Reconciler) once() error {
	id, err := tc.Load(tc.Path(r.dataDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if id.Status != tc.StatusActive || id.ServerURL == "" {
		return nil
	}
	// Enrollment must retry even when unrelated secret/provider sync fails.
	localErr := syncLocalWorkers(r.st, id)
	if err := syncSecrets(r.st, r.dataDir, id); err != nil {
		return err
	}
	if err := syncAssignedWorkers(r.st, id); err != nil {
		return err
	}
	if err := publishAllSecrets(r.st, r.dataDir); err != nil {
		return err
	}
	if err := syncPiProvider(r.st, r.dataDir, id); err != nil {
		return err
	}
	return localErr
}

func growBackoff(current time.Duration) time.Duration {
	if current < reconcileMinBackoff {
		current = reconcileMinBackoff
	}
	next := current * 2
	if next > reconcileMaxBackoff || next < current {
		return reconcileMaxBackoff
	}
	return next
}

// jitterDuration returns d ± 20%. A zero duration stays zero.
func jitterDuration(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	span := int64(d) / 5
	if span <= 0 {
		return d
	}
	delta := rand.Int63n(span*2+1) - span
	return d + time.Duration(delta)
}
