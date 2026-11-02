package server

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"piso/gateway/internal/store"
)

func TestStreamRoutesReachEverySubscriber(t *testing.T) {
	s := testServer(t)
	ts := httptest.NewServer(http.HandlerFunc(s.stream))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	results := make(chan bool, 2)
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		go func() {
			sc := bufio.NewScanner(resp.Body)
			for sc.Scan() {
				if strings.HasPrefix(sc.Text(), "event: route") {
					results <- true
					return
				}
			}
			results <- false
		}()
	}
	if err := s.Store.AddRoute(store.RouteRec{ID: "test", Name: "site", Worker: "worker", Port: 3000}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if !<-results {
			t.Fatal("a connected SSE subscriber missed the route event")
		}
	}
}
