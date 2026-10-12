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

// Package restyv3 instruments the resty.dev/v3 package
// (https://github.com/go-resty/resty) with OpenTelemetry tracing.
//
// Call TraceClient with a resty v3 client to create one client span per
// request attempt. Retried requests produce one span per attempt, each one
// chained to the span of the previous attempt.
//
//	cli := resty.New()
//	restyv3.TraceClient(cli, restyv3.WithTracerName("my-tracer"))
//
// For go-resty/resty/v2 clients, use the github.com/dubonzi/otelresty package.
package restyv3 // import "github.com/dubonzi/otelresty/restyv3"
