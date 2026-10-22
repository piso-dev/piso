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
	// v1→v2: worker registry (slug↔IP) added. Older files have no "workers"
	// field (nil slice); normalize so writers don't emit nulls.
	if st.Version < 2 {
		if st.Workers == nil {
			st.Workers = []WorkerRec{}
		}
		st.Version = 2
		changed = true
	}
	// v2→v3: worker log-label context added. Older files lack the field.
	if st.Version < 3 {
		if st.Contexts == nil {
			st.Contexts = []WorkerCtx{}
		}
		st.Version = 3
		changed = true
	}
	// v3→v4: route provenance + live-tracking fields added. Routes that
	// existed before the worker ports watcher were all host-created
	// (`piso expose` / dashboard), so they default to origin "expose".
	if st.Version < 4 {
		if st.Routes == nil {
			st.Routes = []RouteRec{}
		}
		for i, r := range st.Routes {
			if r.Origin == "" {
				st.Routes[i].Origin = AutoRouteOriginExpose
			}
		}
		st.Version = 4
		changed = true
	}
	// v4→v5: gateway-brokered MCP OAuth rows.
	if st.Version < 5 {
		if st.McpServers == nil {
			st.McpServers = []McpServerRec{}
		}
		st.Version = 5
		changed = true
	}
	// v5→v6: GitHub App rows (PEM + installations).
	if st.Version < 6 {
		if st.GithubApps == nil {
			st.GithubApps = []GithubAppRec{}
		}
		st.Version = 6
		changed = true
	}
	// v6→v7: shared pi provider pointer. Older files omit the field (nil).
	if st.Version < 7 {
		st.Version = 7
		changed = true
	}
	// v7→v8: MCP channel key binding on WorkerRec. Older files omit the
	// fields; the zero value means unbound.
	if st.Version < 8 {
		st.Version = 8
		changed = true
	}
	// v8→v9: provider stack. The single pi provider pointer becomes the first
	// profile of the stack, with ID "piso". Later settings land on the stack
	// fields; the legacy pointer is cleared after conversion.
	if st.Version < 9 {
		if st.PiProvider != nil {
			rec := *st.PiProvider
			if rec.ID == "" {
				rec.ID = "piso"
			}
			if rec.Name == "" {
				rec.Name = "Piso"
			}
			replaced := false
			for i := range st.PiProviders {
				if st.PiProviders[i].ID == rec.ID {
					st.PiProviders[i] = rec
					replaced = true
					break
				}
			}
			if !replaced {
				st.PiProviders = append(st.PiProviders, rec)
			}
			if st.ActivePiProviderID == "" {
				st.ActivePiProviderID = rec.ID
			}
			st.PiProvider = nil
		}
		st.Version = 9
		changed = true
	}
	// v9→v10: destination binding became mandatory. Before this, an empty
	// AllowedHosts meant "any host", so every such secret got a "*" rule at
	// creation. Materialize that as an explicit "*" host list instead of
	// letting the new rule (empty = inert) silently unbind secrets that are
	// substituting right now. Only secrets that actually had a "*" rule are
	// converted: a secret with no rule was never substitutable and stays
	// unbound, which is the state the new UI asks the operator to fix.
	if st.Version < 10 {
		wildcard := map[string]bool{}
		for _, r := range st.Rules {
			if r.Host == "*" && r.Placeholder != "" {
				wildcard[r.Placeholder] = true
			}
		}
		for i, sec := range st.Secrets {
			if sec.GatewayOnly || sec.Revoked || len(CanonicalRuleHosts(sec.AllowedHosts)) > 0 {
				continue
			}
			if !wildcard[sec.Placeholder] {
				continue
			}
			st.Secrets[i].AllowedHosts = []string{"*"}
			changed = true
		}
		st.Version = 10
		changed = true
	}
	// v10→v11: per-worker model overrides. Older files simply have no rows,
	// which means every worker follows the account default.
	if st.Version < 11 {
		if st.ModelOverrides == nil {
			st.ModelOverrides = []ModelOverrideRec{}
		}
		st.Version = 11
		changed = true
	}
	return changed
}
