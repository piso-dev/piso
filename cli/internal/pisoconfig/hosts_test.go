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
