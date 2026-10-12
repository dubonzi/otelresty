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
	"time"

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
	request   *resty.Request
	span      trace.Span
	once      sync.Once
	responded atomic.Bool
	errMu     sync.Mutex
	err       error
	response  *resty.Response
	retryEnd  time.Time
}

// finish runs fn (if any) against the span and ends it. Only the first call
// has any effect.
func (a *attempt) finish(fn func(trace.Span), opts ...trace.SpanEndOption) {
	a.once.Do(func() {
		if fn != nil {
			fn(a.span)
		}
		a.span.End(opts...)
	})
}

func (a *attempt) captureError(err error) {
	if err != nil {
		a.errMu.Lock()
		a.err = err
		a.errMu.Unlock()
	}
}

func (a *attempt) capturedError() error {
	a.errMu.Lock()
	defer a.errMu.Unlock()
	return a.err
}

// attemptFrom only returns an attempt owned by req. A nested request can
// inherit the context key without owning the caller's attempt. Never fall back to
// trace.SpanFromContext, which could return a span owned by the caller.
func attemptFrom(req *resty.Request) *attempt {
	a, _ := req.Context().Value(attemptKey{}).(*attempt)
	if a != nil && a.request != req {
		return nil
	}
	return a
}

// TraceClient instruments the resty client by adding a request middleware, a
// response middleware and the OnSuccess, OnError, OnInvalid and OnPanic hooks,
// plus a retry hook.
//
// One span is created per request attempt. When a request is retried, the span
// of each attempt is a child of the previous attempt's span, and every span is
// ended exactly once.
// Configure the client's transport, redirect policy and response middlewares
// before calling TraceClient so attempt errors can be captured before Resty
// clears them during retry processing.
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
	captureClientErrors(client)

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
		ctx := req.Context()
		// Only retries of this request may finish its previous attempt.
		if prev := attemptFrom(req); prev != nil {
			endRetriedAttempt(prev, cfg, req)
		}
		if cfg.Skipper(req) {
			// Hide an inherited attempt from the HTTP transport as well as hooks.
			req.SetContext(context.WithValue(ctx, attemptKey{}, (*attempt)(nil)))
			return nil
		}

		// The previous attempt's span (if any) is the current span of ctx,
		// which chains the attempts together.
		ctx, span := tracer.Start(ctx, cfg.SpanNameFormatter("", req), cfg.SpanStartOptions...)

		cfg.Propagators.Inject(ctx, propagation.HeaderCarrier(req.Header))

		if cfg.HideURL {
			span.SetAttributes(attribute.String("resty.url", redacted))
		} else {
			u, err := url.Parse(req.URL)
			if err != nil {
				span.SetAttributes(attribute.String("resty.url", redacted))
			} else {
				span.SetAttributes(attribute.String("resty.url", redactedUserinfo(u)))
			}
		}
		if req.Attempt > 1 {
			span.SetAttributes(semconv.HTTPRequestResendCount(req.Attempt - 1))
		}

		ctx = context.WithValue(ctx, attemptKey{}, &attempt{request: req, span: span})
		req.SetContext(ctx)
		return nil
	}
}

// onAfterResponse records the response attributes. The span is not ended here
// because errors raised by later middlewares or by the retry logic are only
// reported to the retry and error hooks.
func onAfterResponse() resty.ResponseMiddleware {
	return func(_ *resty.Client, res *resty.Response) error {
		a := attemptFrom(res.Request)
		if a == nil {
			return nil
		}
		a.responded.Store(true)
		a.response = res
		a.captureError(res.CascadeError)
		setResponseAttributes(a.span, res)
		return nil
	}
}

// onRetry saves the end timestamp and attributes before the retry wait. The
// span stays open until another attempt starts or a final hook runs, because
// cancellation or the retry strategy can still fail during the wait.
func onRetry(cfg *config) resty.RetryHookFunc {
	return func(res *resty.Response, err error) {
		if res == nil || res.Request == nil {
			return
		}
		a := attemptFrom(res.Request)
		if a == nil {
			return
		}
		a.response = res
		a.captureError(err)
		setRequestAttributes(a.span, cfg, res.Request)
		a.retryEnd = time.Now()
	}
}

func onSuccess(cfg *config) resty.SuccessHook {
	return func(_ *resty.Client, res *resty.Response) {
		a := attemptFrom(res.Request)
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
		a := attemptFrom(req)
		if a == nil {
			return
		}
		failAttempt(a, cfg, req, err)
	}
}

// endRetriedAttempt runs only once the next attempt actually starts. Retry
// hooks receive a cleared error in Resty rc.4, so use the captured attempt error.
func endRetriedAttempt(a *attempt, cfg *config, req *resty.Request) {
	var endOpts []trace.SpanEndOption
	var eventOpts []trace.EventOption
	if !a.retryEnd.IsZero() {
		endOpts = append(endOpts, trace.WithTimestamp(a.retryEnd))
		eventOpts = append(eventOpts, trace.WithTimestamp(a.retryEnd))
	}
	a.finish(func(span trace.Span) {
		if a.retryEnd.IsZero() {
			// Request-level retry hooks can replace the tracing hook.
			setRequestAttributes(span, cfg, req)
		}
		if err := a.capturedError(); err != nil {
			if a.response != nil {
				err = &resty.ResponseError{Response: a.response, Err: err}
			}
			recordError(span, cfg, err, eventOpts...)
		} else if !a.responded.Load() {
			span.SetStatus(codes.Error, "request attempt failed")
			span.SetAttributes(semconv.ErrorTypeOther)
		}
	}, endOpts...)
}

func failAttempt(a *attempt, cfg *config, req *resty.Request, err error) {
	a.finish(func(span trace.Span) {
		setRequestAttributes(span, cfg, req)
		recordError(span, cfg, err)
	})
}

func recordError(span trace.Span, cfg *config, err error, opts ...trace.EventOption) {
	if err == nil || cfg.ErrorIgnorer(err) {
		return
	}

	span.RecordError(cfg.ErrorCustomizer(err), opts...)
	span.SetStatus(codes.Error, cfg.ErrorDescriptionFormatter(err))

	typed := err
	var respErr *resty.ResponseError
	if errors.As(err, &respErr) && respErr.Err != nil {
		typed = respErr.Err
	}
	span.SetAttributes(semconv.ErrorType(typed))
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
