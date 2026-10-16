package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestGatewayOnlySecretNeverPublished(t *testing.T) {
	for _, workers := range [][]string{nil, {"*"}, {"alpha"}} {
		sec := SecretRec{ID: "s", EnvKey: "GH_TOKEN", Placeholder: "piso_test", Value: "real-token", GatewayOnly: true, Workers: workers}
		for _, slug := range []string{"alpha", "beta", ""} {
			if SecretVisibleToWorker(sec, slug) {
				t.Fatal("gateway secret visible")
			}
			if strings.Contains(RenderPlaceholdersEnvForWorker([]SecretRec{sec}, slug), "piso_test") {
				t.Fatal("worker env leak")
			}
		}
		if strings.Contains(RenderPlaceholdersEnv([]SecretRec{sec}), "piso_test") {
			t.Fatal("legacy env leak")
		}
		st, err := New(filepath.Join(t.TempDir(), "state.json"), "", "", "", 100)
		if err != nil {
			t.Fatal(err)
		}
		if err = st.AddSecret(sec); err != nil {
			t.Fatal(err)
		}
		if err = st.AddRule(RuleRec{ID: "r", SecretID: "s", Placeholder: "piso_test", Host: "*"}); err != nil {
			t.Fatal(err)
		}
		if err = st.SyncRulesForSecret(sec); err != nil {
			t.Fatal(err)
		}
		if len(st.Rules()) != 0 {
			t.Fatal("gateway secret has substitution rules")
		}
	}
}
