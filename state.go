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

// resetSuppressWindow is how long a credential is left alone after a credit was
// consumed: in-flight failure records from the requests that raced the reset would
// otherwise trigger a second, pointless consume (D12, spec story 14).
const resetSuppressWindow = 5 * time.Minute

// resetState is the in-process debounce for the reset flow, keyed by auth index. It
// is deliberately not persisted: a restart forgets it, which costs at most one extra
// suppressed opportunity to reset (D12).
type resetState struct {
	mu    sync.Mutex
	slots map[string]*credentialSlot
}

// credentialSlot is one credential's debounce slot: whether a flow is running for it
// and, if a credit was just consumed, until when further signals are ignored.
type credentialSlot struct {
	inFlight        bool
	suppressedUntil time.Time
}

func newResetState() *resetState {
	return &resetState{slots: map[string]*credentialSlot{}}
}

// begin claims the reset slot for one credential. When it cannot, the returned reason
// is the log message explaining the drop, so the two cases stay distinguishable in the
// host log: a flow already running (spec story 13) versus a credential still inside
// its post-reset suppression window (spec story 14). The caller drops the signal
// instead of waiting, so goroutines cannot pile up (D12).
func (s *resetState) begin(authIndex string) (bool, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	slot := s.slots[authIndex]
	if slot == nil {
		slot = &credentialSlot{}
		s.slots[authIndex] = slot
	}
	if slot.inFlight {
		return false, reasonResetInFlight
	}
	if now().Before(slot.suppressedUntil) {
		return false, reasonResetSuppressed
	}
	slot.inFlight = true
	return true, ""
}

// finish releases the slot and, when a credit was consumed, suppresses the credential
// for the configured window.
func (s *resetState) finish(authIndex string, consumed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	slot := s.slots[authIndex]
	if slot == nil {
		return
	}
	slot.inFlight = false
	if consumed {
		slot.suppressedUntil = now().Add(resetSuppressWindow)
	}
}
