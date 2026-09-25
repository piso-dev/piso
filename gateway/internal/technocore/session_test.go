package technocore

import "testing"

func TestHTTPToWS(t *testing.T) {
	if got := httpToWS("https://server.example"); got != "wss://server.example" {
		t.Fatalf("got %q", got)
	}
	if got := httpToWS("http://localhost:8000"); got != "ws://localhost:8000" {
		t.Fatalf("got %q", got)
	}
}
