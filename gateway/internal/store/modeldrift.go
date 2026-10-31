package store

import (
	"sort"
	"strings"
	"time"
)

// Model drift.
//
// A provider's configured model ids (its default, its approved list, a
// worker's pinned model) are compared against the last catalog the gateway
// successfully fetched. The comparison is a report, never a mutation: nothing
// here edits a provider or prunes an approved list. A model that left the
// catalog may be a deprecation, a rename, or a bad fetch, and only a human can
// tell which — so the answer is always "not in the catalog checked N ago",
// never "invalid".
//
// The distinction that makes this usable is missing versus unknown. A fetch
// failure, an unfetched catalog, and a local (tinfoil) provider all mean the
// question was not answered, and rendering that as drift would paint a working
// provider red. Only a catalog we hold, with no recorded error, and that is not
// suspect, can declare an id missing.

// Model drift states. These are part of the dashboard contract.
const (
	// ModelStateOK is an id the checked catalog lists.
	ModelStateOK = "ok"
	// ModelStateMissing is an id absent from a catalog we trust.
	ModelStateMissing = "missing"
	// ModelStateUnknown is an id we could not check: no catalog yet, the last
	// fetch failed, the provider's catalog is local, or the catalog looks
	// truncated. Not a verdict.
	ModelStateUnknown = "unknown"
)

// ModelDrift is one configured model id judged against the catalog.
type ModelDrift struct {
	// Role says why this id matters: "default", "approved", or "pinned".
	Role  string `json:"role"`
	Model string `json:"model"`
	State string `json:"state"`
	// Suggested is the catalog id that differs from Model only by case, when
	// there is exactly one. A renamed case is the most common "deleted" model,
	// and naming it saves a hunt.
	Suggested string `json:"suggested,omitempty"`
}

// Drifted reports whether the row is a real absence.
func (d ModelDrift) Drifted() bool { return d.State == ModelStateMissing }

// ModelDriftReport is everything the dashboard needs to describe one provider's
// drift, and nothing it has to recompute.
type ModelDriftReport struct {
	// Checked is the catalog the verdicts came from. Absent means unknown.
	CheckedAt string `json:"checkedAt,omitempty"`
	Catalog   int    `json:"catalog"`
	// Error and Suspect explain why states are unknown; both keep the card
	// informational instead of alarming.
	Error   string `json:"error,omitempty"`
	Suspect bool   `json:"suspect,omitempty"`
	// Items covers the default and every approved id, in stored order.
	Items []ModelDrift `json:"items,omitempty"`
	// Missing and Total count Items, so a card can say "2 of 5 are gone"
	// without walking the list.
	Missing int `json:"missing"`
	Total   int `json:"total"`
}

// ModelDriftFor judges the provider's default and approved ids. pinned adds
// the ids a worker override may pin, which need the same verdict but live
// outside the provider record. pinned is keyed by role label so a caller can
// render several workers' pins against one provider.
//
// A nil provider yields an empty report: there is nothing to be drifted from.
func ModelDriftFor(p *PiProviderRec, pinned map[string]string) ModelDriftReport {
	report := ModelDriftReport{}
	if p == nil {
		return report
	}
	ids := p.AllowedModelIDs()
	report.Total = len(ids)
	if !p.CatalogCheckedAt.IsZero() {
		report.CheckedAt = p.CatalogCheckedAt.UTC().Format(time.RFC3339)
	}
	report.Catalog = len(p.Models)
	report.Error = p.CatalogError
	report.Suspect = p.CatalogSuspect

	index := catalogIndex(p.Models)
	// trusted is the one gate between "we asked" and "we know": without a
	// fetched catalog, or with an error or a truncated one, every id is
	// unknown and nothing is reported as gone.
	trusted := !p.CatalogCheckedAt.IsZero() && p.CatalogError == "" && !p.CatalogSuspect && len(p.Models) > 0

	judge := func(role, model string) ModelDrift {
		item := ModelDrift{Role: role, Model: model, State: ModelStateUnknown}
		if !trusted {
			return item
		}
		if index[model] {
			item.State = ModelStateOK
			return item
		}
		item.State = ModelStateMissing
		item.Suggested = suggestModelID(p.Models, model)
		return item
	}

	def := p.DefaultModelID()
	for _, id := range ids {
		role := "approved"
		switch id {
		case p.DefaultModel:
			role = "default"
		}
		item := judge(role, id)
		if item.Drifted() {
			report.Missing++
		}
		report.Items = append(report.Items, item)
	}
	// A default resolved from the approved list or the catalog is already
	// covered above; one that arrived some other way (an explicit default
	// outside the list, which is saved with a warning) still needs a verdict.
	if def != "" {
		covered := false
		for _, item := range report.Items {
			if item.Model == def {
				covered = true
				break
			}
		}
		if !covered {
			item := judge("default", def)
			if item.Drifted() {
				report.Missing++
			}
			report.Total++
			report.Items = append([]ModelDrift{item}, report.Items...)
		}
	}
	for _, role := range sortedDriftRoles(pinned) {
		model := strings.TrimSpace(pinned[role])
		if model == "" {
			continue
		}
		item := judge(role, model)
		report.Items = append(report.Items, item)
	}
	return report
}

// MarkUnknown clears every verdict and reports the whole catalog as unchecked.
// Used for catalogs this gateway cannot fetch at all (a provider whose models
// are served inside the worker), where "not in the list" would be an
// accusation rather than an observation.
func (r *ModelDriftReport) MarkUnknown() {
	if r == nil {
		return
	}
	r.Missing = 0
	for i := range r.Items {
		r.Items[i].State = ModelStateUnknown
		r.Items[i].Suggested = ""
	}
}

// ModelDriftState judges a single id, for callers that already have a verdict
// for the provider and only need one worker's pin. Report rows win over a
// fresh comparison so a row and its provider card cannot disagree.
func ModelDriftState(report ModelDriftReport, model string) ModelDrift {
	model = strings.TrimSpace(model)
	for _, item := range report.Items {
		if item.Model == model {
			return item
		}
	}
	return ModelDrift{Role: "pinned", Model: model, State: ModelStateUnknown}
}

// sortedDriftRoles orders the pinned roles so a report is stable across
// renders.
func sortedDriftRoles(pinned map[string]string) []string {
	roles := make([]string, 0, len(pinned))
	for role := range pinned {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	return roles
}

// catalogIndex builds the membership set a verdict reads. Exact match only: the
// pin is exact, so drift detection has to be exact too, or a model the pin
// would refuse would read as present here.
func catalogIndex(models []string) map[string]bool {
	index := make(map[string]bool, len(models))
	for _, m := range models {
		if m = strings.TrimSpace(m); m != "" {
			index[m] = true
		}
	}
	return index
}

// suggestModelID finds a catalog id that differs from model only by case or by
// surrounding space. Ambiguity (two candidates) returns "", because a guess
// that picks the wrong one is worse than no guess.
func suggestModelID(models []string, model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	found := ""
	for _, m := range models {
		m = strings.TrimSpace(m)
		if m == "" || m == model || !strings.EqualFold(m, model) {
			continue
		}
		if found != "" {
			return ""
		}
		found = m
	}
	return found
}
