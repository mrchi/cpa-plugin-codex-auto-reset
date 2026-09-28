package main

import (
	"sync"
	"testing"
	"time"
)

// TestResetStateSerializesAndSuppresses pins the debounce contract (D12) on an
// injected clock, so the five-minute window is exercised without waiting for it.
func TestResetStateSerializesAndSuppresses(t *testing.T) {
	state := newResetState()
	clock := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	state.now = func() time.Time { return clock }

	if !state.begin("a") {
		t.Fatal("first begin = false, want true")
	}
	if state.begin("a") {
		t.Fatal("begin while a flow is in flight = true, want false")
	}
	if !state.begin("b") {
		t.Fatal("begin for another credential = false, want true")
	}
	state.finish("b", false)

	// A failed flow releases the slot but suppresses nothing.
	state.finish("a", false)
	if !state.begin("a") {
		t.Fatal("begin after a failed flow = false, want true")
	}

	// A credited flow suppresses the credential for the configured window.
	clock = clock.Add(time.Minute)
	state.finish("a", true)
	if state.begin("a") {
		t.Fatal("begin right after a credited flow = true, want false")
	}
	clock = clock.Add(resetSuppressWindow - time.Second)
	if state.begin("a") {
		t.Fatal("begin just before the window ends = true, want false")
	}
	clock = clock.Add(2 * time.Second)
	if !state.begin("a") {
		t.Fatal("begin after the window = false, want true")
	}
}

// TestResetStateBeginHasOneWinner pins that concurrent signals for one credential
// serialize: exactly one may start a flow.
func TestResetStateBeginHasOneWinner(t *testing.T) {
	state := newResetState()
	const attempts = 64

	var wait sync.WaitGroup
	winners := make(chan struct{}, attempts)
	for range attempts {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if state.begin("a") {
				winners <- struct{}{}
			}
		}()
	}
	wait.Wait()
	close(winners)

	if got := len(winners); got != 1 {
		t.Fatalf("winners = %d, want exactly 1", got)
	}
}
