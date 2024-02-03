// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package loadbalancingexporter // import "github.com/open-telemetry/opentelemetry-collector-contrib/exporter/loadbalancingexporter"

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"go.opencensus.io/stats"
	"go.opencensus.io/tag"
	"go.uber.org/zap"

	"github.com/IBM/sarama"
	"github.com/open-telemetry/opentelemetry-collector-contrib/internal/kafka"
)

var _ resolver = (*kafkaResolver)(nil)

var (
	kafkaResolverMutator = tag.Upsert(tag.MustNewKey("resolver"), "kafka")
)

type kafkaResolver struct {
	logger *zap.Logger
	admin  sarama.ClusterAdmin

	topicPrefix string

	hostname    string
	port        string
	resolver    netResolver
	resInterval time.Duration
	resTimeout  time.Duration

	topics            []string
	onChangeCallbacks []func([]string)

	stopCh             chan (struct{})
	updateLock         sync.Mutex
	shutdownWg         sync.WaitGroup
	changeCallbackLock sync.RWMutex
}

func newKafkaResolver(logger *zap.Logger, config *KafkaResolver, topicPrefix string) (*kafkaResolver, error) {
	c := sarama.NewConfig()

	c.ClientID = config.ClientID

	// These setting are required by the sarama.SyncProducer implementation.
	c.Admin.Timeout = config.Timeout
	c.Admin.Retry.Max = config.Metadata.Retry.Max
	c.Admin.Retry.Backoff = config.Metadata.Retry.Backoff
	c.Metadata.Full = config.Metadata.Full
	c.Metadata.Retry.Max = config.Metadata.Retry.Max
	c.Metadata.Retry.Backoff = config.Metadata.Retry.Backoff

	if config.ResolveCanonicalBootstrapServersOnly {
		c.Net.ResolveCanonicalBootstrapServers = true
	}

	if config.ProtocolVersion != "" {
		version, err := sarama.ParseKafkaVersion(config.ProtocolVersion)
		if err != nil {
			return nil, err
		}
		c.Version = version
	}

	if err := kafka.ConfigureAuthentication(config.Authentication, c); err != nil {
		return nil, err
	}

	admin, err := sarama.NewClusterAdmin(config.Brokers, c)
	if err != nil {
		return nil, err
	}

	return &kafkaResolver{
		logger:      logger,
		admin:       admin,
		topicPrefix: topicPrefix,
		resInterval: 5 * time.Second,
	}, nil
}

func (r *kafkaResolver) start(ctx context.Context) error {
	if _, err := r.resolve(ctx); err != nil {
		r.logger.Warn("failed to resolve", zap.Error(err))
	}

	go r.periodicallyResolve()

	r.logger.Debug("Kafka resolver started",
		zap.Duration("interval", r.resInterval), zap.Duration("timeout", r.resTimeout))
	return nil
}

func (r *kafkaResolver) shutdown(_ context.Context) error {
	r.changeCallbackLock.Lock()
	r.onChangeCallbacks = nil
	r.changeCallbackLock.Unlock()

	close(r.stopCh)
	r.shutdownWg.Wait()
	return nil
}

func (r *kafkaResolver) periodicallyResolve() {
	ticker := time.NewTicker(r.resInterval)

	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), r.resTimeout)
			if _, err := r.resolve(ctx); err != nil {
				r.logger.Warn("failed to resolve", zap.Error(err))
			} else {
				r.logger.Debug("resolved successfully")
			}
			cancel()
		case <-r.stopCh:
			return
		}
	}
}

func (r *kafkaResolver) resolve(ctx context.Context) ([]string, error) {
	r.shutdownWg.Add(1)
	defer r.shutdownWg.Done()

	listTopicsResponse, err := r.admin.ListTopics()
	if err != nil {
		_ = stats.RecordWithTags(ctx, resolverSuccessFalseMutators, mNumResolutions.M(1))
		return nil, err
	}

	_ = stats.RecordWithTags(ctx, resolverSuccessTrueMutators, mNumResolutions.M(1))

	topics := make([]string, 0)
	for topic, _ := range listTopicsResponse {
		if strings.HasPrefix(topic, r.topicPrefix) {
			topics = append(topics, topic)
		}
	}

	// keep it always in the same order
	sort.Strings(topics)

	if equalStringSlice(r.topics, topics) {
		return r.topics, nil
	}

	// the list has changed!
	r.updateLock.Lock()
	r.topics = topics
	r.updateLock.Unlock()
	_ = stats.RecordWithTags(ctx, resolverSuccessTrueMutators, mNumBackends.M(int64(len(topics))))

	// propagate the change
	r.changeCallbackLock.RLock()
	for _, callback := range r.onChangeCallbacks {
		callback(r.topics)
	}
	r.changeCallbackLock.RUnlock()

	return r.topics, nil
}

func (r *kafkaResolver) onChange(f func([]string)) {
	r.changeCallbackLock.Lock()
	defer r.changeCallbackLock.Unlock()
	r.onChangeCallbacks = append(r.onChangeCallbacks, f)
}
