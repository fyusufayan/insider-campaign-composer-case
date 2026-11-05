package clock

import (
	"testing"
	"time"
)

func TestFakeTimers(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f := NewFake(start)
	a, b := f.NewTimer(time.Second), f.NewTimer(3*time.Second)
	if f.Timers() != 2 {
		t.Fatalf("timers = %d", f.Timers())
	}
	f.Advance(999 * time.Millisecond)
	select {
	case <-a.C():
		t.Fatal("fired early")
	default:
	}
	f.Advance(time.Millisecond)
	if at := <-a.C(); !at.Equal(start.Add(time.Second)) {
		t.Errorf("fired at %v", at)
	}
	if !b.Stop() || b.Stop() || a.Stop() {
		t.Error("Stop results")
	}
	f.Advance(time.Hour)
	select {
	case <-b.C():
		t.Fatal("stopped timer fired")
	default:
	}
	if imm := f.NewTimer(0); len(imm.C()) != 1 {
		t.Error("non-positive timer did not fire at once")
	}
	f.Set(start) // never backwards
	if !f.Now().Equal(start.Add(time.Hour + time.Second)) {
		t.Errorf("now = %v", f.Now())
	}
	go func() { f.NewTimer(time.Second) }()
	if !f.BlockUntilTimers(1, 5*time.Second) {
		t.Error("BlockUntilTimers did not see the timer")
	}
	if (Real{}).Now().IsZero() {
		t.Error("real clock")
	}
}
