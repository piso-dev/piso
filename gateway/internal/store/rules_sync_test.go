package store

import (
	"path/filepath"
	"testing"
)

func newRuleStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	st, err := New(filepath.Join(dir, "state.json"), filepath.Join(dir, "log.jsonl"), filepath.Join(dir, "patterns.json"), filepath.Join(dir, "activities.db"), 20)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestCanonicalRuleHosts(t *testing.T) {
	if got := CanonicalRuleHosts(nil); len(got) != 1 || got[0] != "*" {
		t.Fatalf("nil: %+v", got)
	}
	if got := CanonicalRuleHosts([]string{}); len(got) != 1 || got[0] != "*" {
		t.Fatalf("empty: %+v", got)
	}
	if got := CanonicalRuleHosts([]string{"*"}); len(got) != 1 || got[0] != "*" {
		t.Fatalf("star: %+v", got)
	}
	if got := CanonicalRuleHosts([]string{"api.example.com", "*", "x.test"}); len(got) != 1 || got[0] != "*" {
		t.Fatalf("star dominates specifics: %+v", got)
	}
	got := CanonicalRuleHosts([]string{" A.Test ", "a.test", "b.test"})
	if len(got) != 2 || got[0] != "A.Test" || got[1] != "b.test" {
		t.Fatalf("dedupe/case: %+v", got)
	}
}

func TestSyncRulesForSecretNoHostsGetsStarRule(t *testing.T) {
	st := newRuleStore(t)
	sec := SecretRec{ID: "sec_1", Name: "k1", Placeholder: "piso_x_aaa", Value: "v", EnvKey: "K1"}
	if err := st.AddSecret(sec); err != nil {
		t.Fatal(err)
	}
	if err := st.SyncRulesForSecret(sec); err != nil {
		t.Fatal(err)
	}
	rs := st.Rules()
	if len(rs) != 1 || rs[0].Placeholder != "piso_x_aaa" || rs[0].Host != "*" || rs[0].SecretID != "sec_1" {
		t.Fatalf("rules: %+v", rs)
	}
}

func TestSyncRulesForSecretPerHostDedupe(t *testing.T) {
	st := newRuleStore(t)
	sec := SecretRec{ID: "sec_2", Placeholder: "piso_y_bbb", Value: "v",
		AllowedHosts: []string{"A.Example.com", "a.example.com", "b.example.com"}}
	if err := st.AddSecret(sec); err != nil {
		t.Fatal(err)
	}
	if err := st.SyncRulesForSecret(sec); err != nil {
		t.Fatal(err)
	}
	rs := st.Rules()
	if len(rs) != 2 || rs[0].Host != "A.Example.com" || rs[1].Host != "b.example.com" {
		t.Fatalf("rules: %+v", rs)
	}
}

func TestSyncRulesForSecretReplacesOwnKeepsOthers(t *testing.T) {
	st := newRuleStore(t)
	other := SecretRec{ID: "sec_o", Placeholder: "piso_o", Value: "v", AllowedHosts: []string{"keep.test"}}
	if err := st.AddSecret(other); err != nil {
		t.Fatal(err)
	}
	if err := st.SyncRulesForSecret(other); err != nil {
		t.Fatal(err)
	}
	sec := SecretRec{ID: "sec_x", Placeholder: "piso_x_ccc", Value: "v", AllowedHosts: []string{"a.test"}}
	if err := st.AddSecret(sec); err != nil {
		t.Fatal(err)
	}
	// twice: idempotent
	if err := st.SyncRulesForSecret(sec); err != nil {
		t.Fatal(err)
	}
	if err := st.SyncRulesForSecret(sec); err != nil {
		t.Fatal(err)
	}
	// re-scope to another host: the old rule is replaced, others untouched
	sec.AllowedHosts = []string{"b.test"}
	if err := st.SyncRulesForSecret(sec); err != nil {
		t.Fatal(err)
	}
	rs := st.Rules()
	var mine []string
	for _, r := range rs {
		if r.Placeholder == "piso_x_ccc" {
			mine = append(mine, r.Host)
		}
		if r.Placeholder == "piso_o" && r.Host != "keep.test" {
			t.Fatalf("other clobbered: %+v", rs)
		}
	}
	if len(mine) != 1 || mine[0] != "b.test" {
		t.Fatalf("own rules: %+v", rs)
	}
	if len(rs) != 2 {
		t.Fatalf("total rules: %+v", rs)
	}
}