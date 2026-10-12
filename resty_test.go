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

package otelresty

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/propagators/b3"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

func newRecordingProvider() (*sdktrace.TracerProvider, *tracetest.SpanRecorder) {
	rec := tracetest.NewSpanRecorder()
	return sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)), rec
}

func TestValidSpanIsCreated(t *testing.T) {
	srv := httptest.NewServer(testHandler())
	defer srv.Close()
	provider, rec := newRecordingProvider()
	cli := resty.New()

	TraceClient(cli, WithTracerProvider(provider))
	cli.OnBeforeRequest(func(c *resty.Client, r *resty.Request) error {
		span := trace.SpanFromContext(r.Context())
		assert.True(t, span.SpanContext().IsValid())
		return nil
	})

	res, err := cli.R().Get(srv.URL)
	require.NoError(t, err)
	assert.Equal(t, 204, res.StatusCode())

	spans := rec.Ended()
	require.Len(t, spans, 1)
	span := spans[0]
	assert.Equal(t, "http GET", span.Name())
	assert.Equal(t, trace.SpanKindClient, span.SpanKind())

	hasAttr := func(key attribute.Key) bool {
		for _, kv := range span.Attributes() {
			if kv.Key == key {
				return true
			}
		}
		return false
	}

	assert.True(t, hasAttr(semconv.HTTPRequestMethodKey), "should have http.request.method")
	assert.True(t, hasAttr(semconv.HTTPResponseStatusCodeKey), "should have http.response.status_code")
	assert.True(t, hasAttr(semconv.URLFullKey), "should have url.full")
	assert.True(t, hasAttr(semconv.ServerAddressKey), "should have server.address")
	assert.True(t, hasAttr(semconv.URLPathKey), "should have url.path")
	assert.Equal(t, codes.Unset, span.Status().Code)
}

func TestPropagationWithCustomPropagator(t *testing.T) {
	srv := httptest.NewServer(testHandler())
	defer srv.Close()

	prop := b3.New()
	provider := noop.NewTracerProvider()
	ctx := context.Background()
	cli := resty.New()

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{0x01},
		SpanID:  trace.SpanID{0x01},
	})

	ctx = trace.ContextWithRemoteSpanContext(ctx, sc)
	ctx, _ = provider.Tracer(tracerName).Start(ctx, "test")

	TraceClient(cli, WithTracerProvider(provider), WithPropagators(prop))

	cli.OnBeforeRequest(func(c *resty.Client, r *resty.Request) error {
		span := trace.SpanFromContext(r.Context())
		assert.True(t, span.SpanContext().IsValid())
		assert.Equal(t, sc.TraceID(), span.SpanContext().TraceID())
		assert.Equal(t, sc.SpanID(), span.SpanContext().SpanID())
		return nil
	})

	req := cli.R()
	prop.Inject(ctx, propagation.HeaderCarrier(req.Header))
	req.SetContext(ctx)

	res, err := req.Get(srv.URL)
	require.NoError(t, err)
	assert.Equal(t, 204, res.StatusCode())
}

func TestRetrySpansAreAllCompleted(t *testing.T) {
	// Reproduces https://github.com/dubonzi/otelresty/issues/9: resty
	// re-runs OnBeforeRequest on every attempt, but only the final attempt
	// had its span ended, leaving the earlier attempt spans dangling.
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) <= 2 {
			// Abort the connection so the attempt fails with a transport
			// error and OnAfterResponse never runs for it.
			panic(http.ErrAbortHandler)
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	provider, rec := newRecordingProvider()
	cli := resty.New()
	TraceClient(cli, WithTracerProvider(provider))
	cli.SetRetryCount(2).
		SetRetryWaitTime(time.Millisecond).
		SetRetryMaxWaitTime(time.Millisecond)

	res, err := cli.R().Get(srv.URL)
	require.NoError(t, err)
	assert.Equal(t, 200, res.StatusCode())

	// Every attempt must have its span ended: three attempts, three spans.
	spans := rec.Ended()
	require.Len(t, spans, 3)

	// Attempts form a parent-child chain.
	assert.Equal(t, trace.SpanID{}, spans[0].Parent().SpanID())
	assert.Equal(t, spans[0].SpanContext().SpanID(), spans[1].Parent().SpanID())
	assert.Equal(t, spans[1].SpanContext().SpanID(), spans[2].Parent().SpanID())
	assert.Equal(t, spans[0].SpanContext().TraceID(), spans[2].SpanContext().TraceID())
	assert.Equal(t, codes.Unset, spans[2].Status().Code)
}

func TestRetryAllAttemptsFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()

	provider, rec := newRecordingProvider()
	cli := resty.New()
	TraceClient(cli, WithTracerProvider(provider))
	cli.SetRetryCount(2).
		SetRetryWaitTime(time.Millisecond).
		SetRetryMaxWaitTime(time.Millisecond)

	_, err := cli.R().Get(srv.URL)
	require.Error(t, err)

	spans := rec.Ended()
	require.Len(t, spans, 3, "all attempt spans must be ended")

	last := spans[len(spans)-1]
	assert.Equal(t, codes.Error, last.Status().Code)
	assert.NotEmpty(t, last.Events(), "error must be recorded on the final span")
}

func TestIgnoredErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()

	t.Run("error marks span", func(t *testing.T) {
		provider, rec := newRecordingProvider()
		cli := resty.New()
		cli.SetRedirectPolicy(resty.NoRedirectPolicy())
		TraceClient(cli, WithTracerProvider(provider))

		_, err := cli.R().Get(srv.URL)
		require.ErrorIs(t, err, resty.ErrAutoRedirectDisabled)

		spans := rec.Ended()
		require.Len(t, spans, 1)
		assert.Equal(t, codes.Error, spans[0].Status().Code)
		assert.NotEmpty(t, spans[0].Events())
	})

	t.Run("ignored error does not mark span", func(t *testing.T) {
		provider, rec := newRecordingProvider()
		cli := resty.New()
		cli.SetRedirectPolicy(resty.NoRedirectPolicy())
		TraceClient(cli,
			WithTracerProvider(provider),
			WithIgnoredErrors(resty.ErrAutoRedirectDisabled),
		)

		_, err := cli.R().Get(srv.URL)
		require.ErrorIs(t, err, resty.ErrAutoRedirectDisabled)

		spans := rec.Ended()
		require.Len(t, spans, 1)
		assert.Equal(t, codes.Unset, spans[0].Status().Code, "ignored error must not set error status")
		assert.Empty(t, spans[0].Events(), "ignored error must not be recorded")
	})

	t.Run("ignored error with predicate", func(t *testing.T) {
		provider, rec := newRecordingProvider()
		cli := resty.New()
		cli.SetRedirectPolicy(resty.NoRedirectPolicy())
		TraceClient(cli,
			WithTracerProvider(provider),
			WithErrorIgnorer(func(err error) bool {
				return errors.Is(err, resty.ErrAutoRedirectDisabled)
			}),
		)

		_, err := cli.R().Get(srv.URL)
		require.ErrorIs(t, err, resty.ErrAutoRedirectDisabled)

		spans := rec.Ended()
		require.Len(t, spans, 1)
		assert.Equal(t, codes.Unset, spans[0].Status().Code)
	})
}

func TestSkipperDoesNotTouchCallerSpan(t *testing.T) {
	srv := httptest.NewServer(testHandler())
	defer srv.Close()

	provider, rec := newRecordingProvider()
	cli := resty.New()
	TraceClient(cli,
		WithTracerProvider(provider),
		WithSkipper(func(*resty.Request) bool { return true }),
	)

	ctx, callerSpan := provider.Tracer("test").Start(context.Background(), "caller")
	defer callerSpan.End()

	res, err := cli.R().SetContext(ctx).Get(srv.URL)
	require.NoError(t, err)
	assert.Equal(t, 204, res.StatusCode())

	// The middleware must not create a span and must not end the caller's
	// span, even though it is present in the request context.
	assert.True(t, callerSpan.IsRecording(), "caller span must not be ended by the middleware")
	assert.Empty(t, rec.Ended())
}

func TestHideURLRedactsAttributes(t *testing.T) {
	srv := httptest.NewServer(testHandler())
	defer srv.Close()

	provider, rec := newRecordingProvider()
	cli := resty.New()
	TraceClient(cli, WithTracerProvider(provider), WithHideURL(true))

	res, err := cli.R().Get(srv.URL + "/secret/path")
	require.NoError(t, err)
	assert.Equal(t, 204, res.StatusCode())

	spans := rec.Ended()
	require.Len(t, spans, 1)
	attrs := map[attribute.Key]string{}
	for _, kv := range spans[0].Attributes() {
		if kv.Value.Type() == attribute.STRING {
			attrs[kv.Key] = kv.Value.AsString()
		}
	}
	assert.Equal(t, "<redacted>", attrs[attribute.Key("resty.url")])
	assert.Equal(t, "<redacted>", attrs[semconv.URLFullKey])
}

func testHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	})
}
