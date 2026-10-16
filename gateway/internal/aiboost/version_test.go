package aiboost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const versionDraftJSON = `{"id":"33333333-3333-3333-3333-333333333333","type":"new_version","status":"submitted","revision":1,"boosterId":"22222222-2222-2222-2222-222222222222","baseVersionId":"11111111-1111-1111-1111-111111111111","version":"1.1.0","content":{"format":"markdown","body":"\n# Updated\n  preserved  \n"},"changelog":"New checks","rationale":"Useful","review":{"title":"New version of Go","action":"Publish new version","boosterId":"22222222-2222-2222-2222-222222222222","boosterName":"Go","baseVersionId":"11111111-1111-1111-1111-111111111111","baseVersion":"1.0.0","proposedVersion":"1.1.0"}}`

func validVersionDraft() VersionDraft {
	return VersionDraft{
		BoosterID: "22222222-2222-2222-2222-222222222222", BaseVersionID: "11111111-1111-1111-1111-111111111111",
		Version: "1.1.0", Content: Content{"markdown", "\n# Updated\n  preserved  \n"}, Changelog: "New checks", Rationale: "Useful",
	}
}

func TestVersionValidation(t *testing.T) {
	for _, v := range []string{"0.0.0", "1.2.3", "1.2.3-rc.0+build.01", "1.2.3-01a"} {
		if err := validateVersion(v); err != nil {
			t.Fatal(v, err)
		}
	}
	for _, v := range []string{"", "v1.2.3", "1.2", "01.2.3", "1.2.3-01", "1.2.3-a.01", "1.2.3+", "1.2.3\n", "1.2.3-" + strings.Repeat("a", 32)} {
		if validateVersion(v) == nil {
			t.Fatal("accepted", v)
		}
	}
	for _, mutate := range []func(*VersionDraft){
		func(d *VersionDraft) { d.BoosterID = "../approve" },
		func(d *VersionDraft) { d.BaseVersionID = "" },
		func(d *VersionDraft) { d.Version = "latest" },
		func(d *VersionDraft) { d.Changelog = " " },
		func(d *VersionDraft) { d.Changelog = strings.Repeat("界", 10001) },
		func(d *VersionDraft) { d.Rationale = "" },
		func(d *VersionDraft) { d.Content.Body = strings.Repeat("界", 33334) },
		func(d *VersionDraft) { d.Content.Format = "html" },
	} {
		d := validVersionDraft()
		mutate(&d)
		if d.Validate("stable") == nil {
			t.Fatal("invalid draft accepted", d)
		}
	}
	if validVersionDraft().Validate("bad\nkey") == nil {
		t.Fatal("invalid key accepted")
	}
}

func TestVersionSubmissionAndReads(t *testing.T) {
	c, _, _ := fixture(t)
	connect(t, c)
	posts := 0
	c.http.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer access-token-never-public" {
			t.Fatal("gateway credential missing")
		}
		rec := httptest.NewRecorder()
		switch {
		case r.Method == "POST":
			posts++
			if r.URL.Path != "/api/drafts/" || r.Header.Get("Idempotency-Key") != "stable-version" {
				t.Fatal("wrong submission request")
			}
			var got map[string]any
			if json.NewDecoder(r.Body).Decode(&got) != nil || len(got) != 7 || got["type"] != "new_version" || got["boosterId"] != validVersionDraft().BoosterID || got["baseVersionId"] != validVersionDraft().BaseVersionID || got["version"] != "1.1.0" || got["changelog"] != "New checks" || got["rationale"] != "Useful" || got["content"].(map[string]any)["body"] != validVersionDraft().Content.Body {
				t.Fatal("bad version payload", got)
			}
			rec.WriteHeader(201)
			io.WriteString(rec, versionDraftJSON)
		case r.URL.Path == "/api/drafts/":
			if r.URL.Query().Get("status") != "submitted" || r.URL.Query().Get("cursor") != "opaque==" {
				t.Fatal("wrong filters", r.URL)
			}
			fmt.Fprintf(rec, `{"items":[%s],"next":"https://untrusted.example/?cursor=next%%3D%%3D","previous":null}`, versionDraftJSON)
		case r.URL.Path == "/api/drafts/"+editID+"/":
			io.WriteString(rec, versionDraftJSON)
		default:
			t.Fatal("unexpected path", r.URL)
		}
		return rec.Result(), nil
	})
	check := func(out Draft) {
		t.Helper()
		if out.Type != "new_version" || out.BoosterID != validVersionDraft().BoosterID || out.BaseVersionID != validVersionDraft().BaseVersionID || out.Version != "1.1.0" || out.Changelog != "New checks" || out.Content.Body != validVersionDraft().Content.Body || out.Review == nil || out.Review.BaseVersion != "1.0.0" || out.Review.Action != "Publish new version" {
			t.Fatal("version metadata lost", out)
		}
	}
	for i := 0; i < 2; i++ {
		out, err := c.DraftVersion(context.Background(), validVersionDraft(), "stable-version")
		if err != nil {
			t.Fatal(err)
		}
		check(out)
	}
	if posts != 2 {
		t.Fatal(posts)
	}
	page, err := c.Drafts(context.Background(), "submitted", "opaque==")
	if err != nil || len(page.Items) != 1 || page.NextCursor != "next==" || page.PreviousCursor != "" {
		t.Fatal(page, err)
	}
	check(page.Items[0])
	out, err := c.GetDraft(context.Background(), editID)
	if err != nil {
		t.Fatal(err)
	}
	check(out)
}

func TestVersionEditValidation(t *testing.T) {
	ptr := func(s string) *string { return &s }
	for _, e := range []DraftEdit{
		{ExpectedRevision: 1, BaseVersionID: ptr(validVersionDraft().BaseVersionID)},
		{ExpectedRevision: 1, Version: ptr("1.2.0")},
		{ExpectedRevision: 1, Changelog: ptr("Changed")},
	} {
		if err := e.Validate(editID); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []DraftEdit{
		{ExpectedRevision: 1, BaseVersionID: ptr("../x")},
		{ExpectedRevision: 1, Version: ptr("v1.2.0")},
		{ExpectedRevision: 1, Changelog: ptr(" ")},
		{ExpectedRevision: 1, Version: ptr("1.2.0"), Name: ptr("mixed")},
		{ExpectedRevision: 1, Changelog: ptr("Changed"), SuggestedTags: &[]string{}},
	} {
		if e.Validate(editID) == nil {
			t.Fatal("invalid edit accepted", e)
		}
	}
}
