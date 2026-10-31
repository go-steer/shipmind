// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package scenario

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newCA(t *testing.T, name string) testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return testCA{cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

// issue returns a leaf signed by ca: its certificate PEM and PKCS#8 key PEM.
func (ca testCA) issue(t *testing.T, usage x509.ExtKeyUsage, dns []string, uris []*url.URL) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}, DNSNames: dns, URIs: uris,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	k, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: k})
}

func writeFile(t *testing.T, name string, parts ...[]byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, bytes.Join(parts, nil), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The client presents the pod's certificate and verifies the router by
// its service name, as a podidentity/servicedns pair would.
func TestRouterTLSClient(t *testing.T) {
	serverCA, podCA := newCA(t, "servicedns"), newCA(t, "podidentity")
	srvCert, srvKey := serverCA.issue(t, x509.ExtKeyUsageServerAuth, []string{RouterServerName}, nil)
	spiffe, _ := url.Parse("spiffe://cluster.local/ns/shipmind-e2e/sa/shipmind-e2e")
	podCert, podKey := podCA.issue(t, x509.ExtKeyUsageClientAuth, nil, []*url.URL{spiffe})

	pair, err := tls.X509KeyPair(srvCert, srvKey)
	if err != nil {
		t.Fatal(err)
	}
	clients := x509.NewCertPool()
	clients.AddCert(podCA.cert)
	var seen string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.TLS.PeerCertificates[0].URIs[0].String()
		_, _ = w.Write([]byte(`{"state":"idle"}`))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: clients, MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	defer srv.Close()

	bundle := writeFile(t, "credential-bundle.pem", podKey, podCert)
	trust := writeFile(t, "trust.pem", serverCA.pem)
	c, closeTLS, err := RouterTLS{CredentialBundle: bundle, TrustBundle: trust}.Client()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closeTLS() }()
	if c.Timeout != routerTimeout {
		t.Errorf("timeout %v", c.Timeout)
	}
	// httptest listens on 127.0.0.1, so the name verified must be the
	// router's, not the dialed host's.
	r := &Router{Base: srv.URL, Atespace: "agents", Token: "tok", Client: c}
	if state, err := r.State(t.Context(), "lookout-1", "s1"); err != nil || state != "idle" {
		t.Fatalf("State = %q, %v", state, err)
	}
	if seen != spiffe.String() {
		t.Errorf("router saw client %q", seen)
	}

	// A router whose certificate doesn't chain to the trust bundle is refused.
	other := writeFile(t, "other.pem", newCA(t, "other").pem)
	c2, close2, err := RouterTLS{CredentialBundle: bundle, TrustBundle: other}.Client()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = close2() }()
	if _, err := (&Router{Base: srv.URL, Atespace: "agents", Token: "tok", Client: c2}).State(t.Context(), "lookout-1", "s1"); err == nil {
		t.Error("an untrusted router was accepted")
	}
	// So is one presenting a certificate for another name.
	c3, close3, err := RouterTLS{CredentialBundle: bundle, TrustBundle: trust, ServerName: "other.ate-system.svc"}.Client()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = close3() }()
	if _, err := (&Router{Base: srv.URL, Atespace: "agents", Token: "tok", Client: c3}).State(t.Context(), "lookout-1", "s1"); err == nil {
		t.Error("a router certificate for another name was accepted")
	}
}

func TestRouterTLSErrors(t *testing.T) {
	ca := newCA(t, "ca")
	cert, key := ca.issue(t, x509.ExtKeyUsageClientAuth, nil, nil)
	bundle := writeFile(t, "b.pem", key, cert)
	trust := writeFile(t, "t.pem", ca.pem)
	missing := filepath.Join(t.TempDir(), "missing.pem")
	for _, tc := range []struct {
		name string
		rt   RouterTLS
		want string
	}{
		{"no trust", RouterTLS{CredentialBundle: bundle}, "want both"},
		{"no cert", RouterTLS{TrustBundle: trust}, "want both"},
		{"missing cert", RouterTLS{CredentialBundle: missing, TrustBundle: trust}, "missing.pem"},
		{"missing trust", RouterTLS{CredentialBundle: bundle, TrustBundle: missing}, "missing.pem"},
		{"trust not PEM", RouterTLS{CredentialBundle: bundle, TrustBundle: writeFile(t, "x.pem", []byte("nope"))}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := tc.rt.Client(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
