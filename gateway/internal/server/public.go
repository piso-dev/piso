package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

// viaDirectKey marks a request that arrived on the public listener, not a
// relay splice. Workers cannot set this themselves.
type viaDirectKey struct{}

// PublicWorkerHandler is the standalone worker-facing mux: CONNECT goes to
// the MITM proxy, /api/v1/worker/* to the worker API, /ca.crt is the MITM CA.
// Unauthenticated internet clients cannot use it as an open proxy.
func (s *Server) PublicWorkerHandler() http.Handler {
	worker := s.WorkerHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ca.crt" && r.Method == http.MethodGet {
			if s.CA == nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/x-pem-file")
			_, _ = w.Write(s.CA.CertPEM())
			return
		}
		if !s.allowRemoteWorker(r) {
			if r.Method == http.MethodConnect {
				w.Header().Set("Proxy-Authenticate", "Bearer")
				writeJSON(w, http.StatusProxyAuthRequired, map[string]string{"error": "proxy auth required"})
				return
			}
			writeJSON(w, 401, map[string]string{"error": "unauthorized"})
			return
		}
		if r.Method == http.MethodConnect {
			s.Proxy.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/v1/worker/") {
			r2 := r.Clone(r.Context())
			r2.Header.Del("X-Piso-Worker-Via")
			ctx := context.WithValue(r2.Context(), viaDirectKey{}, true)
			worker.ServeHTTP(w, r2.WithContext(ctx))
			return
		}
		http.NotFound(w, r)
	})
}

func (s *Server) allowRemoteWorker(r *http.Request) bool {
	if s == nil || s.Store == nil {
		return false
	}
	if tok := workerBearerToken(r); tok != "" {
		_, ok := s.Store.WorkerByTokenHash(hashWorkerToken(tok))
		return ok
	}
	if rec, ok := s.Store.WorkerByIP(requestIP(r)); ok && !rec.Remote {
		return true
	}
	return false
}

func workerBearerToken(r *http.Request) string {
	for _, h := range []string{r.Header.Get("Authorization"), r.Header.Get("Proxy-Authorization")} {
		h = strings.TrimSpace(h)
		if len(h) >= 7 && strings.EqualFold(h[:7], "Bearer ") {
			return strings.TrimSpace(h[7:])
		}
	}
	return strings.TrimSpace(r.Header.Get("X-Piso-Worker-Token"))
}

func hashWorkerToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}
