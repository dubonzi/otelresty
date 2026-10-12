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

package otelresty // import "github.com/dubonzi/otelresty"

import (
	"context"
	"net"
	"net/http"
	"strconv"

	"github.com/go-resty/resty/v2"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	oteltrace "go.opentelemetry.io/otel/trace"
)

const (
	tracerName = "github.com/dubonzi/otelresty"
)

// attemptSpanKey identifies the span of the current request attempt inside
// the request context. resty re-runs the OnBeforeRequest middleware on every
// retry attempt, so each attempt stores its own span under this key.
type attemptSpanKey struct{}

// TraceClient instruments the resty client by adding OnBeforeRequest, OnAfterResponse and OnError hooks.
func TraceClient(client *resty.Client, options ...Option) {
	cfg := newConfig(options...)
	if cfg.TracerProvider == nil {
		cfg.TracerProvider = otel.GetTracerProvider()
	}
	tracer := cfg.TracerProvider.Tracer(
		cfg.TracerName,
		oteltrace.WithInstrumentationVersion(SemVersion()),
	)

	client.OnBeforeRequest(onBeforeRequest(tracer, cfg))
	client.OnAfterResponse(onAfterResponse(cfg))
	client.OnError(onError(cfg))
}

func onBeforeRequest(tracer oteltrace.Tracer, cfg *config) resty.RequestMiddleware {
	return func(cli *resty.Client, req *resty.Request) error {
		if cfg.Skipper(req) {
			return nil
		}

		// resty re-runs this middleware on every attempt when retries are
		// enabled, while OnAfterResponse only runs for attempts that got a
		// response and OnError only runs once after all attempts finished.
		// Close the previous attempt's span here so no span is left open
		// when a retry starts. Span.End is safe to call more than once.
		if prev, ok := attemptSpan(req); ok {
			prev.End()
		}

		ctx, span := tracer.Start(req.Context(), req.Method, cfg.SpanStartOptions...)
		ctx = context.WithValue(ctx, attemptSpanKey{}, span)

		cfg.Propagators.Inject(ctx, propagation.HeaderCarrier(req.Header))

		if cfg.HideURL {
			span.SetAttributes(attribute.String("resty.url", "<redacted>"))
		} else {
			span.SetAttributes(attribute.String("resty.url", req.URL))
		}

		req.SetContext(ctx)
		return nil
	}
}

func onAfterResponse(cfg *config) resty.ResponseMiddleware {
	return func(c *resty.Client, res *resty.Response) error {
		span, ok := attemptSpan(res.Request)
		if !ok {
			// No span was created for this request (the skipper disabled
			// it, or onBeforeRequest never ran). Leave any span carried in
			// the caller context untouched.
			return nil
		}

		if res.RawResponse != nil {
			span.SetAttributes(clientResponseAttributes(res.RawResponse)...)
		}

		// Setting request attributes here since res.Request.RawRequest is nil
		// in onBeforeRequest.
		span.SetName(cfg.SpanNameFormatter("", res.Request))
		span = setRequestAttributes(span, cfg, res.Request)

		span.End()
		return nil
	}
}

func onError(cfg *config) resty.ErrorHook {
	return func(req *resty.Request, err error) {
		span, ok := attemptSpan(req)
		if !ok {
			return
		}

		if !cfg.isErrorIgnored(err) {
			span.RecordError(cfg.ErrorCustomizer(err))
			span.SetStatus(codes.Error, cfg.ErrorDescriptionFormatter(err))
		}

		span.SetName(cfg.SpanNameFormatter("", req))

		span = setRequestAttributes(span, cfg, req)

		span.End()
	}
}

func setRequestAttributes(span oteltrace.Span, cfg *config, req *resty.Request) oteltrace.Span {
	if req.RawRequest == nil {
		return span
	}

	span.SetAttributes(clientRequestAttributes(req.RawRequest)...)

	if req.RawRequest.URL != nil {
		span.SetAttributes(semconv.URLPathKey.String(req.RawRequest.URL.Path))
	}

	if cfg.HideURL {
		span.SetAttributes(semconv.URLFullKey.String("<redacted>"))
	} else if req.RawRequest.URL != nil {
		span.SetAttributes(semconv.URLFullKey.String(req.RawRequest.URL.String()))
	}

	return span
}

// clientRequestAttributes sets the stable HTTP client span attributes for
// the outgoing request, following the semantic conventions used by the
// official OpenTelemetry HTTP instrumentations.
func clientRequestAttributes(req *http.Request) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		semconv.HTTPRequestMethodKey.String(methodOrGet(req)),
	}

	if req.URL != nil {
		if req.URL.Scheme != "" {
			attrs = append(attrs, semconv.URLSchemeKey.String(req.URL.Scheme))
		}
		if req.URL.Host != "" {
			attrs = append(attrs, serverAttributes(req.URL.Host)...)
		}
	}

	if req.ContentLength > 0 {
		attrs = append(attrs, semconv.HTTPRequestBodySizeKey.Int64(req.ContentLength))
	}

	return attrs
}

// clientResponseAttributes sets the stable HTTP client span attributes for
// the received response.
func clientResponseAttributes(res *http.Response) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		semconv.HTTPResponseStatusCodeKey.Int(res.StatusCode),
	}

	if res.ContentLength > 0 {
		attrs = append(attrs, semconv.HTTPResponseBodySizeKey.Int64(res.ContentLength))
	}

	return attrs
}

// serverAttributes splits a URL host into server.address and server.port.
func serverAttributes(host string) []attribute.KeyValue {
	hostname := host
	if h, p, err := net.SplitHostPort(host); err == nil {
		hostname = h
		if port, err := strconv.Atoi(p); err == nil {
			return append(
				[]attribute.KeyValue{semconv.ServerAddressKey.String(h)},
				semconv.ServerPortKey.Int(port),
			)
		}
	}

	return []attribute.KeyValue{semconv.ServerAddressKey.String(hostname)}
}

func methodOrGet(req *http.Request) string {
	if req.Method != "" {
		return req.Method
	}
	return http.MethodGet
}

// attemptSpan returns the span of the current request attempt, if one was
// created by this instrumentation.
func attemptSpan(req *resty.Request) (oteltrace.Span, bool) {
	if req == nil {
		return nil, false
	}
	span, ok := req.Context().Value(attemptSpanKey{}).(oteltrace.Span)
	if !ok || span == nil {
		return nil, false
	}
	return span, true
}
