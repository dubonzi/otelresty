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

## Resty v3

Clients from [resty v3](https://github.com/go-resty/resty) (`resty.dev/v3`) are supported by the `restyv3` package, which is part of the same module:

```go
import (
  "resty.dev/v3"

  "github.com/dubonzi/otelresty/restyv3"
)

func main() {
  cli := resty.New()

  restyv3.TraceClient(cli, restyv3.WithTracerName("my-tracer"))
}
```

It supports the same options as the resty v2 package, plus `WithErrorIgnorer` and `WithIgnoredErrors`, which keep matching errors (checked with `errors.Is`) from marking the span as failed:

```go
restyv3.TraceClient(cli, restyv3.WithIgnoredErrors(errRedirectDisabled))
```

A span is created for each request attempt. When a request is retried, each attempt gets its own span, chained as a child of the previous attempt's span, and every span is ended exactly once. Spans use the OpenTelemetry HTTP semantic conventions (`semconv/v1.37.0`).
