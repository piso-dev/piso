package aiboost

import (
	"strings"
	"testing"
)

func TestDraftValidation(t *testing.T) {
	valid := DraftPayload{Name: "Go", Description: "Guidance", Rationale: "Useful", Content: Content{Format: "markdown", Body: "\n# Go\n"}}
	for _, key := range []string{"", " leading", "x\ny", strings.Repeat("k", 129)} {
		if valid.Validate(key) == nil {
			t.Fatal("invalid key accepted")
		}
	}
	for _, mutate := range []func(*DraftPayload){
		func(d *DraftPayload) { d.Name = "" }, func(d *DraftPayload) { d.Name = strings.Repeat("界", 256) },
		func(d *DraftPayload) { d.Description = " " }, func(d *DraftPayload) { d.Rationale = strings.Repeat("x", 10001) },
		func(d *DraftPayload) { d.Content.Format = "html" }, func(d *DraftPayload) { d.Content.Body = "  " },
		func(d *DraftPayload) { d.Content.Body = strings.Repeat("界", 33334) }, func(d *DraftPayload) { d.Content.Body = string([]byte{0xff}) },
		func(d *DraftPayload) { d.SuggestedTags = []string{""} }, func(d *DraftPayload) { d.SuggestedTags = []string{strings.Repeat("x", 101)} },
		func(d *DraftPayload) { d.SuggestedTags = make([]string, 31) },
	} {
		d := valid
		mutate(&d)
		if d.Validate("stable-key") == nil {
			t.Fatal("invalid draft accepted")
		}
	}
	valid.Content.Body = strings.Repeat("x", 100000)
	if err := valid.Validate("stable-key"); err != nil {
		t.Fatal(err)
	}
}
