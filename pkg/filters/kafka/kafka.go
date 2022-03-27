/*
 * Copyright (c) 2017, MegaEase
 * All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package kafka

import (
	"fmt"

	"github.com/Shopify/sarama"
	"github.com/megaease/easegress/pkg/context"
	"github.com/megaease/easegress/pkg/filters"
	"github.com/megaease/easegress/pkg/logger"
	"github.com/megaease/easegress/pkg/object/pipeline"
)

const (
	// Kind is the kind of Kafka
	Kind = "Kafka"

	resultGetDataFailed = "GetDataFailed"
)

var kind = &filters.Kind{
	Name:        Kind,
	Description: "Kafka is a backend of MQTTProxy",
	Results:     []string{resultGetDataFailed},
	DefaultSpec: func() filters.Spec {
		return &Spec{}
	},
	CreateInstance: func() filters.Filter {
		return &Kafka{}
	},
}

func init() {
	filters.Register(kind)
}

type (
	// Kafka is kafka backend for MQTT proxy
	Kafka struct {
		spec     *Spec
		producer sarama.AsyncProducer
		done     chan struct{}

		defaultTopic string
		topicKey     string
		headerKey    string
		payloadKey   string
	}
)

var _ filters.Filter = (*Kafka)(nil)
var _ pipeline.MQTTFilter = (*Kafka)(nil)

// Name returns the name of the Kafka filter instance.
func (k *Kafka) Name() string {
	return k.spec.Name()
}

// Kind return kind of Kafka
func (k *Kafka) Kind() *filters.Kind {
	return kind
}

// Spec returns the spec used by the Kafka
func (k *Kafka) Spec() filters.Spec {
	return k.spec
}

func (k *Kafka) setKV() {
	kv := k.spec.KVMap
	if kv != nil {
		k.topicKey = kv.TopicKey
		k.headerKey = kv.HeaderKey
		k.payloadKey = kv.PayloadKey
	}
	if k.spec.Topic != nil {
		k.defaultTopic = k.spec.Topic.Default
	}
}

func (k *Kafka) setProducer() {
	config := sarama.NewConfig()
	config.ClientID = k.spec.Name()
	config.Version = sarama.V1_0_0_0
	producer, err := sarama.NewAsyncProducer(k.spec.Backend, config)
	if err != nil {
		panic(fmt.Errorf("start sarama producer with address %v failed: %v", k.spec.Backend, err))
	}
	k.producer = producer

	go func() {
		for {
			select {
			case <-k.done:
				err := producer.Close()
				if err != nil {
					logger.Errorf("close kafka producer failed: %v", err)
				}
				return
			case err, ok := <-producer.Errors():
				if !ok {
					return
				}
				logger.SpanErrorf(nil, "sarama producer failed: %v", err)
			}
		}
	}()
}

// Init init Kafka
func (k *Kafka) Init(spec filters.Spec) {
	if spec.Protocol() != context.MQTT {
		panic("filter Kafka only support MQTT protocol for now")
	}
	k.spec = spec.(*Spec)
	k.done = make(chan struct{})
	k.setKV()
	k.setProducer()
}

// Inherit init Kafka based on previous generation
func (k *Kafka) Inherit(spec filters.Spec, previousGeneration filters.Filter) {
	previousGeneration.Close()
	k.Init(spec)
}

// Close close Kafka
func (k *Kafka) Close() {
	close(k.done)
}

// Status return status of Kafka
func (k *Kafka) Status() interface{} {
	return nil
}

// HandleMQTT handle MQTT context
func (k *Kafka) HandleMQTT(ctx context.MQTTContext) *context.MQTTResult {
	var topic string
	var headers map[string]string
	var payload []byte
	var ok bool

	// set data from kv map
	if k.topicKey != "" {
		topic, ok = ctx.GetKV(k.topicKey).(string)
		if !ok {
			return &context.MQTTResult{ErrString: resultGetDataFailed}
		}
	}
	if k.headerKey != "" {
		headers, ok = ctx.GetKV(k.headerKey).(map[string]string)
		if !ok {
			return &context.MQTTResult{ErrString: resultGetDataFailed}
		}
	}
	if k.payloadKey != "" {
		payload, ok = ctx.GetKV(k.payloadKey).([]byte)
		if !ok {
			return &context.MQTTResult{ErrString: resultGetDataFailed}
		}
	}

	// set data from PublishPacket if data is missing
	if ctx.PacketType() == context.MQTTPublish {
		p := ctx.PublishPacket()
		if topic == "" {
			topic = p.TopicName
		}
		if payload == nil {
			payload = p.Payload
		}
	}

	if topic == "" {
		topic = k.defaultTopic
	}

	if topic == "" || payload == nil {
		return &context.MQTTResult{ErrString: resultGetDataFailed}
	}

	kafkaHeaders := []sarama.RecordHeader{}
	for k, v := range headers {
		kafkaHeaders = append(kafkaHeaders, sarama.RecordHeader{Key: []byte(k), Value: []byte(v)})
	}

	msg := &sarama.ProducerMessage{
		Topic:   topic,
		Headers: kafkaHeaders,
		Value:   sarama.ByteEncoder(payload),
	}
	k.producer.Input() <- msg
	return &context.MQTTResult{}
}