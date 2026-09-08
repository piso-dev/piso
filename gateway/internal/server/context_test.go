package server

import (
	"testing"

	"piso/gateway/internal/store"
)

// POST /api/v1/worker/context on the WorkerHandler stores the watcher's live
// context, sanitizes labels, and feeds log-row enrichment.
func TestWorkerPostContext(t *testing.T) {
	s := testServer(t)
	wh := s.WorkerHandler()

	res := doJSON(t, wh, "POST", "/api/v1/worker/context", map[string]any{
		"worker": "piso-worker-demo", "slug": "demo",
		"folder": "/workspace", "project": "demo",
		"branch": "feat/x", "commit": "abc1234",
		"model": "routstr/deepseek-v4-flash-0731",
	})
	if res.Code != 200 {
		t.Fatalf("post context %d %s", res.Code, res.Body.String())
	}
	ctx, ok := s.Store.WorkerCtxBySlug("demo")
	if !ok {
		t.Fatal("context not stored")
	}
	if ctx.Project != "demo" || ctx.Branch != "feat/x" || ctx.Commit != "abc1234" || ctx.Model != "routstr/deepseek-v4-flash-0731" {
		t.Fatalf("stored ctx %+v", ctx)
	}

	// corruption in labels (control chars) is sanitized away before store
	res = doJSON(t, wh, "POST", "/api/v1/worker/context", map[string]any{
		"worker": "piso-worker-demo", "slug": "demo",
		"project": "demo\n<script>", "branch": "a\tb  c", "commit": "xxx",
		"model": "routstr/deepseek-v4-flash-0731",
	})
	if res.Code != 200 {
		t.Fatalf("post dirty %d %s", res.Code, res.Body.String())
	}
	ctx, _ = s.Store.WorkerCtxBySlug("demo")
	if ctx.Project != "demo <script>" {
		t.Fatalf("sanitize newline: %q", ctx.Project)
	}
	if ctx.Branch != "a b c" {
		t.Fatalf("sanitize ws: %q", ctx.Branch)
	}

	// a logged request from that worker carries the snapshot labels (latest
	// report: project sanitized to "demo <script>", branch "a b c")
	s.Store.AppendLog(store.Record{ID: "r1", Slug: "demo", Action: "allow", Status: 200})
	rec := s.Store.Records(1)[0]
	if rec.Project != "demo <script>" || rec.Branch != "a b c" || rec.Commit != "xxx" || rec.Model != "routstr/deepseek-v4-flash-0731" {
		t.Fatalf("log row not enriched: %+v", rec)
	}

	// empty worker name is rejected
	if w := doJSON(t, wh, "POST", "/api/v1/worker/context", map[string]any{"slug": "demo"}); w.Code != 400 {
		t.Fatalf("missing worker should 400, got %d", w.Code)
	}
}
