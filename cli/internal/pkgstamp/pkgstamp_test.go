package pkgstamp

import "testing"

func TestHashAndMerge(t *testing.T) {
	fromSettings := DepsFromPackages([]string{"npm:pi-browser", "npm:pi-hermes-memory"})
	host, err := DepsFromHostPackageJSON([]byte(`{"dependencies":{"pi-browser":"^0.1.0"}}`))
	if err != nil {
		t.Fatal(err)
	}
	deps := MergeDeps(host, fromSettings)
	if deps["pi-browser"] != "^0.1.0" {
		t.Fatalf("host version should win: %v", deps)
	}
	if deps["pi-hermes-memory"] != "*" {
		t.Fatalf("settings fill: %v", deps)
	}
	a := Hash(deps)
	b := Hash(deps)
	if a != b || a == "" {
		t.Fatalf("unstable hash")
	}
	deps["pi-browser"] = "^0.2.0"
	if Hash(deps) == a {
		t.Fatal("hash should change when a version changes")
	}
}

func TestPackageJSON(t *testing.T) {
	raw, err := PackageJSON(map[string]string{"pi-browser": "^0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == "" {
		t.Fatal("empty package.json")
	}
}
