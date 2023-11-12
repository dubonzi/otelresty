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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-resty/resty/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/propagators/b3"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

func TestValidSpanIsCreated(t *testing.T) {
	srv := httptest.NewServer(testHandler())
	defer srv.Close()
	provider := sdktrace.NewTracerProvider(sdktrace.WithBatcher(tracetest.NewNoopExporter()))
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

func testHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(204)
	})
}
