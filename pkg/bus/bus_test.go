package bus

import (
	"context"
	"errors"
	"testing"
)

func TestMessageBus_PublishInboundDropsWhenBufferFull(t *testing.T) {
	mb := NewMessageBus()
	defer mb.Close()

	for i := 0; i < cap(mb.inbound); i++ {
		if err := mb.PublishInbound(InboundMessage{Channel: "test", SenderID: "u", ChatID: "c", Content: "msg"}); err != nil {
			t.Fatalf("publish inbound seed %d: %v", i, err)
		}
	}

	err := mb.PublishInbound(InboundMessage{Channel: "test", SenderID: "u", ChatID: "c", Content: "overflow"})
	if !errors.Is(err, ErrPublishDropped) {
		t.Fatalf("expected ErrPublishDropped, got %v", err)
	}
	if mb.DroppedInbound() != 1 {
		t.Fatalf("expected dropped inbound count 1, got %d", mb.DroppedInbound())
	}
}

func TestMessageBus_PublishOutboundDropsWhenBufferFull(t *testing.T) {
	mb := NewMessageBus()
	defer mb.Close()

	for i := 0; i < cap(mb.outbound); i++ {
		if err := mb.PublishOutbound(OutboundMessage{Channel: "test", ChatID: "c", Content: "msg"}); err != nil {
			t.Fatalf("publish outbound seed %d: %v", i, err)
		}
	}

	err := mb.PublishOutbound(OutboundMessage{Channel: "test", ChatID: "c", Content: "overflow"})
	if !errors.Is(err, ErrPublishDropped) {
		t.Fatalf("expected ErrPublishDropped, got %v", err)
	}
	if mb.DroppedOutbound() != 1 {
		t.Fatalf("expected dropped outbound count 1, got %d", mb.DroppedOutbound())
	}
}

func TestMessageBus_PublishReturnsClosedError(t *testing.T) {
	mb := NewMessageBus()
	mb.Close()

	if err := mb.PublishInbound(InboundMessage{Channel: "test", SenderID: "u", ChatID: "c", Content: "msg"}); !errors.Is(err, ErrBusClosed) {
		t.Fatalf("expected inbound ErrBusClosed, got %v", err)
	}
	if err := mb.PublishOutbound(OutboundMessage{Channel: "test", ChatID: "c", Content: "msg"}); !errors.Is(err, ErrBusClosed) {
		t.Fatalf("expected outbound ErrBusClosed, got %v", err)
	}
}

func TestMessageBus_ClosedChannelsReturnFalse(t *testing.T) {
	mb := NewMessageBus()
	mb.Close()

	if _, ok := mb.ConsumeInbound(context.Background()); ok {
		t.Fatalf("expected closed inbound consume to return ok=false")
	}
	if _, ok := mb.SubscribeOutbound(context.Background()); ok {
		t.Fatalf("expected closed outbound subscribe to return ok=false")
	}
}
