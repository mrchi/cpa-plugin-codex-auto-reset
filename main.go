// cpa-plugin-codex-auto-reset is a CLIProxyAPI plugin, built as a c-shared library and loaded
// from CPA's plugins directory.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

// The host api pointer is held on the Go side, so these helpers carry no state:
// cgo copies the preamble into two translation units, and a shared static would
// become two divergent variables.
static int call_host_api(const cliproxy_host_api* host, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (host == NULL || host->call == NULL) {
		return 1;
	}
	return host->call(host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(const cliproxy_host_api* host, void* ptr, size_t len) {
	if (host != NULL && host->free_buffer != NULL && ptr != NULL) {
		host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	pluginVersion = "0.1.0"
	pluginAuthor  = "mrchi"
	pluginRepo    = "https://github.com/mrchi/cpa-plugin-codex-auto-reset"

	// The host accepts any schema version up to pluginabi.SchemaVersion (6); this
	// plugin only relies on version 1 behaviours, so it advertises that.
	registrationSchemaVersion uint32 = 1
)

// activeHost is the production host seam. Tests call handleMethod directly.
var activeHost host = cgoHost{}

// currentConfig holds the latest register/reconfigure configuration.
var currentConfig atomic.Value

// activeDebounce is the process-wide debounce for the reset flow (D12).
var activeDebounce = newDebounce()

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if host == nil || plugin == nil {
		return 1
	}
	hostAPI = host
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := handleMethod(activeHost, C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

// handleMethod routes one RPC call (D6). Unknown methods are reported in an error
// envelope with a zero return code, not as transport failures.
func handleMethod(h host, method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		currentConfig.Store(parseConfig(h, request))
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodPluginShutdown:
		return okEnvelope(struct{}{})
	case pluginabi.MethodUsageHandle:
		return handleUsage(h, request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

// capabilities holds every capability this plugin claims; the host rejects a
// registration with no true capability (D7).
type capabilities struct {
	UsagePlugin bool `json:"usage_plugin"`
}

type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: registrationSchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginID,
			Version:          pluginVersion,
			Author:           pluginAuthor,
			GitHubRepository: pluginRepo,
		},
		Capabilities: capabilities{UsagePlugin: true},
	}
}

// handleUsage classifies one usage record, logs the decision, and hands a hit to the
// reset flow. CPA ignores the response (D22), so it returns immediately (D11): the
// reset flow runs on its own goroutine and never affects this request's outcome.
func handleUsage(h host, request []byte) ([]byte, error) {
	var record pluginapi.UsageRecord
	if errUnmarshal := json.Unmarshal(request, &record); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	cfg := loadedConfig()
	result := classify(record, cfg)
	if result.Reason == "" {
		return okEnvelope(struct{}{})
	}
	level := levelDebug
	if result.Hit {
		level = levelInfo
	}
	h.log(level, result.Reason, classificationFields(record, result))
	if result.Hit {
		startAutoReset(activeDebounce, h, cfg, record)
	}
	return okEnvelope(struct{}{})
}

// classificationFields carries everything needed to audit the decision after the
// fact: which credential and model, the upstream reset timing, and how the window was
// classified (spec story 17).
func classificationFields(record pluginapi.UsageRecord, result classification) map[string]any {
	fields := map[string]any{
		"model":             record.Model,
		"resets_in_seconds": result.ResetsInSeconds,
		"window":            result.Window,
		"window_source":     result.WindowSource,
	}
	if result.WindowMinutes > 0 {
		fields["limit_window_minutes"] = result.WindowMinutes
	}
	if result.ResetsSource != "" {
		fields["resets_source"] = result.ResetsSource
	}
	return recordFields(record, fields)
}

// hostAPI is written once by cliproxy_plugin_init, before any RPC call arrives.
var hostAPI *C.cliproxy_host_api

// callHostAPI performs one host callback and returns its raw response bytes.
func callHostAPI(method string, payload []byte) ([]byte, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var cResponse C.cliproxy_buffer
	var cRequest *C.uint8_t
	if len(payload) > 0 {
		cRequest = (*C.uint8_t)(C.CBytes(payload))
		defer C.free(unsafe.Pointer(cRequest))
	}
	if C.call_host_api(hostAPI, cMethod, cRequest, C.size_t(len(payload)), &cResponse) != 0 {
		return nil, fmt.Errorf("host call %s failed", method)
	}
	if cResponse.ptr == nil {
		return nil, fmt.Errorf("host call %s returned no response", method)
	}
	raw := C.GoBytes(cResponse.ptr, C.int(cResponse.len))
	C.free_host_buffer(hostAPI, cResponse.ptr, cResponse.len)
	return raw, nil
}

// callHost decodes the RPC envelope the host returns for a callback (D4).
func callHost(method string, payload []byte, out any) error {
	raw, errCall := callHostAPI(method, payload)
	if errCall != nil {
		return errCall
	}
	var env pluginabi.Envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		return fmt.Errorf("decode %s envelope: %w", method, errUnmarshal)
	}
	if !env.OK {
		if env.Error != nil {
			return fmt.Errorf("%s: %s", method, env.Error.Message)
		}
		return fmt.Errorf("%s: host reported failure", method)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(env.Result, out)
}

func okEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(pluginabi.Envelope{OK: false, Error: pluginabi.NewError(code, message)})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}
