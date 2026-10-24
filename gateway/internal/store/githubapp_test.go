package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestGithubAppCRUD(t *testing.T) {
	dir := t.TempDir()
	st, err := New(filepath.Join(dir, "state.json"), filepath.Join(dir, "log.jsonl"), filepath.Join(dir, "patterns.json"), filepath.Join(dir, "activities.db"), 10)
	if err != nil {
		t.Fatal(err)
	}
	sec := SecretRec{
		ID: "sec_1", Name: "gha", Placeholder: "piso_github_app_testxx",
		Kind: "github-app", EnvKey: "GH_TOKEN", CreatedAt: time.Now().UTC(),
	}
	if err := st.AddSecret(sec); err != nil {
		t.Fatal(err)
	}
	rec := GithubAppRec{
		ID: "gha_1", SecretID: sec.ID, Name: "gha", Placeholder: sec.Placeholder,
		AppID: 42, PrivateKey: "-----BEGIN FAKE-----",
		Installations: []GithubInstallation{{ID: 9, Account: "complex-gh"}},
	}
	if _, err := st.UpsertGithubApp(rec); err != nil {
		t.Fatal(err)
	}
	got, ok := st.GithubAppByPlaceholder(sec.Placeholder)
	if !ok || got.AppID != 42 || got.Installations[0].Account != "complex-gh" {
		t.Fatalf("got %+v", got)
	}
	views := st.GithubAppViews()
	v := views[sec.Placeholder]
	if !v.HasPEM || len(v.Accounts) != 1 || v.Accounts[0] != "complex-gh" {
		t.Fatalf("view %+v", v)
	}
	if err := st.DeleteGithubApp(rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.SecretByID(sec.ID); ok {
		t.Fatal("secret should be deleted with app")
	}
}

// A mirrored app belongs to a routing group the coordinator owns: removing it
// must not take the group secret (and with it every other app in the group)
// down on this gateway.
func TestDeleteGithubAppRecordKeepsGroupSecret(t *testing.T) {
	dir := t.TempDir()
	st, err := New(filepath.Join(dir, "state.json"), filepath.Join(dir, "log.jsonl"), filepath.Join(dir, "patterns.json"), filepath.Join(dir, "activities.db"), 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddSecret(SecretRec{
		ID: "tc_rid", Name: "GitHub", Placeholder: "piso_github_peer",
		Kind: "github-app", EnvKey: "GH_TOKEN", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertGithubApp(GithubAppRec{
		ID: "gha_tc_42", SecretID: "tc_rid", Source: GithubAppSourceTechnocore,
		AppID: 42, PrivateKey: "pem",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteGithubAppRecord("gha_tc_42"); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.SecretByID("tc_rid"); !ok {
		t.Fatal("group secret removed with the shared app")
	}
	// Removing the group itself is still a full delete.
	if _, err := st.UpsertGithubApp(GithubAppRec{
		ID: "gha_local", SecretID: "tc_rid", AppID: 43, PrivateKey: "pem",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteGithubApp("gha_local"); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.SecretByID("tc_rid"); ok {
		t.Fatal("local delete left the group behind")
	}
}
