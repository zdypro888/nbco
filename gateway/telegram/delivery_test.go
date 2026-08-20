package telegram

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/zdypro888/nbco/store"
)

type memoryTelegramDeliveryLedger struct {
	part       *store.TelegramDeliveryPart
	deliveries int
	failures   int
}

func (l *memoryTelegramDeliveryLedger) BeginTelegramDeliveryPart(
	_ context.Context,
	key string,
	index, count int,
	chatID int64,
	hash string,
) (*store.TelegramDeliveryPart, bool, error) {
	if l.part != nil {
		return l.part, false, nil
	}
	l.part = &store.TelegramDeliveryPart{
		DeliveryKey: key, PartIndex: index, PartCount: count, ChatID: chatID,
		ContentHash: hash, Status: store.TelegramDeliveryStarted,
	}
	return l.part, true, nil
}

func (l *memoryTelegramDeliveryLedger) MarkTelegramDeliveryPartDelivered(
	_ context.Context,
	_ string,
	_ int,
	messageID int64,
	_ time.Time,
) error {
	l.deliveries++
	l.part.Status = store.TelegramDeliveryDelivered
	l.part.TelegramMessageID = &messageID
	return nil
}

func (l *memoryTelegramDeliveryLedger) MarkTelegramDeliveryPartFailed(
	_ context.Context,
	_ string,
	_ int,
	_ string,
) error {
	l.failures++
	l.part.Status = store.TelegramDeliveryFailed
	return nil
}

func TestDeliverTelegramPartOnceDoesNotRepeatAnEditOrSend(t *testing.T) {
	ledger := &memoryTelegramDeliveryLedger{}
	calls := 0
	deliver := func() (int, error) {
		calls++
		return 42, nil
	}

	messageID, settled, err := deliverTelegramPartOnce(
		context.Background(), ledger, "turn:7", 99, 0, 1, "final answer", deliver,
	)
	if err != nil || !settled || messageID != 42 || calls != 1 || ledger.deliveries != 1 {
		t.Fatalf("first delivery = id %d settled %v calls %d marks %d err %v",
			messageID, settled, calls, ledger.deliveries, err)
	}

	messageID, settled, err = deliverTelegramPartOnce(
		context.Background(), ledger, "turn:7", 99, 0, 1, "final answer", func() (int, error) {
			t.Fatal("settled Telegram part was executed again")
			return 0, nil
		},
	)
	if err != nil || !settled || messageID != 42 || calls != 1 {
		t.Fatalf("replay = id %d settled %v calls %d err %v", messageID, settled, calls, err)
	}
}

func TestDeliverTelegramPartOnceSettlesFailedBoundary(t *testing.T) {
	ledger := &memoryTelegramDeliveryLedger{}
	want := errors.New("transport failed")
	messageID, settled, err := deliverTelegramPartOnce(
		context.Background(), ledger, "turn:8", 99, 0, 1, "final answer", func() (int, error) {
			return 0, want
		},
	)
	if !errors.Is(err, want) || !settled || messageID != 0 || ledger.failures != 1 {
		t.Fatalf("failed delivery = id %d settled %v marks %d err %v", messageID, settled, ledger.failures, err)
	}
}
