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
	"github.com/go-resty/resty/v2"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/semconv/v1.13.0/httpconv"
	"go.opentelemetry.io/otel/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

const (
	tracerName = "github.com/dubonzi/otelresty"
)

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

		ctx, span := tracer.Start(req.Context(), req.Method, cfg.SpanStartOptions...)

		attributes := []attribute.KeyValue{
			attribute.String("http.url", req.URL),
			attribute.String("http.method", req.Method),
		}

		if agent := req.Header.Get("user-agent"); agent != "" {
			attributes = append(attributes, attribute.String("http.user_agent", agent))
		}

		span.SetAttributes(attributes...)

		cfg.Propagators.Inject(ctx, propagation.HeaderCarrier(req.Header))
		req.SetContext(ctx)
		return nil
	}
}

func onAfterResponse(cfg *config) resty.ResponseMiddleware {
	return func(c *resty.Client, res *resty.Response) error {
		span := trace.SpanFromContext(res.Request.Context())
		span.SetAttributes(httpconv.ClientResponse(res.RawResponse)...)

		// Setting request attributes here since res.Request.RawRequest is nil
		// in onBeforeRequest.
		span.SetName(cfg.SpanNameFormatter("", res.Request))
		span.SetAttributes(httpconv.ClientRequest(res.Request.RawRequest)...)

		span.End()
		return nil
	}
}

func onError(cfg *config) resty.ErrorHook {
	return func(req *resty.Request, err error) {
		span := trace.SpanFromContext(req.Context())
		span.SetStatus(codes.Error, err.Error())
		span.SetName(cfg.SpanNameFormatter("", req))
		span.SetAttributes(httpconv.ClientRequest(req.RawRequest)...)
		span.End()
	}
}
