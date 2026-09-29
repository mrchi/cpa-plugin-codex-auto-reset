package main

import (
	"encoding/json"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// host is the only seam between plugin logic and CPA: production talks to the host
// over cgo callbacks, tests substitute a fake (D13).
type host interface {
	// log writes through host.log. The host only recognises trace/info/warn/error;
	// any other value, "debug" included, lands at debug level (D13).
	log(level, message string, fields map[string]any)
	authGet(authIndex string) (pluginapi.HostAuthGetResponse, error)
	httpDo(req httpRequest) (pluginapi.HTTPResponse, error)
}

// httpRequest is the wire form of host.http.do. pluginapi.HTTPRequest carries no
// JSON tags, so marshalling it would produce keys the host does not read.
type httpRequest struct {
	Method  string      `json:"method"`
	URL     string      `json:"url"`
	Headers http.Header `json:"headers,omitempty"`
	Body    []byte      `json:"body,omitempty"`
}

type hostLogRequest struct {
	Level   string         `json:"level,omitempty"`
	Message string         `json:"message,omitempty"`
	Fields  map[string]any `json:"fields,omitempty"`
}

type cgoHost struct{}

func (cgoHost) log(level, message string, fields map[string]any) {
	payload, errMarshal := json.Marshal(hostLogRequest{Level: level, Message: message, Fields: fields})
	if errMarshal != nil {
		return
	}
	// Logging is best effort: a failed log must never fail the caller.
	_, _ = callHostAPI(pluginabi.MethodHostLog, payload)
}

func (cgoHost) authGet(authIndex string) (pluginapi.HostAuthGetResponse, error) {
	var response pluginapi.HostAuthGetResponse
	payload, errMarshal := json.Marshal(pluginapi.HostAuthGetRequest{AuthIndex: authIndex})
	if errMarshal != nil {
		return response, errMarshal
	}
	errCall := callHost(pluginabi.MethodHostAuthGet, payload, &response)
	return response, errCall
}

// httpDo performs the request through the host transport. There is no timeout field
// and the callback context carries no deadline, so a hung upstream blocks the caller
// for good: ponytail: upgrade to host.http.operation_open + cancel when that ceiling
// starts to hurt (D20).
func (cgoHost) httpDo(request httpRequest) (pluginapi.HTTPResponse, error) {
	var response pluginapi.HTTPResponse
	payload, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		return response, errMarshal
	}
	errCall := callHost(pluginabi.MethodHostHTTPDo, payload, &response)
	return response, errCall
}

// Log levels host.log recognises, plus levelDebug, which it maps to debug by not
// recognising it (D13). Named so a typo cannot silently demote a line to debug.
const (
	levelDebug = "debug"
	levelInfo  = "info"
	levelWarn  = "warn"
)

// logFields tags every record with the plugin id: the host only adds request_id, so
// records are otherwise unattributable (D13).
func logFields(extra map[string]any) map[string]any {
	fields := make(map[string]any, len(extra)+3)
	fields["plugin"] = pluginID
	for key, value := range extra {
		fields[key] = value
	}
	return fields
}

// recordFields is logFields plus the credential identity every per-record line carries.
func recordFields(record pluginapi.UsageRecord, extra map[string]any) map[string]any {
	fields := logFields(extra)
	fields["auth_id"] = record.AuthID
	fields["auth_index"] = record.AuthIndex
	return fields
}
