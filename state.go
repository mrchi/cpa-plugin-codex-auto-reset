package main

import (
	"sync"
	"time"
)

// resetSuppressWindow is how long a credential is left alone after a credit was
// consumed: in-flight failure records from the requests that raced the reset would
// otherwise trigger a second, pointless consume (D12, spec story 14).
const resetSuppressWindow = 5 * time.Minute

// resetState is the in-process debounce for the reset flow, keyed by auth index. It
// is deliberately not persisted: a restart forgets it, which costs at most one extra
// suppressed opportunity to reset (D12).
type resetState struct {
	mu       sync.Mutex
	creds    map[string]*credentialState
	now      func() time.Time
	suppress time.Duration
}

type credentialState struct {
	inFlight        bool
	suppressedUntil time.Time
}

func newResetState() *resetState {
	return &resetState{
		creds:    map[string]*credentialState{},
		now:      time.Now,
		suppress: resetSuppressWindow,
	}
}

// begin claims the reset slot for one credential, reporting false when a flow is
// already running or the credential is still inside the suppression window. The
// caller drops the signal instead of waiting, so goroutines cannot pile up (D12).
func (s *resetState) begin(authIndex string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.creds[authIndex]
	if state == nil {
		state = &credentialState{}
		s.creds[authIndex] = state
	}
	if state.inFlight || s.now().Before(state.suppressedUntil) {
		return false
	}
	state.inFlight = true
	return true
}

// finish releases the slot and, when a credit was consumed, suppresses the credential
// for the configured window.
func (s *resetState) finish(authIndex string, consumed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.creds[authIndex]
	if state == nil {
		return
	}
	state.inFlight = false
	if consumed {
		state.suppressedUntil = s.now().Add(s.suppress)
	}
}
