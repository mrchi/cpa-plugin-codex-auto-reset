package main

import (
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// fakeHost scripts every host callback in-process, so plugin logic is testable
// without CPA and without a compiled c-shared binary (D13).
type fakeHost struct {
	mu       sync.Mutex
	logs     []fakeLog
	auths    map[string]pluginapi.HostAuthGetResponse
	scripts  map[string]scriptedResponse
	failures map[string]error
	// httpHandler overrides the scripted responses when a case needs behaviour a
	// canned response cannot express, such as holding the flow in place.
	httpHandler func(httpRequest) (pluginapi.HTTPResponse, error)

	httpCalls []httpRequest
	authGets  []string
	// trace records every callback in order, so a test can assert a sequence that
	// spans callback kinds (auth.get → http.do …).
	trace []string
}

type fakeLog struct {
	Level   string
	Message string
	Fields  map[string]any
}

type scriptedResponse struct {
	status int
	body   string
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		auths:    map[string]pluginapi.HostAuthGetResponse{},
		scripts:  map[string]scriptedResponse{},
		failures: map[string]error{},
	}
}

// script answers the next METHOD url callback with one canned response.
func (f *fakeHost) script(method, url string, status int, body string) *fakeHost {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts[method+" "+url] = scriptedResponse{status: status, body: body}
	return f
}

// scriptFailure makes the next METHOD url callback fail at the transport level.
func (f *fakeHost) scriptFailure(method, url string, err error) *fakeHost {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures[method+" "+url] = err
	return f
}

// withCredential makes host.auth.get answer for one auth index.
func (f *fakeHost) withCredential(authIndex, authJSON string) *fakeHost {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auths[authIndex] = pluginapi.HostAuthGetResponse{JSON: []byte(authJSON)}
	return f
}

func (f *fakeHost) log(level, message string, fields map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, fakeLog{Level: level, Message: message, Fields: fields})
}

func (f *fakeHost) authGet(authIndex string) (pluginapi.HostAuthGetResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authGets = append(f.authGets, authIndex)
	f.trace = append(f.trace, "auth.get "+authIndex)
	entry, okAuth := f.auths[authIndex]
	if !okAuth {
		return pluginapi.HostAuthGetResponse{}, fmt.Errorf("auth %q not found", authIndex)
	}
	return entry, nil
}

func (f *fakeHost) httpDo(request httpRequest) (pluginapi.HTTPResponse, error) {
	f.mu.Lock()
	f.httpCalls = append(f.httpCalls, request)
	f.trace = append(f.trace, "http.do "+request.Method+" "+request.URL)
	handler := f.httpHandler
	f.mu.Unlock()
	// The lock is released before answering: a scripted handler may block to hold the
	// flow in place, and other callbacks must not queue behind it.
	if handler != nil {
		return handler(request)
	}
	return f.answer(request)
}

// answer returns the scripted result for one request, so a handler that only needs to
// hold some calls in place can fall through to the normal scripting.
func (f *fakeHost) answer(request httpRequest) (pluginapi.HTTPResponse, error) {
	f.mu.Lock()
	key := request.Method + " " + request.URL
	scripted, okScripted := f.scripts[key]
	errScripted := f.failures[key]
	f.mu.Unlock()
	if errScripted != nil {
		return pluginapi.HTTPResponse{}, errScripted
	}
	if !okScripted {
		return pluginapi.HTTPResponse{}, fmt.Errorf("unscripted request %s %s", request.Method, request.URL)
	}
	return pluginapi.HTTPResponse{StatusCode: scripted.status, Body: []byte(scripted.body)}, nil
}

func (f *fakeHost) clearLogs() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = nil
}

func (f *fakeHost) logged() []fakeLog {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeLog(nil), f.logs...)
}

// requestsFor returns the recorded callbacks for one endpoint, so assertions name the
// request they mean instead of an index into the call list.
func (f *fakeHost) requestsFor(method, url string) []httpRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var matched []httpRequest
	for _, request := range f.httpCalls {
		if request.Method == method && request.URL == url {
			matched = append(matched, request)
		}
	}
	return matched
}

func (f *fakeHost) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.httpCalls)
}

func (f *fakeHost) authGetCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.authGets...)
}

func (f *fakeHost) callTrace() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.trace...)
}

// fakeClock is the replaceable process clock (state.go). Guarded by a mutex because
// the reset flow reads it from its own goroutine (D11) while the test advances it.
type fakeClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeClock) advance(elapsed time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(elapsed)
}

// useFakeClock hands one test a clock it controls and a fresh debounce state, so no
// case waits for real time and no case's suppression window leaks into the next one.
// Both are process-wide values (state.go), which is why the seam is a single helper.
func useFakeClock(t *testing.T, at time.Time) *fakeClock {
	t.Helper()
	fake := &fakeClock{at: at}
	restore := setClock(fake.now)
	activeReset = newResetState()
	t.Cleanup(func() {
		restore()
		activeReset = newResetState()
	})
	return fake
}

// testClock is the instant every case starts from. Credit expiry and resets_at values
// are expressed relative to it, so cases do not depend on the wall clock.
var testClock = time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)

// registerConfig performs the register RPC a host sends before any usage record.
func registerConfig(t *testing.T, fake *fakeHost, configYAML string) {
	t.Helper()
	if _, errHandle := handleMethod(fake, pluginabi.MethodPluginRegister, lifecyclePayload(t, configYAML)); errHandle != nil {
		t.Fatalf("plugin.register: %v", errHandle)
	}
}

// sendUsage performs one usage.handle RPC and checks the empty success envelope the
// host expects (D22).
func sendUsage(t *testing.T, fake *fakeHost, record pluginapi.UsageRecord) {
	t.Helper()
	raw, errHandle := handleMethod(fake, pluginabi.MethodUsageHandle, marshalRecord(t, record))
	if errHandle != nil {
		t.Fatalf("usage.handle: %v", errHandle)
	}
	if result := string(decodeResult(t, raw)); result != "{}" {
		t.Errorf("usage.handle result = %s, want {}", result)
	}
}

// waitFor polls a condition until it holds. The reset flow runs on its own goroutine
// (D11), so tests wait on an observable host-call or log effect rather than sleeping.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within 2s")
		}
		time.Sleep(time.Millisecond)
	}
}

func logsWithMessage(logs []fakeLog, message string) []fakeLog {
	var matched []fakeLog
	for _, entry := range logs {
		if entry.Message == message {
			matched = append(matched, entry)
		}
	}
	return matched
}

// waitForLog waits until the flow has written one specific line. Every reset path ends
// with its own line, so this is how a test observes that the flow finished.
func waitForLog(t *testing.T, fake *fakeHost, message string) fakeLog {
	t.Helper()
	var matched fakeLog
	waitFor(t, func() bool {
		found := logsWithMessage(fake.logged(), message)
		if len(found) == 0 {
			return false
		}
		matched = found[len(found)-1]
		return true
	})
	return matched
}

// hitRecord is the codex 429 weekly-exhaustion record that classifies as a hit.
func hitRecord() pluginapi.UsageRecord {
	return usageRecord(hitBody())
}

// hitBody is the weekly-exhaustion error body that makes classify() report a hit.
func hitBody() string {
	return weeklyBodyWith(`"resets_in_seconds":200000`)
}

// weeklyBodyWith renders a weekly-window 429 body around the given timing fields, so a
// case can vary resets_in_seconds and resets_at one at a time.
func weeklyBodyWith(fields string) string {
	return quotaBody(true, `"type":"usage_limit_reached","limit_window_minutes":10080,`+fields)
}

// resetsAtBody renders a weekly-window body whose only timing is a resets_at
// timestamp at the given instant, the shape that has no resets_in_seconds at all.
// The upstream body carries resets_at as integer Unix seconds, so the fixture does too;
// the legacy RFC3339-string shape is covered by the inline case in classify_test.go.
func resetsAtBody(at time.Time) string {
	return weeklyBodyWith(`"resets_at":` + strconv.FormatInt(at.Unix(), 10))
}

// timingOnlyBody renders a usage_limit_reached body with no window field at all, so
// the window can only come from the resets_in_seconds>18000 fallback.
func timingOnlyBody(fields string) string {
	return quotaBody(true, `"type":"usage_limit_reached",`+fields)
}
