package logx

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// A record's channel comes from the call site, not from the text. These tests
// pin that: the console's whole filter depends on it, and a reworded message
// must never be able to move a line between channels.
func TestRecordCarriesTheChannelOfItsCallSite(t *testing.T) {
	Reset()
	Task.Infof("checkin alice: ok")
	Dialog.Warnf("chat attempt 1/3 failed: %v", "boom")
	Infof("listening on 127.0.0.1:6828")

	got := Recent(0)
	if len(got) != 3 {
		t.Fatalf("got %d records, want 3", len(got))
	}
	want := []Channel{ChannelTask, ChannelDialog, ChannelSystem}
	for i, rec := range got {
		if rec.Channel != want[i] {
			t.Errorf("record %d (%q) channel = %q, want %q", i, rec.Text, rec.Channel, want[i])
		}
	}
	if got[1].Level != "warn" {
		t.Errorf("level = %q, want warn", got[1].Level)
	}
	if got[1].Text != "chat attempt 1/3 failed: boom" {
		t.Errorf("text = %q", got[1].Text)
	}
}

func TestRingEvictsOldestAndKeepsSequence(t *testing.T) {
	Reset()
	first := seqOf(t)
	for i := 0; i < ringCapacity+10; i++ {
		Task.Infof("sweep %d", i)
	}
	got := Recent(0)
	if len(got) != ringCapacity {
		t.Fatalf("ring kept %d records, want %d", len(got), ringCapacity)
	}
	if got[0].Seq != first+11 {
		t.Fatalf("oldest kept seq = %d, want %d", got[0].Seq, first+11)
	}
	if got[len(got)-1].Seq != first+uint64(ringCapacity+10) {
		t.Fatalf("newest seq = %d", got[len(got)-1].Seq)
	}
}

// Recent caps to the newest n while still returning them oldest-first, which is
// the order the console appends them in.
func TestRecentReturnsOldestFirstTrimmedToNewest(t *testing.T) {
	Reset()
	for i := 0; i < 10; i++ {
		Task.Infof("line %d", i)
	}
	got := Recent(3)
	if len(got) != 3 {
		t.Fatalf("got %d, want 3", len(got))
	}
	if !strings.HasSuffix(got[0].Text, "line 7") || !strings.HasSuffix(got[2].Text, "line 9") {
		t.Fatalf("wrong window: %q .. %q", got[0].Text, got[2].Text)
	}
}

func TestSinceSkipsWhatTheClientAlreadyHas(t *testing.T) {
	Reset()
	for i := 0; i < 4; i++ {
		Task.Infof("line %d", i)
	}
	all := Recent(0)
	got := Since(all[1].Seq, 0)
	if len(got) != 2 {
		t.Fatalf("got %d records after seq %d, want 2", len(got), all[1].Seq)
	}
	if got[0].Seq != all[2].Seq {
		t.Fatalf("resumed at seq %d, want %d", got[0].Seq, all[2].Seq)
	}
	if len(Since(all[len(all)-1].Seq, 0)) != 0 {
		t.Fatal("a fully caught-up client must get nothing")
	}
}

func TestSubscribeDeliversAndCancelIsSafeTwice(t *testing.T) {
	Reset()
	feed, cancel := Subscribe()

	var wg sync.WaitGroup
	wg.Add(1)
	var got []Record
	go func() {
		defer wg.Done()
		deadline := time.After(2 * time.Second)
		for len(got) < 2 {
			select {
			case rec := <-feed:
				got = append(got, rec)
			case <-deadline:
				return
			}
		}
	}()

	Task.Infof("one")
	Dialog.Infof("two")
	wg.Wait()
	cancel()
	cancel() // stopping twice must not panic or double-close

	if len(got) != 2 {
		t.Fatalf("subscriber received %d records, want 2", len(got))
	}
	if got[0].Channel != ChannelTask || got[1].Channel != ChannelDialog {
		t.Fatalf("channels = %q, %q", got[0].Channel, got[1].Channel)
	}
}

// A subscriber that stops draining must cost lost lines, never a blocked
// gateway: the emit path is on every request.
func TestSlowSubscriberDoesNotBlockEmit(t *testing.T) {
	Reset()
	_, cancel := Subscribe()
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			Task.Infof("flood %d", i)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("emit blocked behind a subscriber that never reads")
	}
}

func TestDebugIsSuppressedAtInfoLevel(t *testing.T) {
	Reset()
	SetLevel(LevelInfo)
	defer SetLevel(LevelInfo)
	Debugf("not emitted")
	if n := len(Recent(0)); n != 0 {
		t.Fatalf("debug record leaked past the level filter: %d", n)
	}
}

// seqOf reports the sequence number the ring would assign next, so a test can
// assert on absolute positions without depending on what ran before it.
func seqOf(t *testing.T) uint64 {
	t.Helper()
	Task.Infof("sentinel")
	recs := Recent(1)
	if len(recs) != 1 {
		t.Fatal("sentinel was not recorded")
	}
	Reset()
	return recs[0].Seq
}

// One record, one row: an upstream error body arrives with newlines and a
// megabyte of HTML, and the console draws a terminal.
func TestRecordFitsOnOneLineAndIsBounded(t *testing.T) {
	Reset()
	Task.Infof("upstream 401:\n<html>\r\n<body>%s", strings.Repeat("x", maxRecordRunes*2))
	got := Recent(1)
	if len(got) != 1 {
		t.Fatalf("got %d records", len(got))
	}
	if strings.Contains(got[0].Text, "\n") {
		t.Fatalf("record still has a newline: %q", got[0].Text[:40])
	}
	if !strings.HasSuffix(got[0].Text, "…") {
		t.Fatal("a very long record must be clipped, not stored whole")
	}
	if r := []rune(got[0].Text); len(r) != maxRecordRunes+1 {
		t.Fatalf("clipped to %d runes, want %d", len(r), maxRecordRunes+1)
	}
}
