// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package loadbalancingexporter // import "github.com/open-telemetry/opentelemetry-collector-contrib/exporter/loadbalancingexporter"

import (
	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/kafka"
	"go.opentelemetry.io/collector/exporter/exporterhelper"
	"time"

	"github.com/open-telemetry/opentelemetry-collector-contrib/exporter/kafkaexporter"
	"go.opentelemetry.io/collector/exporter/otlpexporter"
)

type routingKey int

const (
	traceIDRouting routingKey = iota
	svcRouting
	metricNameRouting
	resourceRouting
)

// Config defines configuration for the exporter.
type Config struct {
	Protocol   Protocol         `mapstructure:"protocol"`
	Resolver   ResolverSettings `mapstructure:"resolver"`
	RoutingKey string           `mapstructure:"routing_key"`
}

// Protocol holds the individual protocol-specific settings. Only OTLP is supported at the moment.
type Protocol struct {
	OTLP  *otlpexporter.Config  `mapstructure:"otlp"`
	Kafka *kafkaexporter.Config `mapstructure:"kafka"`
}

// ResolverSettings defines the configurations for the backend resolver
type ResolverSettings struct {
	Static *StaticResolver `mapstructure:"static"`
	DNS    *DNSResolver    `mapstructure:"dns"`
	K8sSvc *K8sSvcResolver `mapstructure:"k8s"`
	Kafka  *KafkaResolver  `mapstructure:"kafka"`
}

// StaticResolver defines the configuration for the resolver providing a fixed list of backends
type StaticResolver struct {
	Hostnames []string `mapstructure:"hostnames"`
}

// DNSResolver defines the configuration for the DNS resolver
type DNSResolver struct {
	Hostname string        `mapstructure:"hostname"`
	Port     string        `mapstructure:"port"`
	Interval time.Duration `mapstructure:"interval"`
	Timeout  time.Duration `mapstructure:"timeout"`
}

// K8sSvcResolver defines the configuration for the DNS resolver
type K8sSvcResolver struct {
	Service string  `mapstructure:"service"`
	Ports   []int32 `mapstructure:"ports"`
}

// KafkaResolver defines the configuration for the Kafka resolver
type KafkaResolver struct {
	exporterhelper.TimeoutSettings `mapstructure:",squash"` // squash ensures fields are correctly decoded in embedded struct.

	// The list of kafka brokers (default localhost:9092)
	Brokers []string `mapstructure:"brokers"`

	// ResolveCanonicalBootstrapServersOnly makes Sarama do a DNS lookup for
	// each of the provided brokers. It will then do a PTR lookup for each
	// returned IP, and that set of names becomes the broker list. This can be
	// required in SASL environments.
	ResolveCanonicalBootstrapServersOnly bool `mapstructure:"resolve_canonical_bootstrap_servers_only"`

	// Kafka protocol version
	ProtocolVersion string `mapstructure:"protocol_version"`

	// ClientID to configure the Kafka client with. This can be leveraged by
	// Kafka to enforce ACLs, throttling quotas, and more.
	ClientID string `mapstructure:"client_id"`

	// Encoding of messages (default "otlp_proto")
	Encoding string `mapstructure:"encoding"`

	// Metadata is the namespace for metadata management properties used by the
	// Client, and shared by the Producer/Consumer.
	Metadata kafkaexporter.Metadata `mapstructure:"metadata"`

	// Authentication defines used authentication mechanism.
	Authentication kafka.Authentication `mapstructure:"auth"`

	TopicPrefix string `mapstructure:"topic_prefix"`
}
