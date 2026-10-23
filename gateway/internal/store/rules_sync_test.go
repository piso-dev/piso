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
	// Empty/absent hosts mean "no destination", never "any destination".
	if got := CanonicalRuleHosts(nil); len(got) != 0 {
		t.Fatalf("nil: %+v", got)
	}
	if got := CanonicalRuleHosts([]string{}); len(got) != 0 {
		t.Fatalf("empty: %+v", got)
	}
	if got := CanonicalRuleHosts([]string{" ", ""}); len(got) != 0 {
		t.Fatalf("blank: %+v", got)
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

func TestSyncRulesForSecretNoHostsSeedsNothing(t *testing.T) {
	st := newRuleStore(t)
	sec := SecretRec{ID: "sec_1", Name: "k1", Placeholder: "piso_x_aaa", Value: "v", EnvKey: "K1"}
	if err := st.AddSecret(sec); err != nil {
		t.Fatal(err)
	}
	if err := st.SyncRulesForSecret(sec); err != nil {
		t.Fatal(err)
	}
	if rs := st.Rules(); len(rs) != 0 {
		t.Fatalf("unbound secret must seed no rules: %+v", rs)
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

func TestCanonicalRuleHostsWildcardAndInvalid(t *testing.T) {
	// A subdomain wildcard is a bound destination, not "any host".
	got := CanonicalRuleHosts([]string{" *.Tinfoil.sh ", "*.tinfoil.sh", "router-0.tinfoil.sh"})
	if len(got) != 2 || got[0] != "*.Tinfoil.sh" || got[1] != "router-0.tinfoil.sh" {
		t.Fatalf("wildcard dedupe: %+v", got)
	}
	// Unsupported shapes are dropped instead of approximated...
	got = CanonicalRuleHosts([]string{"router-*.tinfoil.sh", "*tinfoil.sh", "api.*.com", "api.example.com"})
	if len(got) != 1 || got[0] != "api.example.com" {
		t.Fatalf("invalid dropped: %+v", got)
	}
	// ...and reported so the API can 400 rather than silently ignore them.
	bad := InvalidRuleHosts([]string{"api.example.com", "router-*.tinfoil.sh", " ", "*.tinfoil.sh"})
	if len(bad) != 1 || bad[0] != "router-*.tinfoil.sh" {
		t.Fatalf("invalid hosts: %+v", bad)
	}
}

func TestHasRuleMatchesWildcard(t *testing.T) {
	st := newRuleStore(t)
	sec := SecretRec{ID: "sec_w", Placeholder: "piso_tinfoil_w", Value: "v",
		AllowedHosts: []string{"*.tinfoil.sh"}}
	if err := st.AddSecret(sec); err != nil {
		t.Fatal(err)
	}
	if err := st.SyncRulesForSecret(sec); err != nil {
		t.Fatal(err)
	}
	if !st.HasRule("piso_tinfoil_w", "router-0.tinfoil.sh") {
		t.Fatal("wildcard rule should cover router-0.tinfoil.sh")
	}
	if st.HasRule("piso_tinfoil_w", "tinfoil.sh") {
		t.Fatal("wildcard rule must not cover the apex")
	}
}

func TestSuffixWildcardForms(t *testing.T) {
	// A bare TLD is accepted but reported as broad.
	if !HasBroadSuffixWildcard([]string{"*.com"}) {
		t.Fatal("*.com should be flagged broad")
	}
	if HasBroadSuffixWildcard([]string{"*.example.com"}) {
		t.Fatal("*.example.com is not broad")
	}
	if HasBroadSuffixWildcard([]string{"*"}) {
		t.Fatal("plain * is not a suffix wildcard")
	}
}
