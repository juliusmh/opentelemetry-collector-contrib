// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package loadbalancingexporter // import "github.com/open-telemetry/opentelemetry-collector-contrib/exporter/loadbalancingexporter"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/IBM/sarama"
	"log"
	"os"
	"sync"
	"time"

	"go.opencensus.io/stats"
	"go.opencensus.io/tag"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/exporter"
	"go.opentelemetry.io/collector/exporter/otlpexporter"
	"go.opentelemetry.io/collector/pdata/ptrace"
	"go.uber.org/multierr"

	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/kafkaexporter"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/batchpersignal"
)

var _ exporter.Traces = (*traceExporterImp)(nil)

type exporterTraces map[component.Component]map[string]ptrace.Traces
type endpointTraces map[string]ptrace.Traces

type traceExporterImp struct {
	loadBalancer loadBalancer
	routingKey   routingKey

	stopped    bool
	shutdownWg sync.WaitGroup
}

// Create new traces exporter
func newTracesExporter(params exporter.CreateSettings, cfg component.Config) (*traceExporterImp, error) {
	exporterFactory := buildFactory(cfg.(*Config))

	sarama.Logger = log.New(os.Stdout, "[sarama] ", log.LstdFlags)

	lb, err := newLoadBalancer(params, cfg, func(ctx context.Context, endpoint string) (component.Component, error) {
		oCfg := buildExporterConfig(cfg.(*Config), endpoint)
		return exporterFactory.CreateTracesExporter(ctx, params, oCfg)
	})
	if err != nil {
		return nil, err
	}

	traceExporter := traceExporterImp{loadBalancer: lb, routingKey: traceIDRouting}

	switch cfg.(*Config).RoutingKey {
	case "service":
		traceExporter.routingKey = svcRouting
	case "traceID", "":
	default:
		return nil, fmt.Errorf("unsupported routing_key: %s", cfg.(*Config).RoutingKey)
	}
	return &traceExporter, nil
}

func buildFactory(cfg *Config) exporter.Factory {
	if cfg.Protocol.OTLP != nil {
		fmt.Println("using otlp factory")
		return otlpexporter.NewFactory()
	}
	if cfg.Protocol.Kafka != nil {
		fmt.Println("using kafka factory")
		return kafkaexporter.NewFactory()
	}
	return nil
}

func buildExporterConfig(cfg *Config, endpoint string) component.Config {
	if cfg.Protocol.OTLP != nil {
		fmt.Println("using otlp exporter")
		oCfg := cfg.Protocol.OTLP
		oCfg.Endpoint = endpoint
		return oCfg
	}
	if cfg.Protocol.Kafka != nil {
		fmt.Println("using kafka exporter", "topic:", endpoint)
		kCfg := cfg.Protocol.Kafka
		kCfg.Topic = endpoint
		json.NewEncoder(os.Stdout).Encode(kCfg)
		return kCfg
	}
	return nil
}

func (e *traceExporterImp) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}

func (e *traceExporterImp) Start(ctx context.Context, host component.Host) error {
	return e.loadBalancer.Start(ctx, host)
}

func (e *traceExporterImp) Shutdown(context.Context) error {
	e.stopped = true
	e.shutdownWg.Wait()
	return nil
}

func (e *traceExporterImp) ConsumeTraces(ctx context.Context, td ptrace.Traces) error {
	var errs error
	var exp component.Component

	batches := batchpersignal.SplitTraces(td)

	exporterSegregatedTraces := make(exporterTraces)
	endpointSegregatedTraces := make(endpointTraces)
	for _, batch := range batches {
		routingID, err := routingIdentifiersFromTraces(batch, e.routingKey)
		if err != nil {
			return err
		}

		for rid := range routingID {
			endpoint := e.loadBalancer.Endpoint([]byte(rid))
			exp, err = e.loadBalancer.Exporter(endpoint)
			if err != nil {
				return err
			}
			_, ok := exp.(exporter.Traces)
			if !ok {
				return fmt.Errorf("unable to export traces, unexpected exporter type: expected exporter.Traces but got %T", exp)
			}

			_, ok = endpointSegregatedTraces[endpoint]
			if !ok {
				endpointSegregatedTraces[endpoint] = ptrace.NewTraces()
			}
			endpointSegregatedTraces[endpoint] = mergeTraces(endpointSegregatedTraces[endpoint], batch)

			_, ok = exporterSegregatedTraces[exp]
			if !ok {
				exporterSegregatedTraces[exp] = endpointTraces{}
			}
			exporterSegregatedTraces[exp][endpoint] = endpointSegregatedTraces[endpoint]
		}
	}

	errs = multierr.Append(errs, e.consumeTrace(ctx, exporterSegregatedTraces))

	return errs
}

func (e *traceExporterImp) consumeTrace(ctx context.Context, exporterSegregatedTraces exporterTraces) error {
	var err error

	for exp, endpointTraces := range exporterSegregatedTraces {
		for endpoint, td := range endpointTraces {
			te, _ := exp.(exporter.Traces)

			start := time.Now()
			err = te.ConsumeTraces(ctx, td)
			duration := time.Since(start)

			if err == nil {
				_ = stats.RecordWithTags(
					ctx,
					[]tag.Mutator{tag.Upsert(endpointTagKey, endpoint), successTrueMutator},
					mBackendLatency.M(duration.Milliseconds()))
			} else {
				_ = stats.RecordWithTags(
					ctx,
					[]tag.Mutator{tag.Upsert(endpointTagKey, endpoint), successFalseMutator},
					mBackendLatency.M(duration.Milliseconds()))
			}
		}
	}

	return err
}

func routingIdentifiersFromTraces(td ptrace.Traces, key routingKey) (map[string]bool, error) {
	ids := make(map[string]bool)
	rs := td.ResourceSpans()
	if rs.Len() == 0 {
		return nil, errors.New("empty resource spans")
	}

	ils := rs.At(0).ScopeSpans()
	if ils.Len() == 0 {
		return nil, errors.New("empty scope spans")
	}

	spans := ils.At(0).Spans()
	if spans.Len() == 0 {
		return nil, errors.New("empty spans")
	}

	if key == svcRouting {
		for i := 0; i < rs.Len(); i++ {
			svc, ok := rs.At(i).Resource().Attributes().Get("service.name")
			if !ok {
				return nil, errors.New("unable to get service name")
			}
			ids[svc.Str()] = true
		}
		return ids, nil
	}
	tid := spans.At(0).TraceID()
	ids[string(tid[:])] = true
	return ids, nil
}
