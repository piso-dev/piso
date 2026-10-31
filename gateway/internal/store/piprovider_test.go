package store

import (
	"strings"
	"testing"
)

// The allowed set is the one rule the proxy pin, the dashboard view, and (from
// the next change) the worker's /model list all read, so its fallbacks are
// pinned here rather than only through the proxy.
func TestPiProviderAllowedModelIDs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rec   PiProviderRec
		want  string
		unset bool
	}{
		{
			name: "approved list in stored order",
			rec:  PiProviderRec{DefaultModel: "a", ApprovedModels: []string{"a", "b"}},
			want: "a,b",
		},
		{
			name: "empty list means the default alone",
			rec:  PiProviderRec{DefaultModel: "a"},
			want: "a",
		},
		{
			name: "default is appended when the list omits it",
			rec:  PiProviderRec{DefaultModel: "c", ApprovedModels: []string{"a", "b"}},
			want: "a,b,c",
		},
		{
			name: "no default falls back to the first approved model",
			rec:  PiProviderRec{ApprovedModels: []string{"b", "a"}},
			want: "b,a",
		},
		{
			name: "no default falls back to the first catalog entry",
			rec:  PiProviderRec{Models: []string{"catalog-first", "catalog-second"}},
			want: "catalog-first",
		},
		{
			name: "ids are trimmed and de-duplicated",
			rec:  PiProviderRec{DefaultModel: " a ", ApprovedModels: []string{" a ", "b", "b", "  "}},
			want: "a,b",
		},
		{
			name:  "nothing configured resolves to nothing",
			rec:   PiProviderRec{ID: "empty"},
			unset: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.rec.AllowedModelIDs()
			if tc.unset {
				if len(got) != 0 {
					t.Fatalf("allowed %v want empty", got)
				}
				return
			}
			if strings.Join(got, ",") != tc.want {
				t.Fatalf("allowed %v want %s", got, tc.want)
			}
		})
	}
	if got := (*PiProviderRec)(nil).AllowedModelIDs(); got != nil {
		t.Fatalf("nil record: %v", got)
	}
}

func TestPiProviderDefaultModelID(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  PiProviderRec
		want string
	}{
		{"configured default wins", PiProviderRec{DefaultModel: "a", ApprovedModels: []string{"b"}, Models: []string{"c"}}, "a"},
		{"approved before catalog", PiProviderRec{ApprovedModels: []string{"b"}, Models: []string{"c"}}, "b"},
		{"catalog last", PiProviderRec{Models: []string{"", "c"}}, "c"},
		{"nothing", PiProviderRec{ID: "x"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rec.DefaultModelID(); got != tc.want {
				t.Fatalf("default %q want %q", got, tc.want)
			}
		})
	}
	if got := (*PiProviderRec)(nil).DefaultModelID(); got != "" {
		t.Fatalf("nil record: %q", got)
	}
}

// The approved list is copied with the record, like the catalog: a caller that
// keeps the stack copy must not be able to mutate stored state.
func TestPiProviderStackCopiesApprovedModels(t *testing.T) {
	st := workerTestStore(t)
	if err := st.UpsertPiProviderActive(PiProviderRec{ID: "default", BaseURL: "https://api.example.test/v1", DefaultModel: "a", ApprovedModels: []string{"a", "b"}}); err != nil {
		t.Fatal(err)
	}
	stack := st.PiProviderStack()
	if len(stack) != 1 {
		t.Fatalf("stack %+v", stack)
	}
	stack[0].ApprovedModels[0] = "mutated"
	again := st.PiProviderStack()
	if again[0].ApprovedModels[0] != "a" {
		t.Fatalf("stored list mutated: %v", again[0].ApprovedModels)
	}
}
