package logring

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// loggerOver returns a logger whose records are copied into the buffer, and the
// buffer itself. The wrapped handler writes to nowhere: a test does not need the
// stderr half.
func loggerOver(buf *Ring) *slog.Logger {
	discard := slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(buf.Wrap(discard))
}

func TestTheBufferKeepsTheNewestRecordsAndSaysHowManyItLost(t *testing.T) {
	buf := New(3)
	logger := loggerOver(buf)

	for _, message := range []string{"one", "two", "three", "four"} {
		logger.Info(message)
	}

	records := buf.Tail(10, slog.LevelDebug)
	if len(records) != 3 {
		t.Fatalf("the buffer holds %d records, want 3", len(records))
	}
	// Oldest first, and "one" is the record that fell off the end.
	want := []string{"two", "three", "four"}
	for index, record := range records {
		if record.Message != want[index] {
			t.Errorf("record %d is %q, want %q", index, record.Message, want[index])
		}
	}
	if buf.Dropped() != 1 {
		t.Errorf("dropped = %d, want 1", buf.Dropped())
	}
	if buf.Kept() != 3 || buf.Capacity() != 3 {
		t.Errorf("kept = %d, capacity = %d, want 3 and 3", buf.Kept(), buf.Capacity())
	}
}

func TestASequenceNumberNeverRepeatsSoAReaderCanResume(t *testing.T) {
	buf := New(2)
	logger := loggerOver(buf)

	logger.Info("one")
	logger.Info("two")
	logger.Info("three")

	records := buf.Tail(5, slog.LevelDebug)
	if len(records) != 2 {
		t.Fatalf("the buffer holds %d records, want 2", len(records))
	}
	// The first record is gone, so the numbers start at 2: a reader that saw
	// seq 1 can ask for what came after it and get exactly this.
	if records[0].Seq != 2 || records[1].Seq != 3 {
		t.Errorf("sequence numbers are %d and %d, want 2 and 3", records[0].Seq, records[1].Seq)
	}
	if buf.Newest() != 3 {
		t.Errorf("newest = %d, want 3", buf.Newest())
	}
}

func TestTheLevelFilterIsTheLogsOwnAndTheBufferCopiesWhatItPrints(t *testing.T) {
	// The wrapped handler decides what is logged; the buffer copies that and
	// nothing else. A console therefore cannot show DEBUG when the bot runs at
	// INFO, which is the honest answer rather than a missing feature.
	discard := slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelWarn})
	buf := New(8)
	logger := slog.New(buf.Wrap(discard))

	logger.Debug("debug")
	logger.Info("info")
	logger.Warn("warn")
	logger.Error("error")

	all := buf.Tail(10, slog.LevelDebug)
	if len(all) != 2 {
		t.Fatalf("the buffer holds %d records, want 2 (warn and error)", len(all))
	}
	// The filter on the way out narrows what was kept, and says so in the answer.
	warnings := buf.Tail(10, slog.LevelWarn)
	if len(warnings) != 2 {
		t.Fatalf("asking for WARN left %d records, want 2", len(warnings))
	}
	errors := buf.Tail(10, slog.LevelError)
	if len(errors) != 1 || errors[0].Message != "error" {
		t.Fatalf("asking for ERROR left %+v, want the one error", errors)
	}
}

func TestAttributesAreRenderedIncludingGroups(t *testing.T) {
	buf := New(8)
	// A logger with attributes, then one with a group: the copy has to carry
	// both, spelled the way the console would show them.
	logger := loggerOver(buf).With("component", "console").WithGroup("request")

	logger.Info("answered", "status", 200, "took", 3*time.Millisecond)

	records := buf.Tail(1, slog.LevelDebug)
	if len(records) != 1 {
		t.Fatalf("the buffer holds %d records, want 1", len(records))
	}
	got := map[string]string{}
	for _, attr := range records[0].Attrs {
		got[attr.Key] = attr.Value
	}
	if got["component"] != "console" {
		t.Errorf("component = %q, want console", got["component"])
	}
	if got["request.status"] != "200" {
		t.Errorf("request.status = %q, want 200", got["request.status"])
	}
	if got["request.took"] != "3ms" {
		t.Errorf("request.took = %q, want 3ms", got["request.took"])
	}
}

func TestTwoHandlersFromOneParentDoNotShareTheirAttributes(t *testing.T) {
	// WithAttrs is called on the same parent twice in real code (once per
	// component). If the copies shared a backing array the second would
	// overwrite the first, and one component's lines would be labelled with
	// another's.
	buf := New(8)
	base := slog.New(buf.Wrap(slog.NewTextHandler(io.Discard, nil)))
	first := base.With("who", "first")
	second := base.With("who", "second")

	first.Info("from first")
	second.Info("from second")

	records := buf.Tail(2, slog.LevelDebug)
	if len(records) != 2 {
		t.Fatalf("the buffer holds %d records, want 2", len(records))
	}
	for _, record := range records {
		var who string
		for _, attr := range record.Attrs {
			if attr.Key == "who" {
				who = attr.Value
			}
		}
		want := strings.TrimPrefix(record.Message, "from ")
		if who != want {
			t.Errorf("%q was labelled who=%q, want %q", record.Message, who, want)
		}
	}
}

func TestAnErrorAttributeKeepsItsMessage(t *testing.T) {
	buf := New(4)
	loggerOver(buf).Warn("it failed", "error", errors.New("the file is not readable"))

	records := buf.Tail(1, slog.LevelDebug)
	if len(records) != 1 {
		t.Fatalf("the buffer holds %d records, want 1", len(records))
	}
	if len(records[0].Attrs) != 1 || records[0].Attrs[0].Value != "the file is not readable" {
		t.Fatalf("attrs = %+v, want the error's own message", records[0].Attrs)
	}
}

func TestASubscriberSeesWhatArrivesAfterItSubscribed(t *testing.T) {
	buf := New(8)
	logger := loggerOver(buf)

	logger.Info("before")
	sub := buf.Subscribe(slog.LevelInfo)
	defer sub.Close()
	logger.Info("after")
	logger.Debug("too quiet for this reader")

	select {
	case record := <-sub.C:
		if record.Message != "after" {
			t.Fatalf("received %q, want after", record.Message)
		}
	case <-time.After(time.Second):
		t.Fatal("no record arrived within a second")
	}
	select {
	case record := <-sub.C:
		t.Fatalf("received %q, want nothing: DEBUG is below this reader's level", record.Message)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestASubscriberThatStopsReadingIsDroppedRatherThanBlockingTheWriter(t *testing.T) {
	// The writer is the bot's own logging. A reader that stopped reading must
	// cost lines, not milliseconds.
	buf := New(8)
	logger := loggerOver(buf)
	sub := buf.Subscribe(slog.LevelDebug)
	defer sub.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for index := 0; index < subscriberBuffer+64; index++ {
			logger.Info("filling")
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("logging blocked on a subscriber that was not reading")
	}
	if sub.Lost() == 0 {
		t.Error("lost = 0, want the dropped records to be counted")
	}
}

func TestClosingASubscriberTwiceIsSafeAndStopsDelivery(t *testing.T) {
	buf := New(8)
	logger := loggerOver(buf)
	sub := buf.Subscribe(slog.LevelDebug)

	sub.Close()
	sub.Close()
	// The channel is closed, so a receive returns at once instead of hanging.
	if _, open := <-sub.C; open {
		t.Error("the channel is still open after Close")
	}
	logger.Info("after the reader left")
	if got := buf.Kept(); got != 1 {
		t.Errorf("kept = %d, want 1: closing a reader must not stop the buffer", got)
	}
}

func TestAnEmptyBufferAnswersWithEmptySlicesRatherThanNothing(t *testing.T) {
	// An empty JSON array and null are different answers, and the console
	// renders them differently.
	buf := New(4)
	if records := buf.Tail(10, slog.LevelDebug); records == nil || len(records) != 0 {
		t.Errorf("Tail on an empty buffer = %#v, want an empty slice", records)
	}
	if buf.Newest() != 0 {
		t.Errorf("newest = %d, want 0", buf.Newest())
	}
}

func TestANilBufferIsUsableSoNoCallerNeedsASpecialCase(t *testing.T) {
	// The console may be built without a buffer; the logger may be built with a
	// nil one. Both promise to work rather than to panic.
	var buf *Ring
	logger := slog.New(buf.Wrap(slog.NewTextHandler(io.Discard, nil)))
	logger.Info("nothing keeps this")

	if got := buf.Tail(10, slog.LevelDebug); len(got) != 0 {
		t.Errorf("Tail = %+v, want nothing", got)
	}
	if buf.Capacity() != 0 || buf.Kept() != 0 || buf.Newest() != 0 || buf.Dropped() != 0 {
		t.Error("a nil buffer reported something")
	}
	sub := buf.Subscribe(slog.LevelDebug)
	sub.Close()
}

func TestConcurrentLoggingAndReadingIsSafe(t *testing.T) {
	// Run with -race: the bot logs from every goroutine it runs while a browser
	// polls the console.
	buf := New(16)
	logger := loggerOver(buf)

	var wait sync.WaitGroup
	for writer := 0; writer < 4; writer++ {
		wait.Add(1)
		go func(writer int) {
			defer wait.Done()
			for index := 0; index < 50; index++ {
				logger.With("writer", writer).Info("concurrent", "index", index)
			}
		}(writer)
	}
	for reader := 0; reader < 4; reader++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := 0; index < 50; index++ {
				records := buf.Tail(4, slog.LevelDebug)
				for _, record := range records {
					_ = record.Attrs
				}
			}
		}()
	}
	wait.Wait()

	if buf.Kept() != 16 {
		t.Errorf("kept = %d, want the buffer filled to 16", buf.Kept())
	}
	if buf.Dropped() == 0 {
		t.Error("dropped = 0, want the 200 records that did not fit to be counted")
	}
}

func TestARecordWithNoTimeIsStampedSoTheConsoleCanOrderIt(t *testing.T) {
	buf := New(4)
	ring := buf.Wrap(slog.NewTextHandler(io.Discard, nil))
	// A handler called directly with a zero record is what a hand-built record
	// looks like; it should still come out with a time.
	if err := ring.Handle(context.Background(), slog.Record{Message: "no clock"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	records := buf.Tail(1, slog.LevelDebug)
	if len(records) != 1 || records[0].Time.IsZero() {
		t.Fatalf("records = %+v, want one record with a time", records)
	}
}
