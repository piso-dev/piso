package proxy

import "crypto/tls"

// tlsCert is the minimal shape the CA caches (a tls.Certificate without the
// parsed leaf; crypto/tls only needs the DER chain + private key).
type tlsCert struct {
	Certificate [][]byte
	PrivateKey  any
}

func (c tlsCert) toTLS() tls.Certificate {
	return tls.Certificate{Certificate: c.Certificate, PrivateKey: c.PrivateKey}
}
