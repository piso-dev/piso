package store

// applyMigrations upgrades st in place to CurrentStateVersion.
// It returns true when the on-disk file must be rewritten.
func applyMigrations(st *State) bool {
	if st == nil {
		return false
	}
	changed := false
	// v0: files written before Version existed (JSON zero). Normalize nil
	// slices so later writers do not emit nulls, then stamp the file current.
	if st.Version < 1 {
		if st.Secrets == nil {
			st.Secrets = []SecretRec{}
		}
		if st.Rules == nil {
			st.Rules = []RuleRec{}
		}
		if st.Domains == nil {
			st.Domains = []DomainRec{}
		}
		if st.Exceptions == nil {
			st.Exceptions = []ExceptionRec{}
		}
		if st.Routes == nil {
			st.Routes = []RouteRec{}
		}
		st.Version = 1
		changed = true
	}
	return changed
}
