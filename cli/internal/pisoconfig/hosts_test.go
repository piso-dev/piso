package pisoconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendPisoLocalAddsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(path, []byte("127.0.0.1 localhost\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := appendPisoLocal(path); err != nil {
		t.Fatal(err)
	}
	if err := appendPisoLocal(path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), DashboardHost); n != 2 {
		t.Fatalf("piso.local lines: %d (want v4+v6)\n%s", n, b)
	}
}

func TestAppendPisoLocalIdempotentExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(path, []byte("127.0.0.1 localhost piso.local\n::1 piso.local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := appendPisoLocal(path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(b), "managed by piso") != 0 {
		t.Fatalf("should not append when v4 and v6 already present:\n%s", b)
	}
}

func TestAppendIngressHostsAddsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(path, []byte("127.0.0.1 localhost\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := appendIngressHosts(path, []string{"plan-piso", "plan-piso", "bad.name"}); err != nil {
		t.Fatal(err)
	}
	if err := appendIngressHosts(path, []string{"plan-piso"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "plan-piso.piso.local"); n != 2 {
		t.Fatalf("fqdn lines: %d (want v4+v6)\n%s", n, b)
	}
	if strings.Contains(string(b), "bad.name") {
		t.Fatal("rejected name was written")
	}
}

func TestReconcileAddsAndRemoves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(path, []byte("127.0.0.1 localhost\n127.0.0.1 old.piso.local # managed by piso\n::1 old.piso.local # managed by piso\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// reconcile with only piso.local + new label: old is dropped, new added
	if err := reconcileHosts(path, []string{"demo-8080"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	txt := string(b)
	if strings.Contains(txt, "old.piso.local") {
		t.Fatalf("stale label not removed:\n%s", txt)
	}
	if strings.Count(txt, "demo-8080.piso.local") != 2 { // v4 + v6
		t.Fatalf("demo-8080 lines: %d\n%s", strings.Count(txt, "demo-8080.piso.local"), txt)
	}
	if strings.Count(txt, DashboardHost) != 4 {
		t.Fatalf("piso.local kept + added:\n%s", txt)
	}
}

func TestReconcilePreservesNonPisoLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	body := "# a comment\n127.0.0.1 localhost\n127.0.0.1 demo-8080.piso.local # managed by piso\n::1 demo-8080.piso.local # managed by piso\n127.0.0.1 some-other-host\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reconcileHosts(path, []string{"demo-8080"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	txt := string(b)
	if !strings.Contains(txt, "127.0.0.1 some-other-host") || !strings.Contains(txt, "# a comment") {
		t.Fatalf("non-piso lines lost:\n%s", txt)
	}
}

func TestReconcileIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(path, []byte("127.0.0.1 localhost\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileIngressHosts([]string{"demo-8080", "bad-name"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("127.0.0.1 localhost\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileIngressHosts([]string{"demo-8080", "bad-name"}); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileIngressHosts([]string{"demo-8080"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(b), "demo-8080.piso.local") != 2 {
		t.Fatalf("reconcile not idempotent:\n%s", string(b))
	}
}
