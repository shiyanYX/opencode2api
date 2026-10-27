package main

import (
	"sync"
	"testing"
	"time"
)

type fakeR8Clock struct {
	mu  sync.Mutex
	now time.Time
}

func (f *fakeR8Clock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeR8Clock) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	f.mu.Unlock()
}

func TestR8ClockInjection(t *testing.T) {
	base := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	fake := &fakeR8Clock{now: base}
	SetClock(fake)
	defer SetClock(nil)

	if got := Now(); !got.Equal(base) {
		t.Fatalf("Now() = %v, want %v", got, base)
	}

	fake.Advance(90 * time.Second)
	want := base.Add(90 * time.Second)
	if got := Now(); !got.Equal(want) {
		t.Fatalf("Now() after advance = %v, want %v", got, want)
	}

	SetClock(nil)
	realBefore := time.Now()
	got := Now()
	if d := got.Sub(realBefore); d < -time.Second || d > time.Second {
		t.Fatalf("Now() after SetClock(nil) = %v, real %v", got, realBefore)
	}

	SetClock(fake)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := Now(); !got.Equal(want) {
				t.Errorf("concurrent Now() = %v, want %v", got, want)
			}
		}()
	}
	wg.Wait()
}
