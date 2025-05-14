//go:build ignore

// Copyright (c) 2024 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package pkg

import (
	"context"
	"errors"
	"fmt"
	"github.com/alibaba/opentelemetry-go-auto-instrumentation/pkg/inst-api-semconv/instrumenter/db"
	"github.com/alibaba/opentelemetry-go-auto-instrumentation/pkg/inst-api-semconv/instrumenter/experimental"
	"github.com/alibaba/opentelemetry-go-auto-instrumentation/pkg/inst-api-semconv/instrumenter/rpc"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"log"
	http2 "net/http"
	"os"
	"runtime"
	"strings"

	"github.com/alibaba/opentelemetry-go-auto-instrumentation/pkg/core/meter"
	"github.com/alibaba/opentelemetry-go-auto-instrumentation/pkg/inst-api-semconv/instrumenter/http"
	testaccess "github.com/alibaba/opentelemetry-go-auto-instrumentation/pkg/testaccess"
	otelruntime "go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/contrib/propagators/b3"
	"go.opentelemetry.io/otel"
	_ "go.opentelemetry.io/otel/baggage"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/exporters/zipkin"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// set the following environment variables based on https://opentelemetry.io/docs/specs/otel/configuration/sdk-environment-variables
// your service name: OTEL_SERVICE_NAME
// your otlp endpoint: OTEL_EXPORTER_OTLP_ENDPOINT OTEL_EXPORTER_OTLP_TRACES_ENDPOINT OTEL_EXPORTER_OTLP_METRICS_ENDPOINT OTEL_EXPORTER_OTLP_LOGS_ENDPOINT
// your otlp header: OTEL_EXPORTER_OTLP_HEADERS
// your otlp propagate contribution: OTEL_PROPAGATORS
const exec_name = "otel"
const report_protocol = "OTEL_EXPORTER_OTLP_PROTOCOL"
const trace_report_protocol = "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"
const metrics_exporter = "OTEL_METRICS_EXPORTER"
const trace_exporter = "OTEL_TRACES_EXPORTER"
const prometheus_exporter_port = "OTEL_EXPORTER_PROMETHEUS_PORT"
const propagators = "OTEL_PROPAGATORS"
const default_prometheus_exporter_port = "9464"

var (
	metricExporter     metric.Exporter
	spanExporter       trace.SpanExporter
	traceProvider      *trace.TracerProvider
	metricsProvider    *metric.MeterProvider
	batchSpanProcessor trace.SpanProcessor
)

func init() {
	if testaccess.IsInTest() {
		trace.GetTestSpans = testaccess.GetTestSpans
		metric.GetTestMetrics = testaccess.GetTestMetrics
		trace.ResetTestSpans = testaccess.ResetTestSpans
	}
	ctx := context.Background()
	// graceful shutdown
	runtime.ExitHook = func() {
		gracefullyShutdown(ctx)
	}
	path, err := os.Executable()
	if err != nil {
		panic(err)
	}
	// skip when the executable is otel itself
	if strings.HasSuffix(path, exec_name) {
		return
	}
	if err = initOpenTelemetry(ctx); err != nil {
		log.Fatalf("%s: %v", "Failed to initialize opentelemetry resource", err)
	}
}

func newSpanProcessor(ctx context.Context) trace.SpanProcessor {
	if testaccess.IsInTest() {
		traceExporter := testaccess.GetSpanExporter()
		// in test, we just send the span immediately
		simpleProcessor := trace.NewSimpleSpanProcessor(traceExporter)
		return simpleProcessor
	} else {
		var err error
		if os.Getenv(trace_exporter) == "none" {
			spanExporter = tracetest.NewNoopExporter()
		} else if os.Getenv(trace_exporter) == "console" {
			spanExporter, err = stdouttrace.New()
		} else if os.Getenv(trace_exporter) == "zipkin" {
			spanExporter, err = zipkin.New("")
		} else {
			if os.Getenv(report_protocol) == "grpc" || os.Getenv(trace_report_protocol) == "grpc" {
				spanExporter, err = otlptrace.New(ctx, otlptracegrpc.NewClient())
			} else {
				spanExporter, err = otlptrace.New(ctx, otlptracehttp.NewClient())
			}
		}
		if err != nil {
			log.Fatalf("%s: %v", "Failed to create the OpenTelemetry trace exporter", err)
		}
		batchSpanProcessor = trace.NewBatchSpanProcessor(spanExporter)
		return batchSpanProcessor
	}
}

func initOpenTelemetry(ctx context.Context) error {

	batchSpanProcessor = newSpanProcessor(ctx)

	if batchSpanProcessor != nil {
		traceProvider = trace.NewTracerProvider(
			trace.WithSpanProcessor(batchSpanProcessor))
	} else {
		traceProvider = trace.NewTracerProvider()
	}

	otel.SetTracerProvider(traceProvider)
	initPropagators()
	return initMetrics()
}

func initPropagators() {
	var (
		b3Encodings b3.Encoding = 0
	)
	propagator := os.Getenv(propagators)
	if propagator == "" {
		propagator = "tracecontext,baggage"
	}
	propagatorStringList := strings.Split(propagator, ",")
	propagatorList := make([]propagation.TextMapPropagator, 0)
	for _, p := range propagatorStringList {
		switch p {
		case "tracecontext":
			propagatorList = append(propagatorList, propagation.TraceContext{})
		case "baggage":
			propagatorList = append(propagatorList, propagation.Baggage{})
		case "b3":
			b3Encodings = b3Encodings | b3.B3SingleHeader
		case "b3multi":
			b3Encodings = b3Encodings | b3.B3MultipleHeader
		default:
			log.Printf("Unknown propagator: %s", p)
		}
	}

	if b3Encodings > 0 {
		b3Propagator := b3.New(b3.WithInjectEncoding(b3Encodings))
		propagatorList = append(propagatorList, b3Propagator)
	}

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagatorList...))
}

func initMetrics() error {
	ctx := context.Background()
	// TODO: abstract the if-else
	var err error
	if testaccess.IsInTest() {
		metricsProvider = metric.NewMeterProvider(
			metric.WithReader(testaccess.ManualReader),
		)
	} else {
		if os.Getenv(metrics_exporter) == "console" {
			metricExporter, err = stdoutmetric.New()
			metricsProvider = metric.NewMeterProvider(
				metric.WithReader(metric.NewPeriodicReader(metricExporter)),
			)
		} else if os.Getenv(metrics_exporter) == "prometheus" {
			promExporter, err := prometheus.New()
			if err != nil {
				log.Fatalf("Failed to create prometheus metric exporter: %v", err)
			}
			metricsProvider = metric.NewMeterProvider(
				metric.WithReader(promExporter),
			)
			go serveMetrics()
		} else {
			if os.Getenv(report_protocol) == "grpc" || os.Getenv(trace_report_protocol) == "grpc" {
				metricExporter, err = otlpmetricgrpc.New(ctx)
				metricsProvider = metric.NewMeterProvider(
					metric.WithReader(metric.NewPeriodicReader(metricExporter)),
				)
			} else {
				metricExporter, err = otlpmetrichttp.New(ctx)
				metricsProvider = metric.NewMeterProvider(
					metric.WithReader(metric.NewPeriodicReader(metricExporter)),
				)
			}
		}
	}
	if err != nil {
		log.Fatalf("Failed to create metric exporter: %v", err)
	}
	if metricsProvider == nil {
		return errors.New("No MeterProvider is provided")
	}
	otel.SetMeterProvider(metricsProvider)
	m := metricsProvider.Meter("opentelemetry-global-meter")
	meter.SetMeter(m)
	// init http metrics
	http.InitHttpMetrics(m)
	// init rpc metrics
	rpc.InitRpcMetrics(m)
	// init db metrics
	db.InitDbMetrics(m)
	// nacos experimental metrics
	experimental.InitNacosExperimentalMetrics(m)
	// DefaultMinimumReadMemStatsInterval is 15 second
	return otelruntime.Start(otelruntime.WithMeterProvider(metricsProvider))
}

func serveMetrics() {
	http2.Handle("/metrics", promhttp.Handler())
	port := os.Getenv(prometheus_exporter_port)
	if port == "" {
		port = default_prometheus_exporter_port
	}
	log.Printf("serving serveMetrics at localhost:%s/metrics", port)
	err := http2.ListenAndServe(fmt.Sprintf(":%s", port), nil)
	if err != nil {
		fmt.Printf("error serving serveMetrics: %v", err)
		return
	}
}

func gracefullyShutdown(ctx context.Context) {
	if metricsProvider != nil {
		if err := metricsProvider.Shutdown(ctx); err != nil {
			log.Printf("%s: %v", "Failed to shutdown the OpenTelemetry metric provider", err)
		}
	}
	if traceProvider != nil {
		if err := traceProvider.Shutdown(ctx); err != nil {
			log.Printf("%s: %v", "Failed to shutdown the OpenTelemetry trace provider", err)
		}
	}
	if spanExporter != nil {
		if err := spanExporter.Shutdown(ctx); err != nil {
			log.Printf("%s: %v", "Failed to shutdown the OpenTelemetry span exporter", err)
		}
	}
	if metricExporter != nil {
		if err := metricExporter.Shutdown(ctx); err != nil {
			log.Printf("%s: %v", "Failed to shutdown the OpenTelemetry metric exporter", err)
		}
	}
	if batchSpanProcessor != nil {
		if err := batchSpanProcessor.Shutdown(ctx); err != nil {
			log.Printf("%s: %v", "Failed to shutdown the OpenTelemetry batch span processor", err)
		}
	}
}
