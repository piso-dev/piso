package technocore

import (
	"testing"

	"piso/gateway/internal/gitaccess"
	"piso/gateway/internal/store"
	tc "piso/internal/technocore"
)

func TestGitSealedEnvelopeRoundTrip(t *testing.T) {
	origin := testStore(t)
	config := gitaccess.Config{Provider: "gitea", TokenType: "user", BaseURL: "https://git.example.test", Hints: map[string]string{"merge": "denied", "createBranch": "allowed"}}
	rec := store.SecretRec{ID: "sec_git", Name: "Gitea", Kind: gitaccess.Kind, Git: &config, Value: "real-gitea-token", Placeholder: "piso_git_test", EnvKey: "TEA_TOKEN", AllowedHosts: []string{"git.example.test"}, Workers: []string{"demo"}}
	if err := origin.AddSecret(rec); err != nil {
		t.Fatal(err)
	}
	plain, ok := sharePlaintext(origin, rec)
	if !ok {
		t.Fatal("Git connection not shareable")
	}
	id, err := tc.Generate("https://server.invalid", "peer")
	if err != nil {
		t.Fatal(err)
	}
	envelope := testEnvelope(t, id, "git-resource", rec.Name, rec.Placeholder, plain)
	envelope.Metadata["kind"] = gitaccess.Kind
	envelope.Metadata["envKey"] = "TEA_TOKEN"
	envelope.Metadata["allowedHosts"] = []string{"evil.test"} // sealed config is authoritative
	envelope.ContentVersion = 1
	peer := testStore(t)
	rowID, err := applyEnvelope(peer, id, envelope)
	if err != nil {
		t.Fatal(err)
	}
	received, ok := peer.SecretByID(rowID)
	if !ok || received.Value != rec.Value || received.Git == nil || received.Git.Hints["merge"] != "denied" {
		t.Fatalf("connection not materialized: %+v", received)
	}
	if len(received.AllowedHosts) != 1 || received.AllowedHosts[0] != "git.example.test" {
		t.Fatalf("public metadata overrode sealed host: %+v", received.AllowedHosts)
	}
	if !store.SecretVisibleToWorker(received, "demo") || store.SecretVisibleToWorker(received, "other") {
		t.Fatal("worker assignment lost in sharing")
	}
	roundTrip, ok := sharePlaintext(peer, received)
	if !ok || roundTrip != plain {
		t.Fatal("republishing changed sealed connection")
	}
	if _, err := applyEnvelope(peer, id, envelope); err != nil {
		t.Fatal(err)
	}
	again, _ := peer.SecretByID(rowID)
	if again.Value != rec.Value || again.PublishedHash != plaintextHash(plain) {
		t.Fatal("same-version replay corrupted token or hash")
	}
	// Advisory metadata edits are sealed content too: a stale same-version
	// envelope must not erase them before the next publish.
	updated := *again.Git
	updated.Hints = map[string]string{"merge": "unknown"}
	again.Git = &updated
	if err := peer.ReplaceSecret(again); err != nil {
		t.Fatal(err)
	}
	if _, err := applyEnvelope(peer, id, envelope); err != nil {
		t.Fatal(err)
	}
	pending, _ := peer.SecretByID(rowID)
	if pending.Git.Hints["merge"] != "unknown" {
		t.Fatal("stale envelope erased pending hint edit")
	}
}
