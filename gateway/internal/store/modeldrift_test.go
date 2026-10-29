package store

import (
	"testing"
	"time"
)

// checked is a provider whose catalog was fetched successfully.
func checked(models ...string) *PiProviderRec {
	return &PiProviderRec{
		ID:               "piso-one",
		DefaultModel:     models[0],
		Models:           models,
		CatalogCheckedAt: time.Now().UTC(),
	}
}

func states(report ModelDriftReport) map[string]string {
	out := make(map[string]string, len(report.Items))
	for _, item := range report.Items {
		out[item.Role+":"+item.Model] = item.State
	}
	return out
}

func TestModelDriftIsReportOnly(t *testing.T) {
	p := checked("model-a", "model-b")
	p.ApprovedModels = []string{"model-a", "model-b", "model-gone"}
	before := append([]string(nil), p.ApprovedModels...)

	report := ModelDriftFor(p, nil)
	if report.Missing != 1 || report.Total != 3 {
		t.Fatalf("counts: missing=%d total=%d", report.Missing, report.Total)
	}
	if got := states(report); got["approved:model-gone"] != ModelStateMissing {
		t.Fatalf("model-gone: %q", got["approved:model-gone"])
	}
	if got := states(report); got["default:model-a"] != ModelStateOK {
		t.Fatalf("default: %q", got["default:model-a"])
	}
	// Reporting drift must not prune: a human decides whether a missing model
	// is a deprecation, a rename, or a bad fetch.
	if len(p.ApprovedModels) != len(before) {
		t.Fatalf("drift mutated the approved list: %v", p.ApprovedModels)
	}
}

// The gate that keeps a bad fetch from looking like mass drift.
func TestModelDriftUnknownWhenCatalogIsNotTrustworthy(t *testing.T) {
	base := func() *PiProviderRec {
		p := checked("model-a")
		p.ApprovedModels = []string{"model-a", "model-gone"}
		return p
	}

	noCatalog := base()
	noCatalog.Models = nil
	noCatalog.CatalogCheckedAt = time.Time{}

	failed := base()
	failed.CatalogError = "provider model catalog is unavailable"

	suspect := base()
	suspect.CatalogSuspect = true

	for name, p := range map[string]*PiProviderRec{
		"no catalog":   noCatalog,
		"failed fetch": failed,
		"truncated":    suspect,
	} {
		report := ModelDriftFor(p, nil)
		if report.Missing != 0 {
			t.Fatalf("%s: reported %d missing", name, report.Missing)
		}
		for _, item := range report.Items {
			if item.State != ModelStateUnknown {
				t.Fatalf("%s: %s/%s = %q", name, item.Role, item.Model, item.State)
			}
		}
	}
}

func TestModelDriftPinnedRole(t *testing.T) {
	p := checked("model-a", "model-b")
	p.ApprovedModels = []string{"model-a"}
	report := ModelDriftFor(p, map[string]string{"pinned": "model-b", "pinned-2": "model-gone"})
	got := states(report)
	if got["pinned:model-b"] != ModelStateOK {
		t.Fatalf("pinned model-b: %q", got["pinned:model-b"])
	}
	if got["pinned-2:model-gone"] != ModelStateMissing {
		t.Fatalf("pinned-2 model-gone: %q", got["pinned-2:model-gone"])
	}
	// A worker's pin is not an approved model, so it must not move the
	// provider's own counts.
	if report.Missing != 0 || report.Total != 1 {
		t.Fatalf("counts: missing=%d total=%d", report.Missing, report.Total)
	}
}

// A default saved outside the approved list is reported with the default role,
// so the card can point at the model the operator actually configured.
func TestModelDriftDefaultOutsideApprovedList(t *testing.T) {
	p := checked("model-a", "model-b")
	p.DefaultModel = "model-b"
	p.ApprovedModels = []string{"model-a"}
	report := ModelDriftFor(p, nil)
	got := states(report)
	if got["default:model-b"] != ModelStateOK {
		t.Fatalf("default: %q", got["default:model-b"])
	}
	if report.Total != 2 {
		t.Fatalf("total: %d", report.Total)
	}
}

func TestModelDriftSuggestsCaseRename(t *testing.T) {
	p := checked("model-a", "Model-Gone")
	p.ApprovedModels = []string{"model-gone"}
	report := ModelDriftFor(p, nil)
	var missing ModelDrift
	for _, item := range report.Items {
		if item.Drifted() {
			missing = item
		}
	}
	if missing.Model != "model-gone" || missing.Suggested != "Model-Gone" {
		t.Fatalf("items: %+v", report.Items)
	}
}

func TestModelDriftNilProvider(t *testing.T) {
	if report := ModelDriftFor(nil, map[string]string{"pinned": "model-a"}); report.Total != 0 || len(report.Items) != 0 {
		t.Fatalf("report: %+v", report)
	}
}

func TestSetPiProviderCatalogFlagsTruncated(t *testing.T) {
	st := workerTestStore(t)
	full := []string{"a", "b", "c", "d", "e", "f"}
	if err := st.UpsertPiProvider(PiProviderRec{ID: "piso-one", SecretID: "sec", BaseURL: "https://x/v1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetPiProviderCatalog("piso-one", full, ""); err != nil {
		t.Fatal(err)
	}
	// A response with a third of the models is not believed to be a
	// deprecation: it is what a reindexing endpoint returns.
	if err := st.SetPiProviderCatalog("piso-one", []string{"a", "b"}, ""); err != nil {
		t.Fatal(err)
	}
	p := st.PiProviderByID("piso-one")
	if p == nil || !p.CatalogSuspect {
		t.Fatalf("shrunken catalog not flagged: %+v", p)
	}
	if report := ModelDriftFor(p, nil); report.Missing != 0 {
		t.Fatalf("suspect catalog reported drift: %+v", report)
	}
	// The next fetch that agrees with it clears the flag, so a real mass
	// deprecation is reported one refresh later rather than never.
	if err := st.SetPiProviderCatalog("piso-one", []string{"a", "b"}, ""); err != nil {
		t.Fatal(err)
	}
	if p := st.PiProviderByID("piso-one"); p == nil || p.CatalogSuspect {
		t.Fatalf("suspect flag should clear on agreement: %+v", p)
	}
}
