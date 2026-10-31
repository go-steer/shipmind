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
	"errors"
	"net/http"

	"github.com/go-steer/purser/authn/mtls"
	"github.com/go-steer/purser/client"
)

// RouterServerName is the name in the router's serving certificate.
const RouterServerName = "atenet-router.ate-system.svc"

// RouterTLS locates the files a router client presents and trusts when
// the router requires mTLS (Substrate v1.0): a pod's podidentity
// certificate and the cluster's servicedns trust bundle, both projected
// volumes that the kubelet rotates in place.
type RouterTLS struct {
	// CredentialBundle is the podCertificate projection's
	// credentialBundlePath: the private key, then the chain.
	CredentialBundle string
	// TrustBundle is the clusterTrustBundle projection of the router's
	// signer.
	TrustBundle string
	// ServerName is verified against the router's certificate; empty
	// means RouterServerName. Set it when the URL's host differs, as it
	// does through a tunnel.
	ServerName string
}

// Client returns an HTTP client for the router that presents the
// certificate and verifies the router against the trust bundle. Both
// files are re-read as they rotate until Close is called.
func (t RouterTLS) Client() (*http.Client, func() error, error) {
	if t.CredentialBundle == "" || t.TrustBundle == "" {
		return nil, nil, errors.New("router mTLS: want both a credential bundle and a trust bundle")
	}
	certs, err := mtls.NewCertificateFileSource(mtls.CertificateFileOptions{CredentialBundlePath: t.CredentialBundle})
	if err != nil {
		return nil, nil, err
	}
	pool, err := mtls.NewCertPoolFileSource(mtls.CertPoolFileOptions{Path: t.TrustBundle})
	if err != nil {
		return nil, nil, errors.Join(err, certs.Close())
	}
	closeAll := func() error { return errors.Join(certs.Close(), pool.Close()) }
	name := t.ServerName
	if name == "" {
		name = RouterServerName
	}
	cfg, err := client.NewPKI(client.PKIOptions{
		GetClientCertificate: certs.GetClientCertificate,
		RootCAsSource:        pool,
		ServerName:           name,
	})
	if err != nil {
		return nil, nil, errors.Join(err, closeAll())
	}
	return &http.Client{Transport: client.Transport(cfg), Timeout: routerTimeout}, closeAll, nil
}
