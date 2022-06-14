package services

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/NdoleStudio/http-sms-manager/pkg/events"
	"github.com/NdoleStudio/http-sms-manager/pkg/repositories"
	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/google/uuid"
	"github.com/palantir/stacktrace"

	"github.com/NdoleStudio/http-sms-manager/pkg/entities"
	"github.com/NdoleStudio/http-sms-manager/pkg/telemetry"
)

// MessageService is handles message requests
type MessageService struct {
	logger          telemetry.Logger
	tracer          telemetry.Tracer
	eventDispatcher *EventDispatcher
	repository      repositories.MessageRepository
}

// NewMessageService creates a new MessageService
func NewMessageService(
	logger telemetry.Logger,
	tracer telemetry.Tracer,
	repository repositories.MessageRepository,
	eventDispatcher *EventDispatcher,
) (s *MessageService) {
	return &MessageService{
		logger:          logger.WithService(fmt.Sprintf("%T", s)),
		tracer:          tracer,
		repository:      repository,
		eventDispatcher: eventDispatcher,
	}
}

// MessageGetOutstandingParams parameters for sending a new message
type MessageGetOutstandingParams struct {
	Source string
	Limit  int
}

// GetOutstanding fetches messages that still to be sent to the phone
func (service *MessageService) GetOutstanding(ctx context.Context, params MessageGetOutstandingParams) (*[]entities.Message, error) {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	ctxLogger := service.tracer.CtxLogger(service.logger, span)

	messages, err := service.repository.GetOutstanding(ctx, params.Limit)
	if err != nil {
		msg := fmt.Sprintf("could not fetch [%d] outstanding messages", params.Limit)
		return nil, service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("fetched [%d] outstanding messages", len(*messages)))
	return service.handleOutstandingMessages(ctx, params.Source, messages), nil
}

// MessageGetParams parameters for sending a new message
type MessageGetParams struct {
	repositories.IndexParams
	Owner   string
	Contact string
}

// GetMessages fetches sent between 2 phone numbers
func (service *MessageService) GetMessages(ctx context.Context, params MessageGetParams) (*[]entities.Message, error) {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	ctxLogger := service.tracer.CtxLogger(service.logger, span)

	messages, err := service.repository.Index(ctx, params.Owner, params.Contact, params.IndexParams)
	if err != nil {
		msg := fmt.Sprintf("could not fetch messages with parms [%+#v]", params)
		return nil, service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("fetched [%d] messages with prams [%+#v]", len(*messages), params))
	return messages, nil
}

// GetMessage fetches a message by the ID
func (service *MessageService) GetMessage(ctx context.Context, messageID uuid.UUID) (*entities.Message, error) {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	message, err := service.repository.Load(ctx, messageID)
	if err != nil {
		msg := fmt.Sprintf("could not fetch messages with ID [%s]", messageID)
		return nil, service.tracer.WrapErrorSpan(span, stacktrace.PropagateWithCode(err, stacktrace.GetCode(err), msg))
	}

	return message, nil
}

// MessageStorePhoneEventParams parameters registering a message event
type MessageStorePhoneEventParams struct {
	MessageID uuid.UUID
	EventName entities.MessageEventName
	Timestamp time.Time
	Source    string
}

// StoreEvent handles event generated by a mobile phone
func (service *MessageService) StoreEvent(ctx context.Context, message *entities.Message, params MessageStorePhoneEventParams) (*entities.Message, error) {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	var err error

	switch params.EventName {
	case entities.MessageEventNameSent:
		err = service.handleMessageSentEvent(ctx, params, message)
	default:
		return nil, service.tracer.WrapErrorSpan(span, stacktrace.NewError(fmt.Sprintf("cannot handle message event [%s]", params.EventName)))
	}

	if err != nil {
		msg := fmt.Sprintf("could not handle phone event [%s] for message with id [%s]", params.EventName, message.ID)
		return nil, service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	return service.repository.Load(ctx, params.MessageID)
}

// MessageReceiveParams parameters registering a message event
type MessageReceiveParams struct {
	Contact   string
	Owner     string
	Content   string
	Timestamp time.Time
	Source    string
}

// ReceiveMessage handles message received by a mobile phone
func (service *MessageService) ReceiveMessage(ctx context.Context, params MessageReceiveParams) (*entities.Message, error) {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	ctxLogger := service.tracer.CtxLogger(service.logger, span)

	eventPayload := events.MessagePhoneReceivedPayload{
		ID:        uuid.New(),
		Owner:     params.Owner,
		Contact:   params.Contact,
		Timestamp: params.Timestamp,
		Content:   params.Content,
	}

	ctxLogger.Info(fmt.Sprintf("creating cloud event for received with ID [%s]", eventPayload.ID))

	event, err := service.createMessagePhoneReceivedEvent(params.Source, eventPayload)
	if err != nil {
		msg := fmt.Sprintf("cannot create %T from payload with message id [%s]", event, eventPayload.ID)
		return nil, service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("created event [%s] with id [%s] and message id [%s]", event.Type(), event.ID(), eventPayload.ID))

	if err = service.eventDispatcher.Dispatch(ctx, event); err != nil {
		msg := fmt.Sprintf("cannot dispatch event type [%s] and id [%s]", event.Type(), event.ID())
		return nil, service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("event [%s] dispatched succesfully", event.ID()))

	message, err := service.repository.Load(ctx, eventPayload.ID)
	if err != nil {
		msg := fmt.Sprintf("cannot load message with ID [%s] in the repository", eventPayload.ID)
		return nil, service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("fetched message with id [%s] from the repository", message.ID))

	return message, nil
}

func (service *MessageService) handleMessageSentEvent(ctx context.Context, params MessageStorePhoneEventParams, message *entities.Message) error {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	event, err := service.createMessagePhoneSentEvent(params.Source, events.MessagePhoneSentPayload{
		ID:        message.ID,
		Owner:     message.Owner,
		Timestamp: params.Timestamp,
		Contact:   message.Contact,
		Content:   message.Content,
	})
	if err != nil {
		msg := fmt.Sprintf("cannot create event [%s] for message [%s]", events.EventTypeMessagePhoneSent, message.ID)
		return service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	if err = service.eventDispatcher.Dispatch(ctx, event); err != nil {
		msg := fmt.Sprintf("cannot dispatch event type [%s] and id [%s]", event.Type(), event.ID())
		return service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}
	return nil
}

func (service *MessageService) handleOutstandingMessages(ctx context.Context, source string, messages *[]entities.Message) *[]entities.Message {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	ctxLogger := service.tracer.CtxLogger(service.logger, span)

	var wg sync.WaitGroup
	results := make([]entities.Message, 0, len(*messages))
	var lock sync.Mutex

	for _, message := range *messages {
		wg.Add(1)
		go func(ctx context.Context, message entities.Message) {
			defer wg.Done()

			event, err := service.createMessagePhoneSendingEvent(source, events.MessagePhoneSendingPayload{
				ID:      message.ID,
				Owner:   message.Owner,
				Contact: message.Contact,
				Content: message.Content,
			})
			if err != nil {
				msg := fmt.Sprintf("cannot create [%T] for message with ID [%s]", event, message.ID)
				ctxLogger.Error(service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg)))
				return
			}

			ctxLogger.Info(fmt.Sprintf("created event [%s] with id [%s] for message [%s]", event.Type(), event.ID(), message.ID))

			if err = service.eventDispatcher.Dispatch(ctx, event); err != nil {
				msg := fmt.Sprintf("cannot dispatch event [%s] with id [%s] for message [%s]", event.Type(), event.ID(), message.ID)
				ctxLogger.Error(service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg)))
				return
			}

			ctxLogger.Info(fmt.Sprintf("dispatched event [%s] with id [%s] for message [%s]", event.Type(), event.ID(), message.ID))

			resultMessage, err := service.repository.Load(ctx, message.ID)
			if err != nil {
				msg := fmt.Sprintf("cannot load message with id [%s]", message.ID)
				ctxLogger.Error(service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg)))
				return
			}

			ctxLogger.Info(fmt.Sprintf("loaded message [%s]", message.ID))

			lock.Lock()
			defer lock.Unlock()
			results = append(results, *resultMessage)
		}(ctx, message)
	}

	wg.Wait()
	return &results
}

// MessageSendParams parameters for sending a new message
type MessageSendParams struct {
	Owner             string
	Contact           string
	Content           string
	Source            string
	RequestReceivedAt time.Time
}

// SendMessage a new message
func (service *MessageService) SendMessage(ctx context.Context, params MessageSendParams) (*entities.Message, error) {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	ctxLogger := service.tracer.CtxLogger(service.logger, span)

	eventPayload := events.MessageAPISentPayload{
		ID:                uuid.New(),
		Owner:             params.Owner,
		Contact:           params.Contact,
		RequestReceivedAt: params.RequestReceivedAt,
		Content:           params.Content,
	}

	ctxLogger.Info(fmt.Sprintf("creating cloud event for message with ID [%s]", eventPayload.ID))

	event, err := service.createMessageAPISentEvent(params.Source, eventPayload)
	if err != nil {
		msg := fmt.Sprintf("cannot create %T from payload with message id [%s]", event, eventPayload.ID)
		return nil, service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("created event [%s] with id [%s] and message id [%s]", event.Type(), event.ID(), eventPayload.ID))

	if err = service.eventDispatcher.Dispatch(ctx, event); err != nil {
		msg := fmt.Sprintf("cannot dispatch event type [%s] and id [%s]", event.Type(), event.ID())
		return nil, service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("event [%s] dispatched succesfully", event.ID()))

	message, err := service.repository.Load(ctx, eventPayload.ID)
	if err != nil {
		msg := fmt.Sprintf("cannot load message with ID [%s] in the repository", eventPayload.ID)
		return nil, service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("fetched message with id [%s] from the repository", message.ID))

	return message, nil
}

// MessageStoreParams are parameters for creating a new message
type MessageStoreParams struct {
	Owner     string
	Contact   string
	Content   string
	ID        uuid.UUID
	Timestamp time.Time
}

// StoreSentMessage a new message
func (service *MessageService) StoreSentMessage(ctx context.Context, params MessageStoreParams) (*entities.Message, error) {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	ctxLogger := service.tracer.CtxLogger(service.logger, span)

	message := &entities.Message{
		ID:                params.ID,
		Owner:             params.Owner,
		Contact:           params.Contact,
		Content:           params.Content,
		Type:              entities.MessageTypeMobileTerminated,
		Status:            entities.MessageStatusPending,
		RequestReceivedAt: params.Timestamp,
		CreatedAt:         time.Now().UTC(),
		UpdatedAt:         time.Now().UTC(),
		OrderTimestamp:    params.Timestamp,
		SendDuration:      nil,
		LastAttemptedAt:   nil,
		SentAt:            nil,
		ReceivedAt:        nil,
	}

	if err := service.repository.Store(ctx, message); err != nil {
		msg := fmt.Sprintf("cannot save message with id [%s]", params.ID)
		return nil, service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("message saved with id [%s] in the repository", message.ID))
	return message, nil
}

// StoreReceivedMessage a new message
func (service *MessageService) StoreReceivedMessage(ctx context.Context, params MessageStoreParams) (*entities.Message, error) {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	ctxLogger := service.tracer.CtxLogger(service.logger, span)

	message := &entities.Message{
		ID:                params.ID,
		Owner:             params.Owner,
		Contact:           params.Contact,
		Content:           params.Content,
		Type:              entities.MessageTypeMobileOriginated,
		Status:            entities.MessageStatusReceived,
		RequestReceivedAt: params.Timestamp,
		CreatedAt:         time.Now().UTC(),
		UpdatedAt:         time.Now().UTC(),
		OrderTimestamp:    params.Timestamp,
		ReceivedAt:        &params.Timestamp,
	}

	if err := service.repository.Store(ctx, message); err != nil {
		msg := fmt.Sprintf("cannot save message with id [%s]", params.ID)
		return nil, service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("message saved with id [%s] in the repository", message.ID))
	return message, nil
}

// HandleMessageParams are parameters for handling a message event
type HandleMessageParams struct {
	ID        uuid.UUID
	Timestamp time.Time
}

// HandleMessageSending handles when a message is being sent
func (service *MessageService) HandleMessageSending(ctx context.Context, params HandleMessageParams) error {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	ctxLogger := service.tracer.CtxLogger(service.logger, span)

	message, err := service.repository.Load(ctx, params.ID)
	if err != nil {
		msg := fmt.Sprintf("cannot find message with id [%s]", params.ID)
		return service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	if !message.IsSending() {
		msg := fmt.Sprintf("message has wrong status [%s]. expected %s", message.Status, entities.MessageStatusSending)
		return service.tracer.WrapErrorSpan(span, stacktrace.NewError(msg))
	}

	if err = service.repository.Update(ctx, message.AddSendAttempt(params.Timestamp)); err != nil {
		msg := fmt.Sprintf("cannot update message with id [%s] after sending", message.ID)
		return service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("message with id [%s] in the repository after adding send attempt", message.ID))
	return nil
}

// HandleMessageSent handles when a message is has been sent by a mobile phone
func (service *MessageService) HandleMessageSent(ctx context.Context, params HandleMessageParams) error {
	ctx, span := service.tracer.Start(ctx)
	defer span.End()

	ctxLogger := service.tracer.CtxLogger(service.logger, span)

	message, err := service.repository.Load(ctx, params.ID)
	if err != nil {
		msg := fmt.Sprintf("cannot find message with id [%s]", params.ID)
		return service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	if !message.IsSending() {
		msg := fmt.Sprintf("message has wrong status [%s]. expected %s", message.Status, entities.MessageStatusSending)
		return service.tracer.WrapErrorSpan(span, stacktrace.NewError(msg))
	}

	if err = service.repository.Update(ctx, message.Sent(params.Timestamp)); err != nil {
		msg := fmt.Sprintf("cannot update message with id [%s] as sent", message.ID)
		return service.tracer.WrapErrorSpan(span, stacktrace.Propagate(err, msg))
	}

	ctxLogger.Info(fmt.Sprintf("message with id [%s] has been updated to status [%s]", message.ID, message.Status))
	return nil
}

func (service *MessageService) createMessageAPISentEvent(source string, payload events.MessageAPISentPayload) (cloudevents.Event, error) {
	return service.createEvent(events.EventTypeMessageAPISent, source, payload)
}

func (service *MessageService) createMessagePhoneReceivedEvent(source string, payload events.MessagePhoneReceivedPayload) (cloudevents.Event, error) {
	return service.createEvent(events.EventTypeMessagePhoneReceived, source, payload)
}

func (service *MessageService) createMessagePhoneSendingEvent(source string, payload events.MessagePhoneSendingPayload) (cloudevents.Event, error) {
	return service.createEvent(events.EventTypeMessagePhoneSending, source, payload)
}

func (service *MessageService) createMessagePhoneSentEvent(source string, payload events.MessagePhoneSentPayload) (cloudevents.Event, error) {
	return service.createEvent(events.EventTypeMessagePhoneSent, source, payload)
}

func (service *MessageService) createEvent(eventType string, source string, payload any) (cloudevents.Event, error) {
	event := cloudevents.NewEvent()

	event.SetSource(source)
	event.SetType(eventType)
	event.SetTime(time.Now().UTC())
	event.SetID(uuid.New().String())

	if err := event.SetData(cloudevents.ApplicationJSON, payload); err != nil {
		msg := fmt.Sprintf("cannot encode %T [%#+v] as JSON", payload, payload)
		return event, stacktrace.Propagate(err, msg)
	}

	return event, nil
}
