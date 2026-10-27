package main

import (
	"sync"
	"time"
)

// Clock is the injectable time source for N1 state and future v5 code.
// The global injection point is SetClock; tests call SetClock(fake) and
// defer SetClock(nil) to restore the real clock.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

var (
	clockMu     sync.RWMutex
	globalClock Clock = realClock{}
)

// SetClock replaces the process clock. Passing nil restores the real clock.
func SetClock(c Clock) {
	clockMu.Lock()
	if c == nil {
		globalClock = realClock{}
	} else {
		globalClock = c
	}
	clockMu.Unlock()
}

// Now returns the current time from the injected clock.
func Now() time.Time {
	clockMu.RLock()
	c := globalClock
	clockMu.RUnlock()
	if c == nil {
		return time.Now()
	}
	return c.Now()
}
