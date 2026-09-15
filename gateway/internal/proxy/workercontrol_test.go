package proxy

import (
	"net"
	"net/http"
	"path/filepath"
	"testing"

	"piso/gateway/internal/store"
)

// testHandler builds a Handler with a real store + a workerFn that resolves
// the request's RemoteAddr → slug (same as production workerIdentityFn).
func testHandler(t *testing.T) (*Handler, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "state.json"), filepath.Join(dir, "log.jsonl"), filepath.Join(dir, "patterns.json"), filepath.Join(dir, "activities.db"), 20)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{
		Store: st,
		Worker: func(r *http.Request) string {
			// strip the port like production workerIdentityFn (main.go)
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				host = r.RemoteAddr
			}
			rec, ok := st.WorkerByIP(host)
			if !ok {
				return ""
			}
			return rec.Slug
		},
	}
	return h, st
}

func TestWorkerInternetBlocked(t *testing.T) {
	h, st := testHandler(t)
	if _, err := st.UpsertWorker(store.WorkerRec{Name: "piso-worker-demo", Slug: "demo", IPs: []string{"192.168.107.50"}}); err != nil {
		t.Fatal(err)
	}

	req := &http.Request{RemoteAddr: "192.168.107.50:12345"}

	// enabled → not blocked
	if blocked, _ := h.workerInternetBlocked(req); blocked {
		t.Fatal("worker with internet enabled should not be blocked")
	}

	// disable → blocked with correct slug
	if _, err := st.SetWorkerInternet("piso-worker-demo", true); err != nil {
		t.Fatal(err)
	}
	blocked, slug := h.workerInternetBlocked(req)
	if !blocked {
		t.Fatal("disabled worker should be blocked")
	}
	if slug != "demo" {
		t.Fatalf("slug %q, want demo", slug)
	}

	// unknown IP → not blocked (no registry entry)
	other := &http.Request{RemoteAddr: "192.168.107.99:80"}
	if blocked, _ := h.workerInternetBlocked(other); blocked {
		t.Fatal("unknown worker should not be blocked")
	}
}
