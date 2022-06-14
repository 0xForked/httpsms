package events

import (
	"time"

	"github.com/google/uuid"
)

// EventTypeMessagePhoneReceived is emitted when a new message is received by a mobile phone
const EventTypeMessagePhoneReceived = "message.phone.received"

// MessagePhoneReceivedPayload is the payload of the EventTypeMessagePhoneReceived event
type MessagePhoneReceivedPayload struct {
	ID        uuid.UUID `json:"id"`
	Owner     string    `json:"owner"`
	Contact   string    `json:"contact"`
	Timestamp time.Time `json:"timestamp"`
	Content   string    `json:"content"`
}
