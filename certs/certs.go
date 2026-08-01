// Package certs guarantees TLS trust roots on minimal devices. The
// reMarkable's OS may lack the CA bundle paths Go probes; without roots,
// every HTTPS request fails. Pool returns the system pool augmented with an
// embedded Mozilla bundle (cacert.pem from curl.se), so verification works
// either way.
package certs

import (
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"net/http"
	"sync"
)

//go:embed cacert.pem
var bundle []byte

var (
	once sync.Once
	pool *x509.CertPool
)

func Pool() *x509.CertPool {
	once.Do(func() {
		p, err := x509.SystemCertPool()
		if err != nil || p == nil {
			p = x509.NewCertPool()
		}
		p.AppendCertsFromPEM(bundle)
		pool = p
	})
	return pool
}

// Install points the default HTTP transport at the augmented pool. Call
// once at startup; all package-default HTTP clients then verify correctly.
func Install() {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		if t.TLSClientConfig == nil {
			t.TLSClientConfig = &tls.Config{}
		}
		t.TLSClientConfig.RootCAs = Pool()
	}
}
