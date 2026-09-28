package main

import (
	"fmt"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// fakeHost scripts every host callback in-process, so plugin logic is testable
// without CPA and without a compiled c-shared binary.
type fakeHost struct {
	mu          sync.Mutex
	logs        []fakeLog
	auths       map[string]pluginapi.HostAuthGetResponse
	authEntries []pluginapi.HostAuthFileEntry
	routes      []fakeHTTPRoute
	httpHandler func(httpRequest) (pluginapi.HTTPResponse, error)

	httpCalls []httpRequest
}

type fakeLog struct {
	Level   string
	Message string
	Fields  map[string]any
}

// fakeHTTPRoute scripts one host.http.do response; the first route whose method
// matches and whose URL is a prefix of the request URL wins.
type fakeHTTPRoute struct {
	Method string
	URL    string
	Status int
	Body   string
}

func newFakeHost(routes ...fakeHTTPRoute) *fakeHost {
	return &fakeHost{
		auths:  map[string]pluginapi.HostAuthGetResponse{},
		routes: routes,
	}
}

func (f *fakeHost) log(level, message string, fields map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, fakeLog{Level: level, Message: message, Fields: fields})
}

func (f *fakeHost) authGet(authIndex string) (pluginapi.HostAuthGetResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, okAuth := f.auths[authIndex]
	if !okAuth {
		return pluginapi.HostAuthGetResponse{}, fmt.Errorf("auth %q not found", authIndex)
	}
	return entry, nil
}

func (f *fakeHost) authList() ([]pluginapi.HostAuthFileEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authEntries, nil
}

func (f *fakeHost) httpDo(request httpRequest) (pluginapi.HTTPResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.httpCalls = append(f.httpCalls, request)
	if f.httpHandler != nil {
		return f.httpHandler(request)
	}
	for _, route := range f.routes {
		if route.Method == request.Method && strings.HasPrefix(request.URL, route.URL) {
			return pluginapi.HTTPResponse{StatusCode: route.Status, Body: []byte(route.Body)}, nil
		}
	}
	return pluginapi.HTTPResponse{}, fmt.Errorf("unscripted request %s %s", request.Method, request.URL)
}

func (f *fakeHost) logged() []fakeLog {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeLog(nil), f.logs...)
}

func (f *fakeHost) requests() []httpRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]httpRequest(nil), f.httpCalls...)
}
