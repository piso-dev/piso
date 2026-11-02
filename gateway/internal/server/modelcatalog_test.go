package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"piso/gateway/internal/store"
)

func TestWorkerDiscoveredCatalogIsAdvisory(t *testing.T) {
	s := overrideTestServer(t)
	p := store.PiProviderRec{ID: "piso-tinfoil", SecretID: "sec_aiml", BaseURL: tinfoilProxyBaseURL, Kind: piProviderKindTinfoil}
	if err := s.Store.UpsertPiProvider(p); err != nil {
		t.Fatal(err)
	}
	if rec := putOverride(t, s, "demo", p.ID); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	view := s.resolveWorkerModel("demo")
	if _, present := view.Providers[p.ID]; present {
		t.Fatal("unknown local catalog published as denial")
	}
	if view.ModelPolicies[p.ID].Mode != "worker-discovered" {
		t.Fatalf("policy: %+v", view)
	}
	post := func(ip, provider, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/worker/model/catalog", strings.NewReader(`{"worker":"piso-worker-demo","provider":"`+provider+`","baseUrl":"`+p.BaseURL+`",`+body+`}`))
		r.RemoteAddr = ip + ":1234"
		w := httptest.NewRecorder()
		s.WorkerHandler().ServeHTTP(w, r)
		return w
	}
	if rec := post("192.168.107.50", p.ID, `"models":[" z ","a","a"]`); rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	catalog := s.localCatalog(&p)
	if strings.Join(catalog.Models, ",") != "a,z" || len(catalog.Reports) != 1 || catalog.Reports[0].SuccessfulAt.IsZero() {
		t.Fatalf("catalog: %+v", catalog)
	}
	saved := s.Store.PiProviderByID(p.ID)
	if len(saved.Models) != 0 || saved.DefaultModelID() != "" || len(saved.AllowedModelIDs()) != 0 {
		t.Fatalf("report changed policy: %+v", saved)
	}
	if rec := post("192.168.107.51", p.ID, `"models":["a"]`); rec.Code != 403 {
		t.Fatal("unregistered report accepted", rec.Code)
	}
	if rec := post("192.168.107.50", "piso-aiml", `"models":["a"]`); rec.Code != 400 {
		t.Fatal("remote report accepted", rec.Code)
	}
	if rec := post("192.168.107.50", p.ID, `"models":["bad\nmodel"]`); rec.Code != 400 {
		t.Fatal("control character accepted", rec.Code)
	}
	if rec := post("192.168.107.50", p.ID, `"failed":true,"models":[]`); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	catalog = s.localCatalog(&p)
	if len(catalog.Models) != 2 || catalog.Reports[0].Error == "" {
		t.Fatalf("lost last success: %+v", catalog)
	}
	// Manual IDs need no gateway-visible catalog, and reports cannot trigger drift.
	w := httptest.NewRecorder()
	s.handlePutModelOverride(w, httptest.NewRequest(http.MethodPut, "/api/v1/model-overrides", strings.NewReader(`{"slug":"demo","providerId":"piso-tinfoil","model":"manual"}`)))
	if w.Code != 200 || s.resolveWorkerModel("demo").Model != "manual" {
		t.Fatal(w.Code, w.Body.String())
	}
	p.ApprovedModels = []string{"a"}
	if err := s.Store.UpsertPiProvider(p); err != nil {
		t.Fatal(err)
	}
	view = s.resolveWorkerModel("demo")
	if view.ModelPolicies[p.ID].Mode != "restricted" {
		t.Fatalf("policy: %+v", view)
	}
	if s.pinnedModelDrift(&p, "manual") == "" {
		t.Fatal("approval did not restrict manual ID")
	}
	// Changed scope or endpoint invalidates suggestions, without deleting history.
	p.SecretID = "sec_scoped"
	if err := s.Store.UpsertPiProvider(p); err != nil {
		t.Fatal(err)
	}
	if len(s.localCatalog(&p).Models) != 0 {
		t.Fatal("stale secret report exposed")
	}
	if s.resolveWorkerModel("demo").ModelPolicies[p.ID].Mode != "unavailable" {
		t.Fatal("missing grant not unavailable")
	}
	if rec := post("192.168.107.50", p.ID, `"models":["a"]`); rec.Code != 403 {
		t.Fatal("ungranted report accepted", rec.Code)
	}
	// Stored per-worker timestamps survive restart/save format.
	raw, err := json.Marshal(s.Store.WorkerModelCatalogs(p.ID))
	if err != nil || !strings.Contains(string(raw), "successfulAt") {
		t.Fatal(string(raw), err)
	}
}
