package stream

import (
	"context"
	"fmt"

	"github.com/hashicorp/go-multierror"
	"github.com/justtrackio/gosoline/pkg/cfg"
	"github.com/justtrackio/gosoline/pkg/cloud/aws/sqs"
	"github.com/justtrackio/gosoline/pkg/log"
	"github.com/justtrackio/gosoline/pkg/mdl"
	"github.com/spf13/cast"
	"github.com/thoas/go-funk"
)

const sqsOutputBatchSize = 10

type SqsOutputSettings struct {
	cfg.AppId
	QueueId           string
	VisibilityTimeout int
	Fifo              sqs.FifoSettings
	RedrivePolicy     sqs.RedrivePolicy
	ClientName        string
}

func (s SqsOutputSettings) GetAppid() cfg.AppId {
	return s.AppId
}

func (s SqsOutputSettings) GetQueueId() string {
	return s.QueueId
}

func (s SqsOutputSettings) IsFifoEnabled() bool {
	return s.Fifo.Enabled
}

type sqsOutput struct {
	logger   log.Logger
	queue    sqs.Queue
	settings *SqsOutputSettings
}

func NewSqsOutput(ctx context.Context, config cfg.Config, logger log.Logger, settings *SqsOutputSettings) (Output, error) {
	settings.PadFromConfig(config)

	queueName := sqs.GetQueueName(settings)
	queueSettings := &sqs.Settings{
		QueueName:         queueName,
		VisibilityTimeout: settings.VisibilityTimeout,
		Fifo:              settings.Fifo,
		RedrivePolicy:     settings.RedrivePolicy,
		ClientName:        settings.ClientName,
	}

	var err error
	var queue sqs.Queue

	if queue, err = sqs.ProvideQueue(ctx, config, logger, queueSettings); err != nil {
		return nil, fmt.Errorf("can not create queue: %w", err)
	}

	return NewSqsOutputWithInterfaces(logger, queue, settings), nil
}

func NewSqsOutputWithInterfaces(logger log.Logger, queue sqs.Queue, s *SqsOutputSettings) Output {
	return &sqsOutput{
		logger:   logger,
		queue:    queue,
		settings: s,
	}
}

func (o *sqsOutput) WriteOne(ctx context.Context, record WritableMessage) error {
	return o.Write(ctx, []WritableMessage{record})
}

func (o *sqsOutput) Write(ctx context.Context, batch []WritableMessage) error {
	chunks, ok := funk.Chunk(batch, sqsOutputBatchSize).([][]WritableMessage)

	if !ok {
		err := fmt.Errorf("can not chunk messages for sending to sqs")
		o.logger.WithContext(ctx).Error("can not chunk messages for sending to sqs: %w", err)

		return err
	}

	var result error

	for _, chunk := range chunks {
		messages, err := o.buildSqsMessages(ctx, chunk)
		if err != nil {
			result = multierror.Append(result, err)
		}

		if len(messages) == 0 {
			continue
		}

		err = o.queue.SendBatch(ctx, messages)

		if err != nil {
			result = multierror.Append(result, err)
		}
	}

	if result != nil {
		return fmt.Errorf("there were errors on writing to the sqs stream: %w", result)
	}

	return nil
}

func (o *sqsOutput) GetMaxMessageSize() *int {
	return mdl.Int(256 * 1024)
}

func (o *sqsOutput) GetMaxBatchSize() *int {
	return mdl.Int(10)
}

func (o *sqsOutput) buildSqsMessages(ctx context.Context, messages []WritableMessage) ([]*sqs.Message, error) {
	var result error
	sqsMessages := make([]*sqs.Message, 0)

	for _, msg := range messages {
		sqsMessage, err := o.buildSqsMessage(ctx, msg)
		if err != nil {
			result = multierror.Append(result, err)
			continue
		}

		sqsMessages = append(sqsMessages, sqsMessage)
	}

	return sqsMessages, result
}

func (o *sqsOutput) buildSqsMessage(ctx context.Context, msg WritableMessage) (*sqs.Message, error) {
	var err error
	var delay int32
	var messageGroupId string
	var messageDeduplicationId string

	attributes := getAttributes(msg)

	if d, ok := attributes[sqs.AttributeSqsDelaySeconds]; ok {
		if delay, err = cast.ToInt32E(d); err != nil {
			return nil, fmt.Errorf("the type of the %s attribute with value %v should be castable to int32: %w", sqs.AttributeSqsDelaySeconds, attributes[sqs.AttributeSqsDelaySeconds], err)
		}
	}

	if d, ok := attributes[sqs.AttributeSqsMessageGroupId]; ok {
		if messageGroupId, err = cast.ToStringE(d); err != nil {
			return nil, fmt.Errorf("the type of the %s attribute with value %v should be castable to string: %w", sqs.AttributeSqsMessageGroupId, attributes[sqs.AttributeSqsMessageGroupId], err)
		}
	}

	if d, ok := attributes[sqs.AttributeSqsMessageDeduplicationId]; ok {
		if messageDeduplicationId, err = cast.ToStringE(d); err != nil {
			return nil, fmt.Errorf("the type of the %s attribute with value %v should be castable to string: %w", sqs.AttributeSqsMessageDeduplicationId, attributes[sqs.AttributeSqsMessageDeduplicationId], err)
		}
	}

	if o.settings.Fifo.ContentBasedDeduplication && messageDeduplicationId == "" {
		o.logger.WithContext(ctx).WithFields(log.Fields{
			"stacktrace": log.GetStackTrace(0),
		}).Warn("writing message to queue %s (which is configured to use content based deduplication) without message deduplication id", o.queue.GetName())
	}

	body, err := msg.MarshalToString()
	if err != nil {
		return nil, err
	}

	sqsMessage := &sqs.Message{
		DelaySeconds: delay,
		Body:         mdl.String(body),
	}

	if messageGroupId != "" {
		sqsMessage.MessageGroupId = mdl.String(messageGroupId)
	}

	if messageDeduplicationId != "" {
		sqsMessage.MessageDeduplicationId = mdl.String(messageDeduplicationId)
	}

	return sqsMessage, nil
}
