# Go-resty OpenTelemetry Instrumentation
[![Docs](https://godoc.org/github.com/dubonzi/otelresty?status.svg)](https://pkg.go.dev/github.com/dubonzi/otelresty)

This repository aims to create a custom instrumentation for the [go-resty](https://github.com/go-resty/resty) project.

## How to use

Usage is as simple as calling `TraceClient` passing a resty client and options, if needed.

`TraceClient` uses the `OnBeforeRequest`, `OnAfterResponse` and `OnError` hooks from the resty client to create spans and fill their attributes with request, response and error information.

Note that resty hooks follow a queue order, meaning the first hook hook added will run before the others, so make sure to call `TraceClient` after adding your custom hooks so that the span information will have the correct values (considering your hooks modify request/response information).

```go
func main() {
  cli := resty.New()
  opts := []otelresty.Option{otelresty.WithTracerName("my-tracer")}

  otelresty.TraceClient(cli, opts...)
}
```

## Retries

Each retry attempt gets its own span, chained to the previous attempt's span as its parent, and every attempt span is properly ended. If all attempts fail, the error is recorded on the last attempt's span.

## Ignoring errors

Some resty errors are not real failures for your use case. For example, with `resty.NoRedirectPolicy`, every redirected request returns `resty.ErrAutoRedirectDisabled` and would otherwise end up in a span with an error status.

Use `WithIgnoredErrors` to pass a list of errors (matched with `errors.Is`, so sentinel and wrapped errors work):

```go
otelresty.TraceClient(cli, otelresty.WithIgnoredErrors(resty.ErrAutoRedirectDisabled))
```

For more control, `WithErrorIgnorer` takes a predicate consulted before the span is marked as erroneous:

```go
otelresty.TraceClient(cli, otelresty.WithErrorIgnorer(func(err error) bool {
  // return true to leave the span status untouched
  return false
}))
```
