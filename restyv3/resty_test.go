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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/propagators/b3"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"resty.dev/v3"
)

func newRecorder() (*tracetest.SpanRecorder, *sdktrace.TracerProvider) {
	rec := tracetest.NewSpanRecorder()
	return rec, sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
}

func attrMap(s sdktrace.ReadOnlySpan) map[attribute.Key]attribute.Value {
	m := map[attribute.Key]attribute.Value{}
	for _, kv := range s.Attributes() {
		m[kv.Key] = kv.Value
	}
	return m
}

func testHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	})
}

// requireChain asserts every started span ended, that the spans form a single
// parent-child chain in start order and that the last span is the leaf.
func requireChain(t *testing.T, rec *tracetest.SpanRecorder, n int) []sdktrace.ReadOnlySpan {
	t.Helper()
	started := rec.Started()
	ended := rec.Ended()
	require.Len(t, started, n)
	require.Len(t, ended, n, "every attempt span must be ended exactly once")
	for _, s := range started {
		assert.False(t, s.EndTime().IsZero(), "span %s not ended", s.SpanContext().SpanID())
	}

	for i, s := range started {
		if i == 0 {
			continue
		}
		assert.Equal(t, started[i-1].SpanContext().SpanID(), s.Parent().SpanID(), "attempt %d parent", i+1)
		assert.Equal(t, started[0].SpanContext().TraceID(), s.SpanContext().TraceID())
	}

	// The last attempt is the leaf: no span has it as parent.
	leaf := started[n-1].SpanContext().SpanID()
	for _, s := range started {
		assert.NotEqual(t, leaf, s.Parent().SpanID())
	}
	return ended
}

func TestValidSpanIsCreated(t *testing.T) {
	srv := httptest.NewServer(testHandler())
	defer srv.Close()
	rec, provider := newRecorder()
	cli := resty.New()

	TraceClient(cli, WithTracerProvider(provider))
	cli.AddRequestMiddleware(func(c *resty.Client, r *resty.Request) error {
		assert.True(t, trace.SpanFromContext(r.Context()).SpanContext().IsValid())
		return nil
	})

	res, err := cli.R().Get(srv.URL + "/some/path")
	require.NoError(t, err)
	assert.Equal(t, 204, res.StatusCode())

	spans := requireChain(t, rec, 1)
	s := spans[0]
	assert.Equal(t, "http GET", s.Name())
	assert.Equal(t, trace.SpanKindClient, s.SpanKind())
	assert.Equal(t, codes.Unset, s.Status().Code)
	assert.Equal(t, tracerName, s.InstrumentationScope().Name)

	attrs := attrMap(s)
	assert.Equal(t, "GET", attrs["http.request.method"].AsString())
	assert.Equal(t, int64(204), attrs["http.response.status_code"].AsInt64())
	assert.Equal(t, srv.URL+"/some/path", attrs["url.full"].AsString())
	assert.Equal(t, "/some/path", attrs["url.path"].AsString())
	assert.Equal(t, "http", attrs["url.scheme"].AsString())
	assert.Equal(t, "127.0.0.1", attrs["server.address"].AsString())
	assert.Equal(t, srv.URL+"/some/path", attrs["resty.url"].AsString())
	assert.NotContains(t, attrs, attribute.Key("http.request.resend_count"))
}

func TestHideURL(t *testing.T) {
	srv := httptest.NewServer(testHandler())
	defer srv.Close()
	rec, provider := newRecorder()
	cli := resty.New()
	TraceClient(cli, WithTracerProvider(provider), WithHideURL(true))

	_, err := cli.R().Get(srv.URL + "/secret")
	require.NoError(t, err)

	attrs := attrMap(requireChain(t, rec, 1)[0])
	assert.Equal(t, "<redacted>", attrs["url.full"].AsString())
	assert.Equal(t, "<redacted>", attrs["resty.url"].AsString())
	assert.Equal(t, "/secret", attrs["url.path"].AsString())
}

func TestHTTPErrorStatusMarksSpan(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	rec, provider := newRecorder()
	cli := resty.New()
	TraceClient(cli, WithTracerProvider(provider))

	res, err := cli.R().Get(srv.URL)
	require.NoError(t, err)
	assert.Equal(t, 500, res.StatusCode())

	s := requireChain(t, rec, 1)[0]
	assert.Equal(t, codes.Error, s.Status().Code)
	assert.Equal(t, "500", attrMap(s)["error.type"].AsString())
}

func TestSpanNameFormatterAndOptions(t *testing.T) {
	srv := httptest.NewServer(testHandler())
	defer srv.Close()
	rec, provider := newRecorder()
	cli := resty.New()
	TraceClient(cli,
		WithTracerProvider(provider),
		WithTracerName("custom"),
		WithSpanNameFormatter(func(_ string, r *resty.Request) string { return "custom " + r.Method }),
		WithSpanOptions(trace.WithAttributes(attribute.String("extra", "1"))),
	)

	_, err := cli.R().Post(srv.URL)
	require.NoError(t, err)

	s := requireChain(t, rec, 1)[0]
	assert.Equal(t, "custom POST", s.Name())
	assert.Equal(t, "custom", s.InstrumentationScope().Name)
	assert.Equal(t, "1", attrMap(s)["extra"].AsString())
	assert.Equal(t, trace.SpanKindClient, s.SpanKind())
}

func TestPropagationWithCustomPropagator(t *testing.T) {
	var gotB3 atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotB3.Store(r.Header.Get("b3"))
		w.WriteHeader(204)
	}))
	defer srv.Close()

	rec, provider := newRecorder()
	cli := resty.New()
	TraceClient(cli, WithTracerProvider(provider), WithPropagators(b3.New()))

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    trace.TraceID{0x01},
		SpanID:     trace.SpanID{0x01},
		TraceFlags: trace.FlagsSampled,
	})
	ctx := trace.ContextWithRemoteSpanContext(context.Background(), sc)

	res, err := cli.R().SetContext(ctx).Get(srv.URL)
	require.NoError(t, err)
	assert.Equal(t, 204, res.StatusCode())

	s := requireChain(t, rec, 1)[0]
	assert.Equal(t, sc.TraceID(), s.SpanContext().TraceID())
	assert.Equal(t, sc.SpanID(), s.Parent().SpanID())

	header, _ := gotB3.Load().(string)
	require.NotEmpty(t, header, "b3 header must be injected")
	assert.Contains(t, header, s.SpanContext().TraceID().String())
	assert.Contains(t, header, s.SpanContext().SpanID().String())
}

func newRetryClient(retries int) *resty.Client {
	return resty.New().
		SetRetryCount(retries).
		SetRetryWaitTime(time.Millisecond).
		SetRetryMaxWaitTime(2 * time.Millisecond).
		AddRetryConditions(func(_ *resty.Response, err error) bool {
			return err != nil
		})
}

func TestRetriesCreateChainedEndedSpans(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			panic(http.ErrAbortHandler)
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()

	rec, provider := newRecorder()
	cli := newRetryClient(3)
	TraceClient(cli, WithTracerProvider(provider))

	res, err := cli.R().Get(srv.URL)
	require.NoError(t, err)
	assert.Equal(t, 204, res.StatusCode())
	assert.EqualValues(t, 3, calls.Load())

	spans := requireChain(t, rec, 3)
	assert.False(t, spans[0].Parent().IsValid(), "first attempt has no parent")

	// Attempts that failed with a transport error never got a response.
	for _, s := range spans[:2] {
		assert.Equal(t, codes.Error, s.Status().Code)
		assert.NotContains(t, attrMap(s), attribute.Key("http.response.status_code"))
	}
	last := spans[2]
	assert.Equal(t, codes.Unset, last.Status().Code)
	assert.Equal(t, int64(204), attrMap(last)["http.response.status_code"].AsInt64())

	assert.NotContains(t, attrMap(spans[0]), attribute.Key("http.request.resend_count"))
	assert.Equal(t, int64(1), attrMap(spans[1])["http.request.resend_count"].AsInt64())
	assert.Equal(t, int64(2), attrMap(last)["http.request.resend_count"].AsInt64())
}

func TestRetriesOnResponseStatus(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()

	rec, provider := newRecorder()
	cli := newRetryClient(2).AddRetryConditions(func(r *resty.Response, err error) bool {
		return r.StatusCode() == 503
	})
	TraceClient(cli, WithTracerProvider(provider))

	res, err := cli.R().Get(srv.URL)
	require.NoError(t, err)
	assert.Equal(t, 204, res.StatusCode())

	spans := requireChain(t, rec, 2)
	assert.Equal(t, int64(503), attrMap(spans[0])["http.response.status_code"].AsInt64())
	assert.Equal(t, int64(204), attrMap(spans[1])["http.response.status_code"].AsInt64())
}

func TestRetriesWithRequestLevelRetryHooks(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			panic(http.ErrAbortHandler)
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()

	rec, provider := newRecorder()
	cli := newRetryClient(2)
	TraceClient(cli, WithTracerProvider(provider))

	// Request level hooks replace the client ones, so the span of the first
	// attempt must still be ended by the next attempt's request middleware.
	_, err := cli.R().SetRetryHooks(func(*resty.Response, error) {}).Get(srv.URL)
	require.NoError(t, err)

	spans := requireChain(t, rec, 2)
	assert.Equal(t, codes.Error, spans[0].Status().Code)
	assert.Equal(t, codes.Unset, spans[1].Status().Code)
}

func TestAllAttemptsFail(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()

	rec, provider := newRecorder()
	cli := newRetryClient(2)
	TraceClient(cli, WithTracerProvider(provider))

	_, err := cli.R().Get(srv.URL)
	require.Error(t, err)
	assert.EqualValues(t, 3, calls.Load())

	spans := requireChain(t, rec, 3)
	for i, s := range spans {
		assert.Equal(t, codes.Error, s.Status().Code, "attempt %d", i+1)
	}

	// The final span records the error that resty returned.
	final := spans[2]
	assert.Equal(t, err.Error(), final.Status().Description)
	require.NotEmpty(t, final.Events())
	assert.Equal(t, "exception", final.Events()[0].Name)
	assert.Contains(t, attrMap(final), attribute.Key("error.type"))
}

var errRedirectDisabled = errors.New("redirects are disabled")

func redirectServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
}

func noRedirectClient() *resty.Client {
	return resty.New().SetRedirectPolicy(resty.RedirectPolicyFunc(
		func(*http.Request, []*http.Request) error { return errRedirectDisabled },
	))
}

func TestIgnoredErrors(t *testing.T) {
	srv := redirectServer()
	defer srv.Close()

	t.Run("without option", func(t *testing.T) {
		rec, provider := newRecorder()
		cli := noRedirectClient()
		TraceClient(cli, WithTracerProvider(provider))

		_, err := cli.R().Get(srv.URL)
		require.ErrorIs(t, err, errRedirectDisabled)

		s := requireChain(t, rec, 1)[0]
		assert.Equal(t, codes.Error, s.Status().Code)
		assert.NotEmpty(t, s.Events())
	})

	t.Run("WithIgnoredErrors", func(t *testing.T) {
		rec, provider := newRecorder()
		cli := noRedirectClient()
		TraceClient(cli, WithTracerProvider(provider), WithIgnoredErrors(errRedirectDisabled))

		_, err := cli.R().Get(srv.URL)
		require.ErrorIs(t, err, errRedirectDisabled)

		s := requireChain(t, rec, 1)[0]
		assert.Equal(t, codes.Unset, s.Status().Code)
		assert.Empty(t, s.Events())
		assert.NotContains(t, attrMap(s), attribute.Key("error.type"))
	})

	t.Run("WithErrorIgnorer", func(t *testing.T) {
		rec, provider := newRecorder()
		cli := noRedirectClient()
		TraceClient(cli, WithTracerProvider(provider), WithErrorIgnorer(func(err error) bool {
			return errors.Is(err, errRedirectDisabled)
		}))

		_, err := cli.R().Get(srv.URL)
		require.Error(t, err)

		s := requireChain(t, rec, 1)[0]
		assert.Equal(t, codes.Unset, s.Status().Code)
	})

	t.Run("unrelated errors are still recorded", func(t *testing.T) {
		rec, provider := newRecorder()
		cli := noRedirectClient()
		TraceClient(cli, WithTracerProvider(provider), WithIgnoredErrors(errors.New("other")))

		_, err := cli.R().Get(srv.URL)
		require.Error(t, err)

		assert.Equal(t, codes.Error, requireChain(t, rec, 1)[0].Status().Code)
	})
}

func TestErrorCustomizerAndDescriptionFormatter(t *testing.T) {
	srv := redirectServer()
	defer srv.Close()
	rec, provider := newRecorder()
	cli := noRedirectClient()
	custom := errors.New("customized")
	TraceClient(cli, WithTracerProvider(provider),
		WithErrorCustomizer(func(error) error { return custom }),
		WithErrorDescriptionFormatter(func(error) string { return "described" }),
	)

	_, err := cli.R().Get(srv.URL)
	require.Error(t, err)

	s := requireChain(t, rec, 1)[0]
	assert.Equal(t, "described", s.Status().Description)
	require.NotEmpty(t, s.Events())
	assert.Contains(t, s.Events()[0].Attributes, attribute.String("exception.message", "customized"))
}

func TestSkipperDoesNotTouchCallerSpan(t *testing.T) {
	srv := httptest.NewServer(testHandler())
	defer srv.Close()

	run := func(t *testing.T, url string) (*tracetest.SpanRecorder, trace.Span) {
		rec, provider := newRecorder()
		callerCtx, caller := provider.Tracer("caller").Start(context.Background(), "caller")

		cli := resty.New()
		TraceClient(cli, WithTracerProvider(provider), WithSkipper(func(*resty.Request) bool { return true }))

		_, _ = cli.R().SetContext(callerCtx).Get(url)
		return rec, caller
	}

	assertUntouched := func(t *testing.T, rec *tracetest.SpanRecorder, caller trace.Span) {
		t.Helper()
		assert.Empty(t, rec.Ended(), "no span may be ended")
		started := rec.Started()
		require.Len(t, started, 1, "only the caller span exists")
		assert.Equal(t, "caller", started[0].Name())
		assert.True(t, caller.IsRecording(), "caller span must not be ended")
		assert.Empty(t, started[0].Events())
		assert.Equal(t, codes.Unset, started[0].Status().Code)
	}

	t.Run("success", func(t *testing.T) {
		rec, caller := run(t, srv.URL)
		assertUntouched(t, rec, caller)
	})

	t.Run("transport error", func(t *testing.T) {
		closed := httptest.NewServer(testHandler())
		url := closed.URL
		closed.Close()

		rec, caller := run(t, url)
		assertUntouched(t, rec, caller)
	})
}

func TestUntracedRequestErrorDoesNotEndCallerSpan(t *testing.T) {
	// A failure raised before the tracing middleware runs (here: an earlier
	// request middleware) must not end the caller's span either.
	rec, provider := newRecorder()
	callerCtx, caller := provider.Tracer("caller").Start(context.Background(), "caller")

	cli := resty.New()
	cli.AddRequestMiddleware(func(*resty.Client, *resty.Request) error {
		return errors.New("boom")
	})
	TraceClient(cli, WithTracerProvider(provider))

	_, err := cli.R().SetContext(callerCtx).Get("http://127.0.0.1:1")
	require.Error(t, err)

	assert.True(t, caller.IsRecording())
	assert.Empty(t, rec.Ended())
	assert.Len(t, rec.Started(), 1)
}
