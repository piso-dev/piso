package store

import (
	"path/filepath"
	"testing"
)

func TestMcpUpsertAndFind(t *testing.T) {
	dir := t.TempDir()
	st, err := New(filepath.Join(dir, "state.json"), filepath.Join(dir, "log.jsonl"), filepath.Join(dir, "patterns.json"), filepath.Join(dir, "activities.db"), 10)
	if err != nil {
		t.Fatal(err)
	}
	rec := McpServerRec{
		ID: "mcp_1", Worker: "piso-worker-demo", Slug: "demo", Name: "ai-boost",
		URL: "https://mcp.example.com/mcp", Placeholder: "piso_mcp_demo_ai-boost",
		SecretID: "sec_1", Status: McpStatusNeedsAuth,
	}
	out, created, err := st.UpsertMcpServer(rec)
	if err != nil || !created {
		t.Fatalf("insert created=%v err=%v", created, err)
	}
	if out.ID != "mcp_1" {
		t.Fatalf("id %s", out.ID)
	}
	got, ok := st.FindMcp("piso-worker-demo", "ai-boost", "")
	if !ok || got.Placeholder != rec.Placeholder {
		t.Fatalf("find %+v ok=%v", got, ok)
	}
	again, created, err := st.UpsertMcpServer(rec)
	if err != nil || created {
		t.Fatalf("second insert created=%v err=%v", created, err)
	}
	if again.Placeholder != rec.Placeholder {
		t.Fatal("placeholder rotated")
	}
	if sum := got.ToSummary(); sum.Placeholder != rec.Placeholder {
		t.Fatal("summary lost placeholder")
	}
	if sum := got.ToSummary(); sum.ID == "" {
		t.Fatal("summary missing id")
	}
}

func TestApplyMigrationsV4ToV5McpServers(t *testing.T) {
	st := State{Version: 4}
	if !applyMigrations(&st) {
		t.Fatal("expected rewrite")
	}
	if st.Version != CurrentStateVersion {
		t.Fatalf("version %d", st.Version)
	}
	if st.McpServers == nil {
		t.Fatal("mcpServers should be non-nil")
	}
}
