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

package restyv3 // import "github.com/dubonzi/otelresty/restyv3"

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
	"resty.dev/v3"
)

const (
	tracerName = "github.com/dubonzi/otelresty"

	redacted = "<redacted>"
)

// attemptKey is the private context key under which the span of the current
// request attempt is stored.
type attemptKey struct{}

// attempt tracks the span of a single request attempt and guarantees it is
// ended exactly once.
type attempt struct {
	span      trace.Span
	once      sync.Once
	responded atomic.Bool
}

// finish runs fn (if any) against the span and ends it. Only the first call
// has any effect.
func (a *attempt) finish(fn func(trace.Span)) {
	a.once.Do(func() {
		if fn != nil {
			fn(a.span)
		}
		a.span.End()
	})
}

// attemptFrom returns the attempt stored in ctx by the request middleware, or
// nil if the request is not being traced. It deliberately never falls back to
// trace.SpanFromContext, which could return a span owned by the caller.
func attemptFrom(ctx context.Context) *attempt {
	a, _ := ctx.Value(attemptKey{}).(*attempt)
	return a
}

// TraceClient instruments the resty client by adding a request middleware, a
// response middleware and the OnSuccess, OnError, OnInvalid and OnPanic hooks,
// plus a retry hook.
//
// One span is created per request attempt. When a request is retried, the span
// of each attempt is a child of the previous attempt's span, and every span is
// ended exactly once.
func TraceClient(client *resty.Client, options ...Option) {
	cfg := newConfig(options...)
	if cfg.TracerProvider == nil {
		cfg.TracerProvider = otel.GetTracerProvider()
	}
	tracer := cfg.TracerProvider.Tracer(
		cfg.TracerName,
		trace.WithInstrumentationVersion(SemVersion()),
		trace.WithSchemaURL(semconv.SchemaURL),
	)

	client.AddRequestMiddleware(onBeforeRequest(tracer, cfg))
	client.AddResponseMiddleware(onAfterResponse())
	client.AddRetryHooks(onRetry(cfg))
	client.OnSuccess(onSuccess(cfg))
	client.OnError(onError(cfg))
	client.OnInvalid(onError(cfg))
	client.OnPanic(onError(cfg))
}

func onBeforeRequest(tracer trace.Tracer, cfg *config) resty.RequestMiddleware {
	return func(_ *resty.Client, req *resty.Request) error {
		if cfg.Skipper(req) {
			return nil
		}

		ctx := req.Context()

		// A retry re-runs the request middlewares. Close the previous attempt
		// before starting the next one, in case nothing ended it already.
		if prev := attemptFrom(ctx); prev != nil {
			endAbandoned(prev, cfg, req)
		}

		// The previous attempt's span (if any) is the current span of ctx,
		// which chains the attempts together.
		ctx, span := tracer.Start(ctx, cfg.SpanNameFormatter("", req), cfg.SpanStartOptions...)

		cfg.Propagators.Inject(ctx, propagation.HeaderCarrier(req.Header))

		if cfg.HideURL {
			span.SetAttributes(attribute.String("resty.url", redacted))
		} else {
			span.SetAttributes(attribute.String("resty.url", req.URL))
		}
		if req.Attempt > 1 {
			span.SetAttributes(semconv.HTTPRequestResendCount(req.Attempt - 1))
		}

		ctx = context.WithValue(ctx, attemptKey{}, &attempt{span: span})
		req.SetContext(ctx)
		return nil
	}
}

// onAfterResponse records the response attributes. The span is not ended here
// because errors raised by later middlewares or by the retry logic are only
// reported to the retry and error hooks.
func onAfterResponse() resty.ResponseMiddleware {
	return func(_ *resty.Client, res *resty.Response) error {
		a := attemptFrom(res.Request.Context())
		if a == nil {
			return nil
		}
		a.responded.Store(true)
		setResponseAttributes(a.span, res)
		return nil
	}
}

// onRetry ends the span of an attempt that is about to be retried, so its
// duration does not include the backoff wait.
//
// resty may call the retry hooks with a nil error even when the attempt failed
// (the error is not available once file readers are reset), so an attempt that
// never received a response is treated as failed regardless of err.
func onRetry(cfg *config) resty.RetryHookFunc {
	return func(res *resty.Response, err error) {
		if res == nil || res.Request == nil {
			return
		}
		a := attemptFrom(res.Request.Context())
		if a == nil {
			return
		}
		if err != nil {
			failAttempt(a, cfg, res.Request, err)
			return
		}
		endAbandoned(a, cfg, res.Request)
	}
}

func onSuccess(cfg *config) resty.SuccessHook {
	return func(_ *resty.Client, res *resty.Response) {
		a := attemptFrom(res.Request.Context())
		if a == nil {
			return
		}
		a.finish(func(span trace.Span) {
			setRequestAttributes(span, cfg, res.Request)
		})
	}
}

func onError(cfg *config) resty.ErrorHook {
	return func(req *resty.Request, err error) {
		a := attemptFrom(req.Context())
		if a == nil {
			return
		}
		failAttempt(a, cfg, req, err)
	}
}

// endAbandoned ends the span of an attempt that was superseded by a retry. If
// no response was received for it, the attempt is marked as failed, since the
// underlying error is not available at this point.
func endAbandoned(a *attempt, cfg *config, req *resty.Request) {
	a.finish(func(span trace.Span) {
		setRequestAttributes(span, cfg, req)
		if !a.responded.Load() {
			span.SetStatus(codes.Error, "request attempt failed")
			span.SetAttributes(semconv.ErrorTypeOther)
		}
	})
}

func failAttempt(a *attempt, cfg *config, req *resty.Request, err error) {
	a.finish(func(span trace.Span) {
		setRequestAttributes(span, cfg, req)

		if err == nil || cfg.ErrorIgnorer(err) {
			return
		}

		span.RecordError(cfg.ErrorCustomizer(err))
		span.SetStatus(codes.Error, cfg.ErrorDescriptionFormatter(err))

		typed := err
		var respErr *resty.ResponseError
		if errors.As(err, &respErr) && respErr.Err != nil {
			typed = respErr.Err
		}
		span.SetAttributes(semconv.ErrorType(typed))
	})
}

func setResponseAttributes(span trace.Span, res *resty.Response) {
	raw := res.RawResponse
	if raw == nil {
		return
	}

	span.SetAttributes(semconv.HTTPResponseStatusCode(raw.StatusCode))
	if raw.ContentLength >= 0 {
		span.SetAttributes(semconv.HTTPResponseBodySize(int(raw.ContentLength)))
	}
	if v, ok := strings.CutPrefix(raw.Proto, "HTTP/"); ok {
		span.SetAttributes(semconv.NetworkProtocolVersion(v))
	}
	if raw.StatusCode >= http.StatusBadRequest {
		span.SetStatus(codes.Error, "")
		span.SetAttributes(semconv.ErrorTypeKey.String(strconv.Itoa(raw.StatusCode)))
	}
}

// setRequestAttributes sets the request attributes and the span name. It is
// done at the end of the attempt since the underlying http.Request is not
// available yet when the request middleware runs.
func setRequestAttributes(span trace.Span, cfg *config, req *resty.Request) {
	span.SetName(cfg.SpanNameFormatter("", req))

	raw := req.RawRequest
	if raw == nil {
		return
	}

	span.SetAttributes(methodAttributes(raw.Method)...)

	if ua := raw.Header.Get("User-Agent"); ua != "" {
		span.SetAttributes(semconv.UserAgentOriginal(ua))
	}
	if raw.ContentLength > 0 {
		span.SetAttributes(semconv.HTTPRequestBodySize(int(raw.ContentLength)))
	}

	u := raw.URL
	if u == nil {
		return
	}

	if cfg.HideURL {
		span.SetAttributes(semconv.URLFull(redacted))
	} else {
		span.SetAttributes(semconv.URLFull(redactedUserinfo(u)))
	}
	span.SetAttributes(semconv.URLPath(u.Path))
	if u.Scheme != "" {
		span.SetAttributes(semconv.URLScheme(u.Scheme))
	}
	if host := u.Hostname(); host != "" {
		span.SetAttributes(semconv.ServerAddress(host))
		if port := serverPort(u); port > 0 {
			span.SetAttributes(semconv.ServerPort(port))
		}
	}
}

func methodAttributes(method string) []attribute.KeyValue {
	switch method {
	case http.MethodConnect, http.MethodDelete, http.MethodGet, http.MethodHead,
		http.MethodOptions, http.MethodPatch, http.MethodPost, http.MethodPut,
		http.MethodTrace:
		return []attribute.KeyValue{semconv.HTTPRequestMethodKey.String(method)}
	case "":
		return []attribute.KeyValue{semconv.HTTPRequestMethodGet}
	default:
		return []attribute.KeyValue{
			semconv.HTTPRequestMethodOther,
			semconv.HTTPRequestMethodOriginal(method),
		}
	}
}

func serverPort(u *url.URL) int {
	if p := u.Port(); p != "" {
		if port, err := strconv.Atoi(p); err == nil {
			return port
		}
		return 0
	}
	switch u.Scheme {
	case "http":
		return 80
	case "https":
		return 443
	}
	return 0
}

// redactedUserinfo returns the URL string with any credentials replaced, as
// required by the url.full semantic convention.
func redactedUserinfo(u *url.URL) string {
	if u.User == nil {
		return u.String()
	}
	c := *u
	c.User = url.UserPassword("REDACTED", "REDACTED")
	return c.String()
}
