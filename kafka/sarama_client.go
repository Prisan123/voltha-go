/*
 * Copyright 2018-present Open Networking Foundation

 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at

 * http://www.apache.org/licenses/LICENSE-2.0

 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
package kafka

import (
	"errors"
	"fmt"
	scc "github.com/bsm/sarama-cluster"
	"github.com/golang/protobuf/proto"
	"github.com/google/uuid"
	"github.com/opencord/voltha-go/common/log"
	ca "github.com/opencord/voltha-go/protos/core_adapter"
	"gopkg.in/Shopify/sarama.v1"
	"strings"
	"sync"
	"time"
)

func init() {
	log.AddPackage(log.JSON, log.WarnLevel, nil)
}

type returnErrorFunction func() error

// consumerChannels represents one or more consumers listening on a kafka topic.  Once a message is received on that
// topic, the consumer(s) broadcasts the message to all the listening channels.   The consumer can be a partition
//consumer or a group consumer
type consumerChannels struct {
	consumers []interface{}
	channels  []chan *ca.InterContainerMessage
}

// SaramaClient represents the messaging proxy
type SaramaClient struct {
	cAdmin                        sarama.ClusterAdmin
	client                        sarama.Client
	KafkaHost                     string
	KafkaPort                     int
	producer                      sarama.AsyncProducer
	consumer                      sarama.Consumer
	groupConsumer                 *scc.Consumer
	consumerType                  int
	groupName                     string
	producerFlushFrequency        int
	producerFlushMessages         int
	producerFlushMaxmessages      int
	producerRetryMax              int
	producerRetryBackOff          time.Duration
	producerReturnSuccess         bool
	producerReturnErrors          bool
	consumerMaxwait               int
	maxProcessingTime             int
	numPartitions                 int
	numReplicas                   int
	autoCreateTopic               bool
	doneCh                        chan int
	topicToConsumerChannelMap     map[string]*consumerChannels
	lockTopicToConsumerChannelMap sync.RWMutex
}

type SaramaClientOption func(*SaramaClient)

func Host(host string) SaramaClientOption {
	return func(args *SaramaClient) {
		args.KafkaHost = host
	}
}

func Port(port int) SaramaClientOption {
	return func(args *SaramaClient) {
		args.KafkaPort = port
	}
}

func ConsumerType(consumer int) SaramaClientOption {
	return func(args *SaramaClient) {
		args.consumerType = consumer
	}
}

func ProducerFlushFrequency(frequency int) SaramaClientOption {
	return func(args *SaramaClient) {
		args.producerFlushFrequency = frequency
	}
}

func ProducerFlushMessages(num int) SaramaClientOption {
	return func(args *SaramaClient) {
		args.producerFlushMessages = num
	}
}

func ProducerFlushMaxMessages(num int) SaramaClientOption {
	return func(args *SaramaClient) {
		args.producerFlushMaxmessages = num
	}
}

func ReturnOnErrors(opt bool) SaramaClientOption {
	return func(args *SaramaClient) {
		args.producerReturnErrors = opt
	}
}

func ReturnOnSuccess(opt bool) SaramaClientOption {
	return func(args *SaramaClient) {
		args.producerReturnSuccess = opt
	}
}

func ConsumerMaxWait(wait int) SaramaClientOption {
	return func(args *SaramaClient) {
		args.consumerMaxwait = wait
	}
}

func MaxProcessingTime(pTime int) SaramaClientOption {
	return func(args *SaramaClient) {
		args.maxProcessingTime = pTime
	}
}

func NumPartitions(number int) SaramaClientOption {
	return func(args *SaramaClient) {
		args.numPartitions = number
	}
}

func NumReplicas(number int) SaramaClientOption {
	return func(args *SaramaClient) {
		args.numReplicas = number
	}
}

func AutoCreateTopic(opt bool) SaramaClientOption {
	return func(args *SaramaClient) {
		args.autoCreateTopic = opt
	}
}

func NewSaramaClient(opts ...SaramaClientOption) *SaramaClient {
	client := &SaramaClient{
		KafkaHost: DefaultKafkaHost,
		KafkaPort: DefaultKafkaPort,
	}
	client.consumerType = DefaultConsumerType
	client.producerFlushFrequency = DefaultProducerFlushFrequency
	client.producerFlushMessages = DefaultProducerFlushMessages
	client.producerFlushMaxmessages = DefaultProducerFlushMaxmessages
	client.producerReturnErrors = DefaultProducerReturnErrors
	client.producerReturnSuccess = DefaultProducerReturnSuccess
	client.producerRetryMax = DefaultProducerRetryMax
	client.producerRetryBackOff = DefaultProducerRetryBackoff
	client.consumerMaxwait = DefaultConsumerMaxwait
	client.maxProcessingTime = DefaultMaxProcessingTime
	client.numPartitions = DefaultNumberPartitions
	client.numReplicas = DefaultNumberReplicas
	client.autoCreateTopic = DefaultAutoCreateTopic

	for _, option := range opts {
		option(client)
	}

	client.lockTopicToConsumerChannelMap = sync.RWMutex{}

	return client
}

func (sc *SaramaClient) Start() error {
	log.Info("Starting-kafka-sarama-client")

	// Create the Done channel
	sc.doneCh = make(chan int, 1)

	var err error

	// Create the Cluster Admin
	if err = sc.createClusterAdmin(); err != nil {
		log.Errorw("Cannot-create-cluster-admin", log.Fields{"error": err})
		return err
	}

	// Create the Publisher
	if err := sc.createPublisher(); err != nil {
		log.Errorw("Cannot-create-kafka-publisher", log.Fields{"error": err})
		return err
	}

	// Create the master consumers
	if err := sc.createConsumer(); err != nil {
		log.Errorw("Cannot-create-kafka-consumers", log.Fields{"error": err})
		return err
	}

	// Create the topic to consumers/channel map
	sc.topicToConsumerChannelMap = make(map[string]*consumerChannels)

	return nil
}

func (sc *SaramaClient) Stop() {
	log.Info("stopping-sarama-client")

	//Send a message over the done channel to close all long running routines
	sc.doneCh <- 1

	if sc.producer != nil {
		if err := sc.producer.Close(); err != nil {
			panic(err)
		}
	}

	if sc.consumer != nil {
		if err := sc.consumer.Close(); err != nil {
			panic(err)
		}
	}

	if sc.groupConsumer != nil {
		if err := sc.groupConsumer.Close(); err != nil {
			panic(err)
		}
	}

	if sc.cAdmin != nil {
		if err := sc.cAdmin.Close(); err != nil {
			panic(err)
		}
	}

	//TODO: Clear the consumers map
	sc.clearConsumerChannelMap()

	log.Info("sarama-client-stopped")
}

//CreateTopic creates a topic on the Kafka Broker.
func (sc *SaramaClient) CreateTopic(topic *Topic, numPartition int, repFactor int) error {
	// Set the topic details
	topicDetail := &sarama.TopicDetail{}
	topicDetail.NumPartitions = int32(numPartition)
	topicDetail.ReplicationFactor = int16(repFactor)
	topicDetail.ConfigEntries = make(map[string]*string)
	topicDetails := make(map[string]*sarama.TopicDetail)
	topicDetails[topic.Name] = topicDetail

	if err := sc.cAdmin.CreateTopic(topic.Name, topicDetail, false); err != nil {
		if err == sarama.ErrTopicAlreadyExists {
			//	Not an error
			log.Debugw("topic-already-exist", log.Fields{"topic": topic.Name})
			return nil
		}
		log.Errorw("create-topic-failure", log.Fields{"error": err})
		return err
	}
	// TODO: Wait until the topic has been created.  No API is available in the Sarama clusterAdmin to
	// do so.
	log.Debugw("topic-created", log.Fields{"topic": topic, "numPartition": numPartition, "replicationFactor": repFactor})
	return nil
}

//DeleteTopic removes a topic from the kafka Broker
func (sc *SaramaClient) DeleteTopic(topic *Topic) error {
	// Remove the topic from the broker
	if err := sc.cAdmin.DeleteTopic(topic.Name); err != nil {
		if err == sarama.ErrUnknownTopicOrPartition {
			//	Not an error as does not exist
			log.Debugw("topic-not-exist", log.Fields{"topic": topic.Name})
			return nil
		}
		log.Errorw("delete-topic-failed", log.Fields{"topic": topic, "error": err})
		return err
	}

	// Clear the topic from the consumer channel.  This will also close any consumers listening on that topic.
	if err := sc.clearTopicFromConsumerChannelMap(*topic); err != nil {
		log.Errorw("failure-clearing-channels", log.Fields{"topic": topic, "error": err})
		return err
	}
	return nil
}

// Subscribe registers a caller to a topic. It returns a channel that the caller can use to receive
// messages from that topic
func (sc *SaramaClient) Subscribe(topic *Topic) (<-chan *ca.InterContainerMessage, error) {
	log.Debugw("subscribe", log.Fields{"topic": topic.Name})

	// If a consumers already exist for that topic then resuse it
	if consumerCh := sc.getConsumerChannel(topic); consumerCh != nil {
		log.Debugw("topic-already-subscribed", log.Fields{"topic": topic.Name})
		// Create a channel specific for that consumers and add it to the consumers channel map
		ch := make(chan *ca.InterContainerMessage)
		sc.addChannelToConsumerChannelMap(topic, ch)
		return ch, nil
	}

	// Register for the topic and set it up
	var consumerListeningChannel chan *ca.InterContainerMessage
	var err error

	// Use the consumerType option to figure out the type of consumer to launch
	if sc.consumerType == PartitionConsumer {
		if sc.autoCreateTopic {
			if err = sc.CreateTopic(topic, sc.numPartitions, sc.numReplicas); err != nil {
				log.Errorw("create-topic-failure", log.Fields{"error": err, "topic": topic.Name})
				return nil, err
			}
		}
		if consumerListeningChannel, err = sc.setupPartitionConsumerChannel(topic, sarama.OffsetNewest); err != nil {
			log.Warnw("create-consumers-channel-failure", log.Fields{"error": err, "topic": topic.Name})
			return nil, err
		}
	} else if sc.consumerType == GroupCustomer {
		// TODO: create topic if auto create is on.  There is an issue with the sarama cluster library that
		// does not consume from a precreated topic in some scenarios
		if consumerListeningChannel, err = sc.setupGroupConsumerChannel(topic, "mytest"); err != nil {
			log.Warnw("create-consumers-channel-failure", log.Fields{"error": err, "topic": topic.Name})
			return nil, err
		}
	} else {
		log.Warnw("unknown-consumer-type", log.Fields{"consumer-type": sc.consumerType})
		return nil, errors.New("unknown-consumer-type")
	}

	return consumerListeningChannel, nil
}

//UnSubscribe unsubscribe a consumer from a given topic
func (sc *SaramaClient) UnSubscribe(topic *Topic, ch <-chan *ca.InterContainerMessage) error {
	log.Debugw("unsubscribing-channel-from-topic", log.Fields{"topic": topic.Name})
	err := sc.removeChannelFromConsumerChannelMap(*topic, ch)
	return err
}

// send formats and sends the request onto the kafka messaging bus.
func (sc *SaramaClient) Send(msg interface{}, topic *Topic, keys ...string) error {

	// Assert message is a proto message
	var protoMsg proto.Message
	var ok bool
	// ascertain the value interface type is a proto.Message
	if protoMsg, ok = msg.(proto.Message); !ok {
		log.Warnw("message-not-proto-message", log.Fields{"msg": msg})
		return errors.New(fmt.Sprintf("not-a-proto-msg-%s", msg))
	}

	var marshalled []byte
	var err error
	//	Create the Sarama producer message
	if marshalled, err = proto.Marshal(protoMsg); err != nil {
		log.Errorw("marshalling-failed", log.Fields{"msg": protoMsg, "error": err})
		return err
	}
	key := ""
	if len(keys) > 0 {
		key = keys[0] // Only the first key is relevant
	}
	kafkaMsg := &sarama.ProducerMessage{
		Topic: topic.Name,
		Key:   sarama.StringEncoder(key),
		Value: sarama.ByteEncoder(marshalled),
	}

	// Send message to kafka
	sc.producer.Input() <- kafkaMsg
	return nil
}

func (sc *SaramaClient) createClusterAdmin() error {
	kafkaFullAddr := fmt.Sprintf("%s:%d", sc.KafkaHost, sc.KafkaPort)
	config := sarama.NewConfig()
	config.Version = sarama.V1_0_0_0

	// Create a cluster Admin
	var cAdmin sarama.ClusterAdmin
	var err error
	if cAdmin, err = sarama.NewClusterAdmin([]string{kafkaFullAddr}, config); err != nil {
		log.Errorw("cluster-admin-failure", log.Fields{"error": err, "broker-address": kafkaFullAddr})
		return err
	}
	sc.cAdmin = cAdmin
	return nil
}

func (sc *SaramaClient) addTopicToConsumerChannelMap(id string, arg *consumerChannels) {
	sc.lockTopicToConsumerChannelMap.Lock()
	defer sc.lockTopicToConsumerChannelMap.Unlock()
	if _, exist := sc.topicToConsumerChannelMap[id]; !exist {
		sc.topicToConsumerChannelMap[id] = arg
	}
}

func (sc *SaramaClient) deleteFromTopicToConsumerChannelMap(id string) {
	sc.lockTopicToConsumerChannelMap.Lock()
	defer sc.lockTopicToConsumerChannelMap.Unlock()
	if _, exist := sc.topicToConsumerChannelMap[id]; exist {
		delete(sc.topicToConsumerChannelMap, id)
	}
}

func (sc *SaramaClient) getConsumerChannel(topic *Topic) *consumerChannels {
	sc.lockTopicToConsumerChannelMap.Lock()
	defer sc.lockTopicToConsumerChannelMap.Unlock()

	if consumerCh, exist := sc.topicToConsumerChannelMap[topic.Name]; exist {
		return consumerCh
	}
	return nil
}

func (sc *SaramaClient) addChannelToConsumerChannelMap(topic *Topic, ch chan *ca.InterContainerMessage) {
	sc.lockTopicToConsumerChannelMap.Lock()
	defer sc.lockTopicToConsumerChannelMap.Unlock()
	if consumerCh, exist := sc.topicToConsumerChannelMap[topic.Name]; exist {
		consumerCh.channels = append(consumerCh.channels, ch)
		return
	}
	log.Warnw("consumers-channel-not-exist", log.Fields{"topic": topic.Name})
}

//closeConsumers closes a list of sarama consumers.  The consumers can either be a partition consumers or a group consumers
func closeConsumers(consumers []interface{}) error {
	var err error
	for _, consumer := range consumers {
		//	Is it a partition consumers?
		if partionConsumer, ok := consumer.(sarama.PartitionConsumer); ok {
			if errTemp := partionConsumer.Close(); errTemp != nil {
				log.Debugw("partition!!!", log.Fields{"err": errTemp})
				if strings.Compare(errTemp.Error(), sarama.ErrUnknownTopicOrPartition.Error()) == 0 {
					// This can occur on race condition
					err = nil
				} else {
					err = errTemp
				}
			}
		} else if groupConsumer, ok := consumer.(*scc.Consumer); ok {
			if errTemp := groupConsumer.Close(); errTemp != nil {
				if strings.Compare(errTemp.Error(), sarama.ErrUnknownTopicOrPartition.Error()) == 0 {
					// This can occur on race condition
					err = nil
				} else {
					err = errTemp
				}
			}
		}
	}
	return err
}

func (sc *SaramaClient) removeChannelFromConsumerChannelMap(topic Topic, ch <-chan *ca.InterContainerMessage) error {
	sc.lockTopicToConsumerChannelMap.Lock()
	defer sc.lockTopicToConsumerChannelMap.Unlock()
	if consumerCh, exist := sc.topicToConsumerChannelMap[topic.Name]; exist {
		// Channel will be closed in the removeChannel method
		consumerCh.channels = removeChannel(consumerCh.channels, ch)
		// If there are no more channels then we can close the consumers itself
		if len(consumerCh.channels) == 0 {
			log.Debugw("closing-consumers", log.Fields{"topic": topic})
			err := closeConsumers(consumerCh.consumers)
			//err := consumerCh.consumers.Close()
			delete(sc.topicToConsumerChannelMap, topic.Name)
			return err
		}
		return nil
	}
	log.Warnw("topic-does-not-exist", log.Fields{"topic": topic.Name})
	return errors.New("topic-does-not-exist")
}

func (sc *SaramaClient) clearTopicFromConsumerChannelMap(topic Topic) error {
	sc.lockTopicToConsumerChannelMap.Lock()
	defer sc.lockTopicToConsumerChannelMap.Unlock()
	if consumerCh, exist := sc.topicToConsumerChannelMap[topic.Name]; exist {
		for _, ch := range consumerCh.channels {
			// Channel will be closed in the removeChannel method
			removeChannel(consumerCh.channels, ch)
		}
		err := closeConsumers(consumerCh.consumers)
		//if err == sarama.ErrUnknownTopicOrPartition {
		//	// Not an error
		//	err = nil
		//}
		//err := consumerCh.consumers.Close()
		delete(sc.topicToConsumerChannelMap, topic.Name)
		return err
	}
	log.Debugw("topic-does-not-exist", log.Fields{"topic": topic.Name})
	return nil
}

func (sc *SaramaClient) clearConsumerChannelMap() error {
	sc.lockTopicToConsumerChannelMap.Lock()
	defer sc.lockTopicToConsumerChannelMap.Unlock()
	var err error
	for topic, consumerCh := range sc.topicToConsumerChannelMap {
		for _, ch := range consumerCh.channels {
			// Channel will be closed in the removeChannel method
			removeChannel(consumerCh.channels, ch)
		}
		if errTemp := closeConsumers(consumerCh.consumers); errTemp != nil {
			err = errTemp
		}
		//err = consumerCh.consumers.Close()
		delete(sc.topicToConsumerChannelMap, topic)
	}
	return err
}

//createPublisher creates the publisher which is used to send a message onto kafka
func (sc *SaramaClient) createPublisher() error {
	// This Creates the publisher
	config := sarama.NewConfig()
	config.Producer.Partitioner = sarama.NewRandomPartitioner
	config.Producer.Flush.Frequency = time.Duration(sc.producerFlushFrequency)
	config.Producer.Flush.Messages = sc.producerFlushMessages
	config.Producer.Flush.MaxMessages = sc.producerFlushMaxmessages
	config.Producer.Return.Errors = sc.producerReturnErrors
	config.Producer.Return.Successes = sc.producerReturnSuccess
	//config.Producer.RequiredAcks = sarama.WaitForAll
	config.Producer.RequiredAcks = sarama.WaitForLocal

	kafkaFullAddr := fmt.Sprintf("%s:%d", sc.KafkaHost, sc.KafkaPort)
	brokers := []string{kafkaFullAddr}

	if producer, err := sarama.NewAsyncProducer(brokers, config); err != nil {
		log.Errorw("error-starting-publisher", log.Fields{"error": err})
		return err
	} else {
		sc.producer = producer
	}
	log.Info("Kafka-publisher-created")
	return nil
}

func (sc *SaramaClient) createConsumer() error {
	config := sarama.NewConfig()
	config.Consumer.Return.Errors = true
	config.Consumer.Fetch.Min = 1
	config.Consumer.MaxWaitTime = time.Duration(sc.consumerMaxwait) * time.Millisecond
	config.Consumer.MaxProcessingTime = time.Duration(sc.maxProcessingTime) * time.Millisecond
	config.Consumer.Offsets.Initial = sarama.OffsetNewest
	kafkaFullAddr := fmt.Sprintf("%s:%d", sc.KafkaHost, sc.KafkaPort)
	brokers := []string{kafkaFullAddr}

	if consumer, err := sarama.NewConsumer(brokers, config); err != nil {
		log.Errorw("error-starting-consumers", log.Fields{"error": err})
		return err
	} else {
		sc.consumer = consumer
	}
	log.Info("Kafka-consumers-created")
	return nil
}

// createGroupConsumer creates a consumers group
func (sc *SaramaClient) createGroupConsumer(topic *Topic, groupId *string, retries int) (*scc.Consumer, error) {
	config := scc.NewConfig()
	config.ClientID = uuid.New().String()
	config.Group.Mode = scc.ConsumerModeMultiplex
	//config.Consumer.Return.Errors = true
	//config.Group.Return.Notifications = false
	//config.Consumer.MaxWaitTime = time.Duration(DefaultConsumerMaxwait) * time.Millisecond
	//config.Consumer.MaxProcessingTime = time.Duration(DefaultMaxProcessingTime) * time.Millisecond
	config.Consumer.Offsets.Initial = sarama.OffsetNewest
	kafkaFullAddr := fmt.Sprintf("%s:%d", sc.KafkaHost, sc.KafkaPort)
	brokers := []string{kafkaFullAddr}

	if groupId == nil {
		g := DefaultGroupName
		groupId = &g
	}
	topics := []string{topic.Name}
	var consumer *scc.Consumer
	var err error

	if consumer, err = scc.NewConsumer(brokers, *groupId, topics, config); err != nil {
		log.Errorw("create-consumers-failure", log.Fields{"error": err, "topic": topic.Name, "groupId": groupId})
		return nil, err
	}
	log.Debugw("create-consumers-success", log.Fields{"topic": topic.Name, "groupId": groupId})
	//time.Sleep(10*time.Second)
	sc.groupConsumer = consumer
	return consumer, nil
}

// dispatchToConsumers sends the intercontainermessage received on a given topic to all subscribers for that
// topic via the unique channel each subsciber received during subscription
func (sc *SaramaClient) dispatchToConsumers(consumerCh *consumerChannels, protoMessage *ca.InterContainerMessage) {
	// Need to go over all channels and publish messages to them - do we need to copy msg?
	sc.lockTopicToConsumerChannelMap.Lock()
	defer sc.lockTopicToConsumerChannelMap.Unlock()
	for _, ch := range consumerCh.channels {
		go func(c chan *ca.InterContainerMessage) {
			c <- protoMessage
		}(ch)
	}
}

func (sc *SaramaClient) consumeFromAPartition(topic *Topic, consumer sarama.PartitionConsumer, consumerChnls *consumerChannels) {
	log.Debugw("starting-partition-consumption-loop", log.Fields{"topic": topic.Name})
startloop:
	for {
		select {
		case err := <-consumer.Errors():
			if err != nil {
				log.Warnw("partition-consumers-error", log.Fields{"error": err})
			} else {
				// There is a race condition when this loop is stopped and the consumer is closed where
				// the actual error comes as nil
				log.Warn("partition-consumers-error")
			}
		case msg := <-consumer.Messages():
			//log.Debugw("message-received", log.Fields{"msg": msg, "receivedTopic": msg.Topic})
			if msg == nil {
				// There is a race condition when this loop is stopped and the consumer is closed where
				// the actual msg comes as nil
				break startloop
			}
			msgBody := msg.Value
			icm := &ca.InterContainerMessage{}
			if err := proto.Unmarshal(msgBody, icm); err != nil {
				log.Warnw("partition-invalid-message", log.Fields{"error": err})
				continue
			}
			go sc.dispatchToConsumers(consumerChnls, icm)
		case <-sc.doneCh:
			log.Infow("partition-received-exit-signal", log.Fields{"topic": topic.Name})
			break startloop
		}
	}
	log.Infow("partition-consumer-stopped", log.Fields{"topic": topic.Name})
}

func (sc *SaramaClient) consumeGroupMessages(topic *Topic, consumer *scc.Consumer, consumerChnls *consumerChannels) {
	log.Debugw("starting-group-consumption-loop", log.Fields{"topic": topic.Name})

startloop:
	for {
		select {
		case err := <-consumer.Errors():
			if err != nil {
				log.Warnw("group-consumers-error", log.Fields{"error": err})
			} else {
				// There is a race condition when this loop is stopped and the consumer is closed where
				// the actual error comes as nil
				log.Warn("group-consumers-error")
			}
		case msg := <-consumer.Messages():
			//log.Debugw("message-received", log.Fields{"msg": msg, "receivedTopic": msg.Topic})
			if msg == nil {
				// There is a race condition when this loop is stopped and the consumer is closed where
				// the actual msg comes as nil
				break startloop
			}
			msgBody := msg.Value
			icm := &ca.InterContainerMessage{}
			if err := proto.Unmarshal(msgBody, icm); err != nil {
				log.Warnw("invalid-message", log.Fields{"error": err})
				continue
			}
			go sc.dispatchToConsumers(consumerChnls, icm)
			consumer.MarkOffset(msg, "")
		case ntf := <-consumer.Notifications():
			log.Debugw("group-received-notification", log.Fields{"notification": ntf})
		case <-sc.doneCh:
			log.Infow("group-received-exit-signal", log.Fields{"topic": topic.Name})
			break startloop
		}
	}
	log.Infow("group-consumer-stopped", log.Fields{"topic": topic.Name})
}

func (sc *SaramaClient) startConsumers(topic *Topic) error {
	log.Debugw("starting-consumers", log.Fields{"topic": topic.Name})
	var consumerCh *consumerChannels
	if consumerCh = sc.getConsumerChannel(topic); consumerCh == nil {
		log.Errorw("consumers-not-exist", log.Fields{"topic": topic.Name})
		return errors.New("consumers-not-exist")
	}
	// For each consumer listening for that topic, start a consumption loop
	for _, consumer := range consumerCh.consumers {
		if pConsumer, ok := consumer.(sarama.PartitionConsumer); ok {
			go sc.consumeFromAPartition(topic, pConsumer, consumerCh)
		} else if gConsumer, ok := consumer.(*scc.Consumer); ok {
			go sc.consumeGroupMessages(topic, gConsumer, consumerCh)
		} else {
			log.Errorw("invalid-consumer", log.Fields{"topic": topic})
			return errors.New("invalid-consumer")
		}
	}
	return nil
}

//// setupConsumerChannel creates a consumerChannels object for that topic and add it to the consumerChannels map
//// for that topic.  It also starts the routine that listens for messages on that topic.
func (sc *SaramaClient) setupPartitionConsumerChannel(topic *Topic, initialOffset int64) (chan *ca.InterContainerMessage, error) {
	var pConsumers []sarama.PartitionConsumer
	var err error

	if pConsumers, err = sc.createPartionConsumers(topic, initialOffset); err != nil {
		log.Errorw("creating-partition-consumers-failure", log.Fields{"error": err, "topic": topic.Name})
		return nil, err
	}

	consumersIf := make([]interface{}, 0)
	for _, pConsumer := range pConsumers {
		consumersIf = append(consumersIf, pConsumer)
	}

	// Create the consumers/channel structure and set the consumers and create a channel on that topic - for now
	// unbuffered to verify race conditions.
	consumerListeningChannel := make(chan *ca.InterContainerMessage)
	cc := &consumerChannels{
		consumers: consumersIf,
		channels:  []chan *ca.InterContainerMessage{consumerListeningChannel},
	}

	// Add the consumers channel to the map
	sc.addTopicToConsumerChannelMap(topic.Name, cc)

	//Start a consumers to listen on that specific topic
	go sc.startConsumers(topic)

	return consumerListeningChannel, nil
}

// setupConsumerChannel creates a consumerChannels object for that topic and add it to the consumerChannels map
// for that topic.  It also starts the routine that listens for messages on that topic.
func (sc *SaramaClient) setupGroupConsumerChannel(topic *Topic, groupId string) (chan *ca.InterContainerMessage, error) {
	// TODO:  Replace this development partition consumers with a group consumers
	var pConsumer *scc.Consumer
	var err error
	if pConsumer, err = sc.createGroupConsumer(topic, &groupId, DefaultMaxRetries); err != nil {
		log.Errorw("creating-partition-consumers-failure", log.Fields{"error": err, "topic": topic.Name})
		return nil, err
	}
	// Create the consumers/channel structure and set the consumers and create a channel on that topic - for now
	// unbuffered to verify race conditions.
	consumerListeningChannel := make(chan *ca.InterContainerMessage)
	cc := &consumerChannels{
		consumers: []interface{}{pConsumer},
		channels:  []chan *ca.InterContainerMessage{consumerListeningChannel},
	}

	// Add the consumers channel to the map
	sc.addTopicToConsumerChannelMap(topic.Name, cc)

	//Start a consumers to listen on that specific topic
	go sc.startConsumers(topic)

	return consumerListeningChannel, nil
}

func (sc *SaramaClient) createPartionConsumers(topic *Topic, initialOffset int64) ([]sarama.PartitionConsumer, error) {
	log.Debugw("creating-partition-consumers", log.Fields{"topic": topic.Name})
	partitionList, err := sc.consumer.Partitions(topic.Name)
	if err != nil {
		log.Warnw("get-partition-failure", log.Fields{"error": err, "topic": topic.Name})
		return nil, err
	}

	pConsumers := make([]sarama.PartitionConsumer, 0)
	for _, partition := range partitionList {
		var pConsumer sarama.PartitionConsumer
		if pConsumer, err = sc.consumer.ConsumePartition(topic.Name, partition, initialOffset); err != nil {
			log.Warnw("consumers-partition-failure", log.Fields{"error": err, "topic": topic.Name})
			return nil, err
		}
		pConsumers = append(pConsumers, pConsumer)
	}
	return pConsumers, nil
}

func removeChannel(channels []chan *ca.InterContainerMessage, ch <-chan *ca.InterContainerMessage) []chan *ca.InterContainerMessage {
	var i int
	var channel chan *ca.InterContainerMessage
	for i, channel = range channels {
		if channel == ch {
			channels[len(channels)-1], channels[i] = channels[i], channels[len(channels)-1]
			close(channel)
			return channels[:len(channels)-1]
		}
	}
	return channels
}
