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

package restyv3

import (
	"crypto/tls"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"resty.dev/v3"
)

// captureClientErrors preserves errors that Resty rc.4 clears before retry
// hooks run. It delegates to the client's configured transport and policy.
func captureClientErrors(client *resty.Client) {
	base := client.Transport()
	if base == nil {
		base = http.DefaultTransport
	}
	transport := &errorCapturingTransport{base: base}
	switch base := base.(type) {
	case resty.TLSClientConfiger:
		client.SetTransport(&errorCapturingTLSTransport{errorCapturingTransport: transport, TLSClientConfiger: base})
	case *http.Transport:
		client.SetTransport(&errorCapturingHTTPTransport{errorCapturingTransport: transport, transport: base})
	default:
		client.SetTransport(transport)
	}

	hc := client.Client()
	policy := hc.CheckRedirect
	hc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		var err error
		if policy != nil {
			err = policy(req, via)
		} else if len(via) >= 10 {
			err = errors.New("stopped after 10 redirects")
		}
		// ErrUseLastResponse returns a successful response from http.Client.Do.
		if err != nil && err != http.ErrUseLastResponse && len(via) > 0 {
			captureHTTPError(via[0], err)
		}
		return err
	}
}

type errorCapturingTransport struct {
	base http.RoundTripper
}

func (t *errorCapturingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := t.base.RoundTrip(req)
	captureHTTPError(req, err)
	return res, err
}

func (t *errorCapturingTransport) CloseIdleConnections() {
	if closer, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// Retain Resty's TLS configuration interface when the original transport
// supports it, including its standard *http.Transport.
type errorCapturingTLSTransport struct {
	*errorCapturingTransport
	resty.TLSClientConfiger
}

type errorCapturingHTTPTransport struct {
	*errorCapturingTransport
	transport *http.Transport
}

func (t *errorCapturingHTTPTransport) TLSClientConfig() *tls.Config {
	if t.transport.TLSClientConfig == nil {
		t.transport.TLSClientConfig = &tls.Config{}
	}
	return t.transport.TLSClientConfig
}

func (t *errorCapturingHTTPTransport) SetTLSClientConfig(cfg *tls.Config) error {
	t.transport.TLSClientConfig = cfg
	return nil
}

func captureHTTPError(req *http.Request, err error) {
	if err == nil {
		return
	}
	a, _ := req.Context().Value(attemptKey{}).(*attempt)
	if a == nil {
		return
	}
	// Match the url.Error wrapping used by http.Client, with credentials redacted.
	method := strings.ToLower(req.Method)
	if len(method) > 0 {
		method = strings.ToUpper(method[:1]) + method[1:]
	}
	a.captureError(&url.Error{Op: method, URL: redactedUserinfo(req.URL), Err: err})
}
