// Copyright 2017 Authors of Cilium
//
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

package proxy

import (
	"fmt"
	"testing"
	"time"

	"github.com/cilium/cilium/pkg/identity"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/policy"
	"github.com/cilium/cilium/pkg/policy/api"
	"github.com/cilium/cilium/pkg/proxy/logger"

	"github.com/optiopay/kafka"
	"github.com/optiopay/kafka/proto"
	"github.com/sirupsen/logrus"

	. "gopkg.in/check.v1"
)

// Hook up gocheck into the "go test" runner.
func Test(t *testing.T) {
	TestingT(t)
}

type proxyTestSuite struct{}

var _ = Suite(&proxyTestSuite{})

var (
	localEndpointMock logger.EndpointUpdater = &proxyUpdaterMock{
		id:       1000,
		ipv4:     "10.0.0.1",
		ipv6:     "f00d::1",
		labels:   []string{"id.foo", "id.bar"},
		identity: identity.NumericIdentity(256),
	}
)

// newTestBrokerConf returns BrokerConf with default configuration adjusted for
// tests
func newTestBrokerConf(clientID string) kafka.BrokerConf {
	conf := kafka.NewBrokerConf(clientID)
	conf.DialTimeout = 400 * time.Millisecond
	conf.LeaderRetryLimit = 10
	conf.LeaderRetryWait = 2 * time.Millisecond
	return conf
}

type loggerMap struct{}

func fields(args ...interface{}) logrus.Fields {
	fields := logrus.Fields{}
	for i := 0; i+1 < len(args); i += 2 {
		fields[args[i].(string)] = args[i+1]
	}
	return fields
}

func (loggerMap) Debug(msg string, args ...interface{}) { log.WithFields(fields(args...)).Debug(msg) }
func (loggerMap) Info(msg string, args ...interface{})  { log.WithFields(fields(args...)).Info(msg) }
func (loggerMap) Warn(msg string, args ...interface{})  { log.WithFields(fields(args...)).Warn(msg) }
func (loggerMap) Error(msg string, args ...interface{}) { log.WithFields(fields(args...)).Error(msg) }

var (
	proxyAddress, proxyPort = "127.0.0.1", 15000
)

type metadataTester struct {
	host               string
	port               int
	topics             map[string]bool
	allowCreate        bool
	numGeneralFetches  int
	numSpecificFetches int
}

func newMetadataHandler(srv *Server, allowCreate bool) *metadataTester {
	tester := &metadataTester{
		host:        proxyAddress,
		port:        proxyPort,
		allowCreate: allowCreate,
		topics:      make(map[string]bool),
	}
	tester.topics["allowedTopic"] = true
	tester.topics["disallowedTopic"] = true
	return tester
}

func (m *metadataTester) NumGeneralFetches() int {
	return m.numGeneralFetches
}

func (m *metadataTester) NumSpecificFetches() int {
	return m.numSpecificFetches
}

func (m *metadataTester) Handler() RequestHandler {
	return func(request Serializable) Serializable {
		req := request.(*proto.MetadataReq)

		if len(req.Topics) == 0 {
			m.numGeneralFetches++
		} else {
			m.numSpecificFetches++
		}

		resp := &proto.MetadataResp{
			CorrelationID: req.CorrelationID,
			Brokers: []proto.MetadataRespBroker{
				{NodeID: 1, Host: m.host, Port: int32(m.port)},
			},
			Topics: []proto.MetadataRespTopic{},
		}

		wantsTopic := make(map[string]bool)
		for _, topic := range req.Topics {
			if m.allowCreate {
				m.topics[topic] = true
			}
			wantsTopic[topic] = true
		}

		for topic := range m.topics {
			// Return either all topics or only topics that they explicitly requested
			_, explicitTopic := wantsTopic[topic]
			if len(req.Topics) > 0 && !explicitTopic {
				continue
			}

			resp.Topics = append(resp.Topics, proto.MetadataRespTopic{
				Name: topic,
				Partitions: []proto.MetadataRespPartition{
					{
						ID:       0,
						Leader:   1,
						Replicas: []int32{1},
						Isrs:     []int32{1},
					},
					{
						ID:       1,
						Leader:   1,
						Replicas: []int32{1},
						Isrs:     []int32{1},
					},
				},
			})
		}
		return resp
	}
}

func (k *proxyTestSuite) TestKafkaRedirect(c *C) {
	// this isn't thread safe but there is no function to get it
	// SetLevel is atomic, however.
	oldLevel := log.Level
	defer log.SetLevel(oldLevel)
	log.SetLevel(logrus.DebugLevel)

	server := NewServer()
	server.Start()
	defer server.Close()

	log.WithFields(logrus.Fields{
		"address": server.Address(),
	}).Debug("Started kafka server")

	proxyAddress := fmt.Sprintf("%s:%d", proxyAddress, uint16(proxyPort))

	kafkaRule1 := api.PortRuleKafka{APIKey: "metadata", APIVersion: "0"}
	c.Assert(kafkaRule1.Sanitize(), IsNil)

	kafkaRule2 := api.PortRuleKafka{APIKey: "produce", APIVersion: "0", Topic: "allowedTopic"}
	c.Assert(kafkaRule2.Sanitize(), IsNil)

	r := newRedirect(localEndpointMock, "foo")
	r.ProxyPort = uint16(proxyPort)
	r.ingress = true

	r.rules = policy.L7DataMap{
		api.WildcardEndpointSelector: api.L7Rules{
			Kafka: []api.PortRuleKafka{kafkaRule1, kafkaRule2},
		},
	}

	redir, err := createKafkaRedirect(r, kafkaConfiguration{
		lookupNewDest: func(remoteAddr string, dport uint16) (uint32, string, error) {
			return uint32(200), server.Address(), nil
		},
		// Disable use of SO_MARK
		noMarker: true,
	}, DefaultEndpointInfoRegistry)
	c.Assert(err, IsNil)
	defer redir.Close(nil)

	log.WithFields(logrus.Fields{
		"address": proxyAddress,
	}).Debug("Started kafka proxy")

	server.Handle(MetadataRequest, newMetadataHandler(server, false).Handler())

	broker, err := kafka.Dial([]string{proxyAddress}, newTestBrokerConf("tester"))
	if err != nil {
		c.Fatalf("cannot create broker: %s", err)
	}

	// setup producer
	prodConf := kafka.NewProducerConf()
	prodConf.RetryWait = time.Millisecond
	prodConf.Logger = loggerMap{}
	producer := broker.Producer(prodConf)
	messages := []*proto.Message{
		{Value: []byte("first")},
		{Value: []byte("second")},
	}

	// Start handling allowedTopic produce requests
	server.Handle(ProduceRequest, func(request Serializable) Serializable {
		req := request.(*proto.ProduceReq)
		log.WithField(logfields.Request, logfields.Repr(req)).Debug("Handling req")
		return &proto.ProduceResp{
			CorrelationID: req.CorrelationID,
			Topics: []proto.ProduceRespTopic{
				{
					Name: req.Topics[0].Name,
					Partitions: []proto.ProduceRespPartition{
						{
							ID:     0,
							Offset: 5,
						},
					},
				},
			},
		}
	})

	// send a Produce request for an allowed topic
	offset, err := producer.Produce("allowedTopic", 0, messages...)
	c.Assert(err, IsNil)
	c.Assert(offset, Equals, int64(5))

	// send a Produce request for disallowed topic
	_, err = producer.Produce("disallowedTopic", 0, messages...)
	c.Assert(err, Equals, proto.ErrTopicAuthorizationFailed)

	log.Debug("Testing done, closing listen socket")
	redir.Close(nil)

	// In order to see in the logs that the connections get closed after the
	// 1-minute timeout, uncomment this line:
	// time.Sleep(2 * time.Minute)
}
