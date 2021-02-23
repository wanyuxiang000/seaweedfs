package msgclient

import (
	"context"

	"github.com/OneOfOne/xxhash"
	"google.golang.org/grpc"

	"github.com/chrislusf/seaweedfs/weed/messaging/broker"
	"github.com/chrislusf/seaweedfs/weed/pb/messaging_pb"
)

type Publisher struct {
	publishClients     []messaging_pb.SeaweedMessaging_PublishClient
	topicConfiguration *messaging_pb.TopicConfiguration
	messageCount       uint64
	publisherId        string
}

func (mc *MessagingClient) NewPublisher(publisherId, namespace, topic string) (*Publisher, error) {
	// read topic configuration
	topicConfiguration := &messaging_pb.TopicConfiguration{
		PartitionCount: 4,
	}
	publishClients := make([]messaging_pb.SeaweedMessaging_PublishClient, topicConfiguration.PartitionCount)
	for i := 0; i < int(topicConfiguration.PartitionCount); i++ {
		tp := broker.TopicPartition{
			Namespace: namespace,
			Topic:     topic,
			Partition: int32(i),
		}
		grpcClientConn, err := mc.findBroker(tp)
		if err != nil {
			return nil, err
		}
		client, err := setupPublisherClient(grpcClientConn, tp)
		if err != nil {
			return nil, err
		}
		publishClients[i] = client
	}
	return &Publisher{
		publishClients:     publishClients,
		topicConfiguration: topicConfiguration,
	}, nil
}

func setupPublisherClient(grpcConnection *grpc.ClientConn, tp broker.TopicPartition) (messaging_pb.SeaweedMessaging_PublishClient, error) {

	stream, err := messaging_pb.NewSeaweedMessagingClient(grpcConnection).Publish(context.Background())
	if err != nil {
		return nil, err
	}

	// send init message
	err = stream.Send(&messaging_pb.PublishRequest{
		Init: &messaging_pb.PublishRequest_InitMessage{
			Namespace: tp.Namespace,
			Topic:     tp.Topic,
			Partition: tp.Partition,
		},
	})
	if err != nil {
		return nil, err
	}

	// process init response
	initResponse, err := stream.Recv()
	if err != nil {
		return nil, err
	}
	if initResponse.Redirect != nil {
		// TODO follow redirection
	}
	if initResponse.Config != nil {
	}

	// setup looks for control messages
	doneChan := make(chan error, 1)
	go func() {
		for {
			in, err := stream.Recv()
			if err != nil {
				doneChan <- err
				return
			}
			if in.Redirect != nil {
			}
			if in.Config != nil {
			}
		}
	}()

	return stream, nil

}

func (p *Publisher) Publish(m *messaging_pb.Message) error {
	hashValue := p.messageCount
	p.messageCount++
	if p.topicConfiguration.Partitoning == messaging_pb.TopicConfiguration_NonNullKeyHash {
		if m.Key != nil {
			hashValue = xxhash.Checksum64(m.Key)
		}
	} else if p.topicConfiguration.Partitoning == messaging_pb.TopicConfiguration_KeyHash {
		hashValue = xxhash.Checksum64(m.Key)
	} else {
		// round robin
	}

	idx := int(hashValue) % len(p.publishClients)
	if idx < 0 {
		idx += len(p.publishClients)
	}
	return p.publishClients[idx].Send(&messaging_pb.PublishRequest{
		Data: m,
	})
}
