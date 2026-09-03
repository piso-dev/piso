package server

import (
	"testing"

	"piso/gateway/internal/store"
)

func TestHostExceptionRegex(t *testing.T) {
	got := hostExceptionRegex("release-assets.githubusercontent.com")
	want := `^release-assets\.githubusercontent\.com$`
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestExistingHostPatternException(t *testing.T) {
	es := []store.ExceptionRec{
		{ID: "off", Enabled: false, HostRegex: `^cdn\.example\.com$`, PatternID: "jwt"},
		{ID: "jwt", Enabled: true, HostRegex: `^cdn\.example\.com$`, PatternID: "jwt"},
		{ID: "sk", Enabled: true, HostRegex: `^cdn\.example\.com$`, PatternID: "openai-sk"},
	}
	got, ok := existingHostPatternException(es, `^cdn\.example\.com$`, "jwt")
	if !ok || got.ID != "jwt" {
		t.Fatalf("want jwt, got %+v ok=%v", got, ok)
	}
	if _, ok := existingHostPatternException(es, `^other\.test$`, "jwt"); ok {
		t.Fatal("other host should miss")
	}
}
