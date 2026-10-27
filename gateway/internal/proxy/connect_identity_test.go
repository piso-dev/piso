package proxy

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"

	"piso/gateway/internal/store"
	"piso/internal/piprofile"
	"testing"
	"time"
)

func TestConnectPreservesWorkerIdentity(t *testing.T) {
	for _, remote := range []bool{true, false} {
		t.Run(fmt.Sprintf("remote=%v", remote), func(t *testing.T) {
			h, st := testHandler(t)
			dir := t.TempDir()
			var err error
			h.CA, err = LoadCA(filepath.Join(dir, "ca.pem"), filepath.Join(dir, "key.pem"))
			if err != nil {
				t.Fatal(err)
			}
			// A manually configured slug must work without fetching a catalog.
			if err := st.UpsertPiProviderActive(store.PiProviderRec{ID: "piso-manual", BaseURL: "https://93.184.216.34/v1", DefaultModel: "chosen"}); err != nil {
				t.Fatal(err)
			}
			if err := piprofile.WriteProviderStack(dir, []piprofile.Provider{{ID: "piso-manual", BaseURL: "https://93.184.216.34/v1", APIKey: "piso_example", Models: []string{"chosen"}}}); err != nil {
				t.Fatal(err)
			}
			profile, err := os.ReadFile(filepath.Join(dir, "models.json"))
			if err != nil || !strings.Contains(string(profile), `"id": "chosen"`) {
				t.Fatalf("manual model missing: %s %v", profile, err)
			}
			h.Worker = func(r *http.Request) string {
				if !remote || r.Header.Get("Proxy-Authorization") == "Bearer test-worker-token" {
					return "worker-1"
				}
				return ""
			}
			seen := make(chan *http.Request, 2)
			h.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				seen <- r.Clone(r.Context())
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok")), ContentLength: 2}, nil
			})}
			srv := httptest.NewServer(h)
			defer srv.Close()
			conn, err := net.DialTimeout("tcp", strings.TrimPrefix(srv.URL, "http://"), 3*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(10 * time.Second))
			// A literal public IP avoids DNS or real internet access; origin forwarding
			// is intercepted by the transport above, not by relaxing destination policy.
			fmt.Fprint(conn, "CONNECT 93.184.216.34:443 HTTP/1.1\r\nHost: 93.184.216.34:443\r\nProxy-Authorization: Bearer test-worker-token\r\n\r\n")
			br := bufio.NewReader(conn)
			response, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != 200 {
				t.Fatalf("CONNECT: %s", response.Status)
			}
			roots := x509.NewCertPool()
			roots.AddCert(h.CA.cert)
			tunnel := tls.Client(&hijackedConn{Conn: conn, br: br}, &tls.Config{RootCAs: roots, ServerName: "93.184.216.34", MinVersion: tls.VersionTLS12})
			defer tunnel.Close()
			if err := tunnel.Handshake(); err != nil {
				t.Fatal(err)
			}
			responses := bufio.NewReader(tunnel)
			for i := 0; i < 2; i++ {
				body := `{"model":"chosen","messages":[]}`
				fmt.Fprintf(tunnel, "POST /v1/chat/completions HTTP/1.1\r\nHost: 93.184.216.34\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
				resp, err := http.ReadResponse(responses, &http.Request{Method: http.MethodGet})
				if err != nil {
					t.Fatal(err)
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 {
					t.Fatalf("origin status %d", resp.StatusCode)
				}
				req := <-seen
				for _, header := range []string{"Authorization", "Proxy-Authorization", "X-Piso-Worker-Token"} {
					if req.Header.Get(header) != "" {
						t.Fatalf("CONNECT credential leaked via %s", header)
					}
				}
			}
			records := st.Records(10)
			if len(records) != 2 {
				t.Fatalf("records: %d", len(records))
			}
			for _, rec := range records {
				if rec.Model != "chosen" {
					t.Fatalf("model not recorded: %+v", rec)
				}
				if rec.Worker != "worker-1" || rec.Slug != "worker-1" {
					t.Fatalf("lost identity: %+v", rec)
				}
			}
		})
	}
}
