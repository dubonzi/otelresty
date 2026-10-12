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
	"errors"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"resty.dev/v3"
)

// defaultSkipper provides default behaviour, which won't skip span creation.
func defaultSkipper(*resty.Request) bool {
	return false
}

func defaultSpanNameFormatter(_ string, req *resty.Request) string {
	return "http " + req.Method
}

func defaultErrorDescriptionFormatter(err error) string {
	return err.Error()
}

func defaultErrorCustomizer(err error) error {
	return err
}

func defaultErrorIgnorer(error) bool {
	return false
}

// config is used to configure the go-resty middleware.
type config struct {
	TracerProvider            trace.TracerProvider
	Propagators               propagation.TextMapPropagator
	SpanNameFormatter         func(string, *resty.Request) string
	ErrorDescriptionFormatter func(err error) string
	ErrorCustomizer           func(err error) error
	ErrorIgnorer              func(err error) bool
	SpanStartOptions          []trace.SpanStartOption
	Skipper                   func(*resty.Request) bool
	TracerName                string
	HideURL                   bool
}

func newConfig(options ...Option) *config {
	cfg := &config{
		Propagators:               otel.GetTextMapPropagator(),
		TracerProvider:            otel.GetTracerProvider(),
		Skipper:                   defaultSkipper,
		TracerName:                tracerName,
		SpanNameFormatter:         defaultSpanNameFormatter,
		ErrorDescriptionFormatter: defaultErrorDescriptionFormatter,
		ErrorCustomizer:           defaultErrorCustomizer,
		ErrorIgnorer:              defaultErrorIgnorer,
		HideURL:                   false,
	}

	defaultOpts := []Option{
		WithSpanOptions(trace.WithSpanKind(trace.SpanKindClient)),
	}

	options = append(defaultOpts, options...)

	for _, opt := range options {
		opt.apply(cfg)
	}

	return cfg
}

// Option applies a configuration value.
type Option interface {
	apply(*config)
}

type optionFunc func(*config)

func (o optionFunc) apply(c *config) {
	o(c)
}

// WithSkipper specifies a skipper function to determine if the middleware
// should not create a span for a determined request. If not specified,
// a span will always be created.
func WithSkipper(skipper func(r *resty.Request) bool) Option {
	return optionFunc(func(c *config) {
		if skipper != nil {
			c.Skipper = skipper
		}
	})
}

// WithPropagators specifies propagators to use for injecting trace
// information into the HTTP requests. If none are specified, global
// ones will be used.
func WithPropagators(propagators propagation.TextMapPropagator) Option {
	return optionFunc(func(cfg *config) {
		if propagators != nil {
			cfg.Propagators = propagators
		}
	})
}

// WithTracerProvider specifies a tracer provider to use for creating a tracer.
// If none is specified, the global provider is used.
func WithTracerProvider(provider trace.TracerProvider) Option {
	return optionFunc(func(cfg *config) {
		if provider != nil {
			cfg.TracerProvider = provider
		}
	})
}

// WithSpanOptions configures an additional set of
// trace.SpanOptions, which are applied to each new span.
func WithSpanOptions(opts ...trace.SpanStartOption) Option {
	return optionFunc(func(c *config) {
		c.SpanStartOptions = append(c.SpanStartOptions, opts...)
	})
}

// WithSpanNameFormatter takes a function that will be called on every
// request and the returned string will become the Span Name.
func WithSpanNameFormatter(f func(operation string, r *resty.Request) string) Option {
	return optionFunc(func(c *config) {
		if f != nil {
			c.SpanNameFormatter = f
		}
	})
}

// WithErrorDescriptionFormatter takes a function that will be called when
// a request fails to modify the error message used as the span status
// description.
func WithErrorDescriptionFormatter(f func(err error) string) Option {
	return optionFunc(func(c *config) {
		if f != nil {
			c.ErrorDescriptionFormatter = f
		}
	})
}

// WithErrorCustomizer takes a function that will be called when a request
// fails and allows for customization of the error recorded on the span.
func WithErrorCustomizer(f func(err error) error) Option {
	return optionFunc(func(c *config) {
		if f != nil {
			c.ErrorCustomizer = f
		}
	})
}

// WithErrorIgnorer takes a predicate that is consulted before a span is marked
// as erroneous. If it returns true for an error, the error is not recorded and
// the span status is left unset. The span is still ended.
//
// When used together with WithIgnoredErrors, an error is ignored if any of the
// predicates matches it.
func WithErrorIgnorer(f func(err error) bool) Option {
	return optionFunc(func(c *config) {
		if f == nil {
			return
		}
		prev := c.ErrorIgnorer
		c.ErrorIgnorer = func(err error) bool {
			return prev(err) || f(err)
		}
	})
}

// WithIgnoredErrors is a convenience over WithErrorIgnorer: errors matching
// any of errs using errors.Is do not mark the span as erroneous.
func WithIgnoredErrors(errs ...error) Option {
	return WithErrorIgnorer(func(err error) bool {
		for _, target := range errs {
			if errors.Is(err, target) {
				return true
			}
		}
		return false
	})
}

// WithTracerName sets the name of the tracer used to create spans. The
// default value is "github.com/dubonzi/otelresty".
func WithTracerName(name string) Option {
	return optionFunc(func(c *config) {
		c.TracerName = name
	})
}

// WithHideURL if set to true, the url.full and resty.url attributes will be
// replaced with "<redacted>". The default value is false.
func WithHideURL(hide bool) Option {
	return optionFunc(func(c *config) {
		c.HideURL = hide
	})
}
