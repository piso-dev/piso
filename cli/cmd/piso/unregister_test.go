package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUnregisterWorkerWithGateway(t *testing.T) {
	for _, status := range []int{200, 404, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/workers/piso-worker-demo" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(status)
			}))
			defer srv.Close()
			err := unregisterWorkerWithGateway(srv.URL, "piso-worker-demo")
			if status == 200 && err != nil {
				t.Fatal(err)
			}
			if status != 200 && (err == nil || !strings.Contains(err.Error(), "gateway")) {
				t.Fatalf("error not surfaced: %v", err)
			}
		})
	}
}
