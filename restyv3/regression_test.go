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
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"resty.dev/v3"
)

func TestURLCredentialsAreRedacted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		assert.True(t, ok)
		assert.Equal(t, "test-user", username)
		assert.Equal(t, "test-password", password)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	rec, provider := newRecorder()
	cli := resty.New()
	defer cli.Close()
	TraceClient(cli, WithTracerProvider(provider))
	u, err := url.Parse(srv.URL + "/some/path")
	require.NoError(t, err)
	u.User = url.UserPassword("test-user", "test-password")
	_, err = cli.R().Get(u.String())
	require.NoError(t, err)
	attrs := attrMap(requireChain(t, rec, 1)[0])
	want := strings.Replace(u.String(), "test-user:test-password", "REDACTED:REDACTED", 1)
	assert.Equal(t, want, attrs["resty.url"].AsString())
	assert.Equal(t, want, attrs["url.full"].AsString())
}

func TestNestedRequestPreservesParentAttempt(t *testing.T) {
	for _, scenario := range []string{"traced", "skipped", "error before tracing"} {
		t.Run(scenario, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/outer" {
					w.WriteHeader(http.StatusInternalServerError)
				} else {
					w.WriteHeader(http.StatusNoContent)
				}
			}))
			defer srv.Close()
			rec, provider := newRecorder()
			cli := resty.New()
			defer cli.Close()
			boom := errors.New("inner request failed before tracing")
			if scenario == "error before tracing" {
				cli.AddRequestMiddleware(func(_ *resty.Client, r *resty.Request) error {
					if strings.HasSuffix(r.URL, "/inner") {
						return boom
					}
					return nil
				})
			}
			TraceClient(cli, WithTracerProvider(provider), WithSkipper(func(r *resty.Request) bool {
				return scenario == "skipped" && strings.HasSuffix(r.URL, "/inner")
			}))
			cli.AddRequestMiddleware(func(_ *resty.Client, r *resty.Request) error {
				if !strings.HasSuffix(r.URL, "/outer") {
					return nil
				}
				parent := trace.SpanFromContext(r.Context())
				_, err := cli.R().SetContext(r.Context()).Get(srv.URL + "/inner")
				if scenario == "error before tracing" {
					assert.ErrorIs(t, err, boom)
				} else {
					assert.NoError(t, err)
				}
				assert.True(t, parent.IsRecording(), "the inner request must not end the parent")
				return nil
			})
			_, err := cli.R().Get(srv.URL + "/outer")
			require.NoError(t, err)
			n := 1
			if scenario == "traced" {
				n = 2
			}
			spans := requireChain(t, rec, n)
			outer := spans[len(spans)-1]
			assert.Equal(t, codes.Error, outer.Status().Code)
			assert.Empty(t, outer.Events())
			assert.Equal(t, srv.URL+"/outer", attrMap(outer)["url.full"].AsString())
			assert.Equal(t, int64(500), attrMap(outer)["http.response.status_code"].AsInt64())
		})
	}
}

func TestIgnoredErrorsOnRetriedAttempts(t *testing.T) {
	srv := redirectServer()
	defer srv.Close()
	for _, replaceHooks := range []bool{false, true} {
		t.Run(map[bool]string{false: "client hooks", true: "request hooks"}[replaceHooks], func(t *testing.T) {
			rec, provider := newRecorder()
			cli := noRedirectClient().SetRetryCount(1).SetRetryWaitTime(time.Millisecond).SetRetryMaxWaitTime(time.Millisecond).
				AddRetryConditions(func(_ *resty.Response, err error) bool { return err != nil })
			defer cli.Close()
			TraceClient(cli, WithTracerProvider(provider), WithIgnoredErrors(errRedirectDisabled))
			req := cli.R()
			if replaceHooks {
				req.SetRetryHooks(func(*resty.Response, error) {})
			}
			_, err := req.Get(srv.URL)
			require.ErrorIs(t, err, errRedirectDisabled)
			for _, span := range requireChain(t, rec, 2) {
				assert.Equal(t, codes.Unset, span.Status().Code)
				assert.Empty(t, span.Events())
				assert.NotContains(t, attrMap(span), "error.type")
			}
		})
	}
}

func TestResponseMiddlewareErrorsOnRetriedAttempts(t *testing.T) {
	srv := httptest.NewServer(testHandler())
	defer srv.Close()
	boom := errors.New("response middleware failed")
	for _, ignore := range []bool{false, true} {
		t.Run(map[bool]string{false: "record", true: "ignore"}[ignore], func(t *testing.T) {
			rec, provider := newRecorder()
			cli := newRetryClient(1)
			defer cli.Close()
			cli.AddResponseMiddleware(func(*resty.Client, *resty.Response) error { return boom })
			TraceClient(cli, WithTracerProvider(provider), WithErrorIgnorer(func(err error) bool { return ignore && errors.Is(err, boom) }))
			_, err := cli.R().Get(srv.URL)
			require.ErrorIs(t, err, boom)
			for _, span := range requireChain(t, rec, 2) {
				if ignore {
					assert.Equal(t, codes.Unset, span.Status().Code)
					assert.Empty(t, span.Events())
				} else {
					assert.Equal(t, codes.Error, span.Status().Code)
					require.Len(t, span.Events(), 1)
					assert.Equal(t, "response middleware failed", span.Status().Description)
				}
			}
		})
	}
}

func TestErrorAfterRetryHookIsRecorded(t *testing.T) {
	srv := httptest.NewServer(testHandler())
	defer srv.Close()
	boom := errors.New("retry strategy failed")
	for _, scenario := range []string{"canceled", "strategy error"} {
		t.Run(scenario, func(t *testing.T) {
			rec, provider := newRecorder()
			cli := newRetryClient(1).AddRetryConditions(func(*resty.Response, error) bool { return true })
			defer cli.Close()
			TraceClient(cli, WithTracerProvider(provider))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := boom
			cli.SetRetryDelayStrategy(func(*resty.Response, error) (time.Duration, error) {
				if scenario == "canceled" {
					cancel()
					return time.Hour, nil
				}
				return 0, boom
			})
			if scenario == "canceled" {
				want = context.Canceled
			}
			_, err := cli.R().SetContext(ctx).Get(srv.URL)
			require.ErrorIs(t, err, want)
			span := requireChain(t, rec, 1)[0]
			assert.Equal(t, codes.Error, span.Status().Code)
			assert.Contains(t, span.Status().Description, want.Error())
			require.Len(t, span.Events(), 1)
		})
	}
}

func TestTransportErrorsOnRetriedAttempts(t *testing.T) {
	ignored := errors.New("ignored transport error")
	other := errors.New("unrelated transport error")
	for _, cause := range []error{ignored, other} {
		t.Run(cause.Error(), func(t *testing.T) {
			rec, provider := newRecorder()
			transport := &failingTransport{err: cause}
			cli := newRetryClient(1).SetTransport(transport)
			defer cli.Close()
			TraceClient(cli, WithTracerProvider(provider), WithErrorIgnorer(func(err error) bool {
				var httpErr *url.Error
				return errors.As(err, &httpErr) && errors.Is(err, ignored)
			}))
			_, err := cli.R().Get("http://example.test/resource")
			require.ErrorIs(t, err, cause)
			for _, span := range requireChain(t, rec, 2) {
				if cause == ignored {
					assert.Equal(t, codes.Unset, span.Status().Code)
					assert.Empty(t, span.Events())
				} else {
					assert.Equal(t, codes.Error, span.Status().Code)
					require.Len(t, span.Events(), 1)
					assert.Contains(t, span.Status().Description, cause.Error())
				}
			}
			cli.Client().CloseIdleConnections()
			assert.True(t, transport.closed, "the underlying transport still closes idle connections")
		})
	}
}

type failingTransport struct {
	err    error
	closed bool
}

func (t *failingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, t.err }
func (t *failingTransport) CloseIdleConnections()                           { t.closed = true }

func TestRetrySpanDurationExcludesBackoff(t *testing.T) {
	rec, provider := newRecorder()
	cli := newRetryClient(1).SetTransport(&failingTransport{err: errors.New("transport failed")})
	defer cli.Close()
	TraceClient(cli, WithTracerProvider(provider))
	var retryHookTime time.Time
	cli.AddRetryHooks(func(*resty.Response, error) { retryHookTime = time.Now() })
	_, err := cli.R().Get("http://example.test/resource")
	require.Error(t, err)
	spans := requireChain(t, rec, 2)
	assert.False(t, spans[0].EndTime().After(retryHookTime), "retry end must precede backoff")
	assert.True(t, spans[1].StartTime().After(retryHookTime))
	require.Len(t, spans[0].Events(), 1)
	assert.False(t, spans[0].Events()[0].Time.After(spans[0].EndTime()))
}

func TestTransportTLSConfigurationIsPreserved(t *testing.T) {
	srv := httptest.NewTLSServer(testHandler())
	defer srv.Close()
	rec, provider := newRecorder()
	cli := resty.NewWithClient(srv.Client())
	defer cli.Close()
	TraceClient(cli, WithTracerProvider(provider))
	cfg := cli.TLSClientConfig().Clone()
	cli.SetTLSClientConfig(cfg)
	assert.Same(t, cfg, cli.TLSClientConfig())
	_, err := cli.R().Get(srv.URL)
	require.NoError(t, err)
	assert.Equal(t, codes.Unset, requireChain(t, rec, 1)[0].Status().Code)
}

func TestUseLastResponseRedirectPolicyIsPreserved(t *testing.T) {
	srv := redirectServer()
	defer srv.Close()
	rec, provider := newRecorder()
	cli := resty.New().SetRedirectPolicy(resty.RedirectPolicyFunc(func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }))
	defer cli.Close()
	TraceClient(cli, WithTracerProvider(provider))
	res, err := cli.R().Get(srv.URL)
	require.NoError(t, err)
	assert.Equal(t, http.StatusFound, res.StatusCode())
	span := requireChain(t, rec, 1)[0]
	assert.Equal(t, codes.Unset, span.Status().Code)
	assert.Empty(t, span.Events())
}
