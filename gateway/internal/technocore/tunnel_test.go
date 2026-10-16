package technocore

import (
	"testing"

	tc "piso/internal/technocore"
)

func TestTunnelShouldStop(t *testing.T) {
	dir := t.TempDir()
	id, err := tc.Generate("https://coord.example", "gw")
	if err != nil {
		t.Fatal(err)
	}
	id.Status = tc.StatusActive
	id.Placement = tc.PlacementStandalone
	if err := tc.Save(tc.Path(dir), id); err != nil {
		t.Fatal(err)
	}

	_, stop := tunnelShouldStop(id, dir, func() bool { return true })
	if stop {
		t.Fatal("relay still needed: pool must stay up with no public URL")
	}

	id.PublicURL = "https://gw.example:8443"
	if err := tc.Save(tc.Path(dir), id); err != nil {
		t.Fatal(err)
	}
	_, stop = tunnelShouldStop(tc.Identity{}, dir, func() bool { return true })
	if stop {
		t.Fatal("public URL set but workers have not switched: pool must stay up")
	}
	_, stop = tunnelShouldStop(tc.Identity{}, dir, func() bool { return false })
	if !stop {
		t.Fatal("public URL set and no worker still needs relay: pool must stop")
	}
	_, stop = tunnelShouldStop(tc.Identity{}, dir, nil)
	if stop {
		t.Fatal("nil need func must not drop the pool")
	}

	id.Placement = tc.PlacementLocal
	if err := tc.Save(tc.Path(dir), id); err != nil {
		t.Fatal(err)
	}
	_, stop = tunnelShouldStop(tc.Identity{}, dir, func() bool { return true })
	if !stop {
		t.Fatal("local placement must stop the pool")
	}
}
