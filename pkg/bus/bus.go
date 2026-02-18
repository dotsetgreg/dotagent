package bus

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

type MessageBus struct {
	inbound  chan InboundMessage
	outbound chan OutboundMessage
	handlers map[string]MessageHandler
	closed   bool
	dropped  droppedCounters
	mu       sync.RWMutex
}

type droppedCounters struct {
	inbound  atomic.Uint64
	outbound atomic.Uint64
}

const publishTimeout = 100 * time.Millisecond

func NewMessageBus() *MessageBus {
	return &MessageBus{
		inbound:  make(chan InboundMessage, 100),
		outbound: make(chan OutboundMessage, 100),
		handlers: make(map[string]MessageHandler),
	}
}

func (mb *MessageBus) PublishInbound(msg InboundMessage) {
	mb.mu.RLock()
	defer mb.mu.RUnlock()
	if mb.closed {
		return
	}

	select {
	case mb.inbound <- msg:
	default:
		timer := time.NewTimer(publishTimeout)
		defer timer.Stop()
		select {
		case mb.inbound <- msg:
		case <-timer.C:
			mb.dropped.inbound.Add(1)
		}
	}
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

func (mb *MessageBus) PublishOutbound(msg OutboundMessage) {
	mb.mu.RLock()
	defer mb.mu.RUnlock()
	if mb.closed {
		return
	}

	select {
	case mb.outbound <- msg:
	default:
		timer := time.NewTimer(publishTimeout)
		defer timer.Stop()
		select {
		case mb.outbound <- msg:
		case <-timer.C:
			mb.dropped.outbound.Add(1)
		}
	}
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
}

func (mb *MessageBus) DroppedInbound() uint64 {
	return mb.dropped.inbound.Load()
}

func (mb *MessageBus) DroppedOutbound() uint64 {
	return mb.dropped.outbound.Load()
}
