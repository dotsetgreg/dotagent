package bus

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

type MessageBus struct {
	inbound         chan InboundMessage
	outbound        chan OutboundMessage
	events          chan EventMessage
	handlers        map[string]MessageHandler
	closed          bool
	dropped         droppedCounters
	inboundPublish  PublishConfig
	outboundPublish PublishConfig
	eventsPublish   PublishConfig
	mu              sync.RWMutex
}

type droppedCounters struct {
	inbound  atomic.Uint64
	outbound atomic.Uint64
	events   atomic.Uint64
}

type PublishConfig struct {
	Timeout     time.Duration
	MaxAttempts int
}

type MessageBusOptions struct {
	InboundBuffer   int
	OutboundBuffer  int
	EventBuffer     int
	InboundPublish  PublishConfig
	OutboundPublish PublishConfig
	EventsPublish   PublishConfig
}

const (
	defaultInboundBufferSize  = 100
	defaultOutboundBufferSize = 100
	defaultEventsBufferSize   = 128
)

var (
	ErrBusClosed      = errors.New("message bus is closed")
	ErrPublishDropped = errors.New("message bus publish dropped after timeout")
)

func NewMessageBus() *MessageBus {
	return NewMessageBusWithOptions(MessageBusOptions{})
}

func NewMessageBusWithOptions(opts MessageBusOptions) *MessageBus {
	inboundBuffer := opts.InboundBuffer
	if inboundBuffer <= 0 {
		inboundBuffer = defaultInboundBufferSize
	}
	outboundBuffer := opts.OutboundBuffer
	if outboundBuffer <= 0 {
		outboundBuffer = defaultOutboundBufferSize
	}
	eventsBuffer := opts.EventBuffer
	if eventsBuffer <= 0 {
		eventsBuffer = defaultEventsBufferSize
	}

	return &MessageBus{
		inbound:         make(chan InboundMessage, inboundBuffer),
		outbound:        make(chan OutboundMessage, outboundBuffer),
		events:          make(chan EventMessage, eventsBuffer),
		handlers:        make(map[string]MessageHandler),
		inboundPublish:  normalizePublishConfig(opts.InboundPublish),
		outboundPublish: normalizePublishConfig(opts.OutboundPublish),
		eventsPublish:   normalizePublishConfig(opts.EventsPublish),
	}
}

func normalizePublishConfig(cfg PublishConfig) PublishConfig {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 200 * time.Millisecond
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	return cfg
}

func (mb *MessageBus) PublishInbound(msg InboundMessage) error {
	mb.mu.RLock()
	defer mb.mu.RUnlock()
	if mb.closed {
		return ErrBusClosed
	}

	for attempt := 0; attempt < mb.inboundPublish.MaxAttempts; attempt++ {
		select {
		case mb.inbound <- msg:
			return nil
		default:
		}
		if attempt == mb.inboundPublish.MaxAttempts-1 {
			mb.dropped.inbound.Add(1)
			return ErrPublishDropped
		}
		timer := time.NewTimer(mb.inboundPublish.Timeout)
		select {
		case mb.inbound <- msg:
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	mb.dropped.inbound.Add(1)
	return ErrPublishDropped
}

func (mb *MessageBus) ConsumeInbound(ctx context.Context) (InboundMessage, bool) {
	select {
	case msg, ok := <-mb.inbound:
		if !ok {
			return InboundMessage{}, false
		}
		return msg, true
	case <-ctx.Done():
		return InboundMessage{}, false
	}
}

func (mb *MessageBus) PublishOutbound(msg OutboundMessage) error {
	mb.mu.RLock()
	defer mb.mu.RUnlock()
	if mb.closed {
		return ErrBusClosed
	}

	for attempt := 0; attempt < mb.outboundPublish.MaxAttempts; attempt++ {
		select {
		case mb.outbound <- msg:
			return nil
		default:
		}
		if attempt == mb.outboundPublish.MaxAttempts-1 {
			mb.dropped.outbound.Add(1)
			return ErrPublishDropped
		}
		timer := time.NewTimer(mb.outboundPublish.Timeout)
		select {
		case mb.outbound <- msg:
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	mb.dropped.outbound.Add(1)
	return ErrPublishDropped
}

func (mb *MessageBus) SubscribeOutbound(ctx context.Context) (OutboundMessage, bool) {
	select {
	case msg, ok := <-mb.outbound:
		if !ok {
			return OutboundMessage{}, false
		}
		return msg, true
	case <-ctx.Done():
		return OutboundMessage{}, false
	}
}

func (mb *MessageBus) RegisterHandler(channel string, handler MessageHandler) {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	mb.handlers[channel] = handler
}

func (mb *MessageBus) GetHandler(channel string) (MessageHandler, bool) {
	mb.mu.RLock()
	defer mb.mu.RUnlock()
	handler, ok := mb.handlers[channel]
	return handler, ok
}

func (mb *MessageBus) Close() {
	mb.mu.Lock()
	defer mb.mu.Unlock()
	if mb.closed {
		return
	}
	mb.closed = true
	close(mb.inbound)
	close(mb.outbound)
	close(mb.events)
}

func (mb *MessageBus) DroppedInbound() uint64 {
	return mb.dropped.inbound.Load()
}

func (mb *MessageBus) DroppedOutbound() uint64 {
	return mb.dropped.outbound.Load()
}

func (mb *MessageBus) PublishEvent(event EventMessage) error {
	mb.mu.RLock()
	defer mb.mu.RUnlock()
	if mb.closed {
		return ErrBusClosed
	}

	for attempt := 0; attempt < mb.eventsPublish.MaxAttempts; attempt++ {
		select {
		case mb.events <- event:
			return nil
		default:
		}
		if attempt == mb.eventsPublish.MaxAttempts-1 {
			mb.dropped.events.Add(1)
			return ErrPublishDropped
		}
		timer := time.NewTimer(mb.eventsPublish.Timeout)
		select {
		case mb.events <- event:
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	mb.dropped.events.Add(1)
	return ErrPublishDropped
}

func (mb *MessageBus) SubscribeEvents(ctx context.Context) (EventMessage, bool) {
	select {
	case msg, ok := <-mb.events:
		if !ok {
			return EventMessage{}, false
		}
		return msg, true
	case <-ctx.Done():
		return EventMessage{}, false
	}
}

func (mb *MessageBus) DroppedEvents() uint64 {
	return mb.dropped.events.Load()
}
