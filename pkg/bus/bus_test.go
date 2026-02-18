package bus

import (
	"context"
	"testing"
)

func TestMessageBus_PublishInboundDropsWhenBufferFull(t *testing.T) {
	mb := NewMessageBus()
	defer mb.Close()

	for i := 0; i < cap(mb.inbound); i++ {
		mb.PublishInbound(InboundMessage{Channel: "test", SenderID: "u", ChatID: "c", Content: "msg"})
	}

	mb.PublishInbound(InboundMessage{Channel: "test", SenderID: "u", ChatID: "c", Content: "overflow"})
	if mb.DroppedInbound() != 1 {
		t.Fatalf("expected dropped inbound count 1, got %d", mb.DroppedInbound())
	}
}

func TestMessageBus_PublishOutboundDropsWhenBufferFull(t *testing.T) {
	mb := NewMessageBus()
	defer mb.Close()

	for i := 0; i < cap(mb.outbound); i++ {
		mb.PublishOutbound(OutboundMessage{Channel: "test", ChatID: "c", Content: "msg"})
	}

	mb.PublishOutbound(OutboundMessage{Channel: "test", ChatID: "c", Content: "overflow"})
	if mb.DroppedOutbound() != 1 {
		t.Fatalf("expected dropped outbound count 1, got %d", mb.DroppedOutbound())
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
