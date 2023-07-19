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
