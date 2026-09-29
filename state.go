package main

import (
	"sync"
	"time"
)

// clock is the single process-wide time source: the debounce window, credit expiry
// and the resets_at fallback all read it through now(). Tests swap it out
// (useFakeClock) so no case waits for real time, and the reset flow reads it from its
// own goroutine (D11), so the swap is guarded.
var clock = struct {
	sync.Mutex
	read func() time.Time
}{read: time.Now}

func now() time.Time {
	clock.Lock()
	defer clock.Unlock()
	return clock.read()
}

// setClock installs a time source and returns the undo.
func setClock(read func() time.Time) func() {
	clock.Lock()
	previous := clock.read
	clock.read = read
	clock.Unlock()
	return func() { setClock(previous) }
}

// resetSuppressWindow is how long a credential is left alone after a settled flow:
// in-flight failure records from the requests that raced it would otherwise repeat a
// consume, or the upstream calls that just established there is nothing to do
// (D12, spec story 14).
const resetSuppressWindow = 5 * time.Minute

// The two reasons a signal is dropped, kept apart so the host log can tell a flow
// already running (spec story 13) from a credential inside its suppression window
// (spec story 14).
const (
	reasonResetInFlight   = "auto-reset: reset already in flight: signal dropped"
	reasonResetSuppressed = "auto-reset: credential suppressed after a recent reset: signal dropped"
)

// debounce is the in-process guard for the reset flow, keyed by auth index. It is
// deliberately not persisted: a restart forgets it, which costs at most one extra
// suppressed opportunity to reset (D12).
type debounce struct {
	mu     sync.Mutex
	states map[string]*credentialState
}

// credentialState is one credential's debounce entry: whether a flow is running for it
// and until when further signals are ignored.
type credentialState struct {
	inFlight        bool
	suppressedUntil time.Time
}

func newDebounce() *debounce {
	return &debounce{states: map[string]*credentialState{}}
}

// begin claims the credential for one flow. When it cannot, the returned reason is the
// log line explaining the drop. The caller drops the signal instead of waiting, so
// goroutines cannot pile up (D12).
func (d *debounce) begin(authIndex string) (bool, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	state := d.states[authIndex]
	if state == nil {
		state = &credentialState{}
		d.states[authIndex] = state
	}
	if state.inFlight {
		return false, reasonResetInFlight
	}
	if now().Before(state.suppressedUntil) {
		return false, reasonResetSuppressed
	}
	state.inFlight = true
	return true, ""
}

// finish releases the credential and, when the flow settled its outcome, suppresses it
// for the configured window.
func (d *debounce) finish(authIndex string, settled bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	state := d.states[authIndex]
	if state == nil {
		return
	}
	state.inFlight = false
	if settled {
		state.suppressedUntil = now().Add(resetSuppressWindow)
	}
}
