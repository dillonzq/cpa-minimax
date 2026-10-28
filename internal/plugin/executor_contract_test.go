package plugin

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestEffectivePayloadContract(t *testing.T) {
	cases := []struct {
		name, model, input, output, original, payload, marker string
		streamFrames                                          []string
	}{
		{"intercepted chat", "chat-model", "openai", "openai", `{"messages":[{"role":"user","content":"original"}]}`, `{"messages":[{"role":"user","content":"intercepted"}]}`, "intercepted", chatSSEFrames},
		{"empty original", "chat-model", "openai", "claude", "", `{"messages":[{"role":"user","content":"effective"}]}`, "effective", chatSSEFrames},
		{"malformed original", "chat-model", "openai", "openai-response", "{", `{"messages":[{"role":"user","content":"effective"}]}`, "effective", chatSSEFrames},
		{"already translated payload", "chat-model", "openai", "claude", `{"messages":[{"role":"user","content":[{"type":"text","text":"original"}]}]}`, `{"messages":[{"role":"user","content":"effective"}],"native_extra":42}`, "effective", chatSSEFrames},
		{"legacy original only", "chat-model", "openai", "openai", `{"messages":[{"role":"user","content":"legacy"}]}`, "", "legacy", chatSSEFrames},
		{"responses format fallback", "responses-model", "", "openai-response", "", `{"input":"effective"}`, "effective", responsesSSEFrames},
		{"messages input responses output", "messages-alt", "claude", "openai-response", "", `{"messages":[{"role":"user","content":"effective"}],"max_tokens":16}`, "effective", messagesSSEFrames},
		{"native files", "responses-model", "openai-response", "openai-response", "", `{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file_example"}]}]}`, "file_example", responsesSSEFrames},
		{"native image files", "responses-model", "openai-response", "openai-response", "", `{"input":[{"role":"user","content":[{"type":"input_image","file_id":"image_file_example"}]}]}`, "image_file_example", responsesSSEFrames},
		{"native unknown messages blocks", "messages-alt", "claude", "claude", "", `{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"url","url":"https://file.invalid/sample"}},{"type":"future_part","value":9007199254740993}]}],"max_tokens":16}`, "future_part", messagesSSEFrames},
	}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			name := tc.name + "/nonstream"
			if stream {
				name = tc.name + "/stream"
			}
			t.Run(name, func(t *testing.T) {
				var m *Manager
				var f *fakeCaller
				if stream {
					m, f = newStreamManager(t, streamScript{upstreamID: "up-contract", frames: tc.streamFrames})
				} else {
					m, f = newExecManager(t)
				}
				var payload []byte
				if tc.payload != "" {
					payload = []byte(tc.payload)
				}
				req := executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: "minimax/" + tc.model, AuthProvider: ProviderID, AuthAttributes: map[string]string{"api_key": testKey}, SourceFormat: tc.input, Format: tc.output, OriginalRequest: []byte(tc.original), Payload: payload, Stream: stream}, StreamID: "down-contract", HostCallbackID: "scope-contract"}
				body, _ := json.Marshal(req)
				method := pluginabi.MethodExecutorExecute
				if stream {
					method = pluginabi.MethodExecutorExecuteStream
				}
				raw, err := m.HandleCall(method, body)
				if err != nil || !decodeEnv(t, raw).OK {
					t.Fatalf("execute: %v, %s", err, raw)
				}
				if !m.bridge.WaitForInFlight(time.Second) {
					t.Fatal("execution did not drain")
				}
				method = pluginabi.MethodHostHTTPDo
				if stream {
					method = pluginabi.MethodHostHTTPDoStream
				}
				calls := f.callsOf(method)
				var wire hostHTTPReq
				if len(calls) == 0 || json.Unmarshal(calls[len(calls)-1].payload, &wire) != nil {
					t.Fatal("no HTTP request")
				}
				if !strings.Contains(string(wire.Body), tc.marker) || strings.Contains(string(wire.Body), "original") {
					t.Fatal("effective payload lost")
				}
				if wire.HostCallbackID != "scope-contract" || wire.OperationID == "" {
					t.Fatal("missing request ownership")
				}
				if tc.name == "already translated payload" && !strings.Contains(string(wire.Body), `"native_extra":42`) {
					t.Fatal("payload was converted twice")
				}
				if stream {
					closes := f.callsOf(pluginabi.MethodHostStreamClose)
					if len(closes) != 1 || strings.Contains(string(closes[0].payload), `"error"`) {
						t.Fatal("valid stream failed")
					}
				} else {
					var response pluginapi.ExecutorResponse
					decodeResult(t, raw, &response)
					var output map[string]any
					_ = json.Unmarshal(response.Payload, &output)
					switch tc.output {
					case "claude":
						if _, ok := output["content"]; !ok {
							t.Fatal("output Format ignored")
						}
					case "openai-response":
						if _, ok := output["output"]; !ok {
							t.Fatal("output Format ignored")
						}
					case "openai":
						if _, ok := output["choices"]; !ok {
							t.Fatal("output Format ignored")
						}
					}
				}
			})
		}
	}
}

func TestEffectiveInvalidPayloadNeverFallsBack(t *testing.T) {
	for _, payload := range []string{"{", " ", `{"messages":[{"role":"user","content":[{"type":"input_file","file_id":"x"}]}]}`} {
		for _, stream := range []bool{false, true} {
			t.Run(payload, func(t *testing.T) {
				m, f := newExecManager(t)
				req := executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: "minimax/messages-alt", AuthProvider: ProviderID, AuthAttributes: map[string]string{"api_key": testKey}, SourceFormat: "openai", OriginalRequest: []byte(ccRequestBody), Payload: []byte(payload), Stream: stream}}
				raw, _ := m.HandleCall(pluginabi.MethodExecutorExecute, mustJSON(req))
				if decodeEnv(t, raw).OK {
					t.Fatal("invalid effective payload replaced with original")
				}
				if len(f.callsOf(pluginabi.MethodHostHTTPDoStream)) != 0 {
					t.Fatal("invalid request reached upstream")
				}
			})
		}
	}
}

func TestEffectivePayloadPresence(t *testing.T) {
	req := executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{Payload: []byte("null"), OriginalRequest: []byte(ccRequestBody)}}
	if string(req.effectivePayload()) != "null" {
		t.Fatal("present payload fell back to original")
	}
}

func TestExplicitEmptyPayloadDoesNotReplayOriginal(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, canonical := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/canonical=%t", stream, canonical), func(t *testing.T) {
				var m *Manager
				var f *fakeCaller
				if stream {
					m, f = newStreamManager(t, streamScript{upstreamID: "empty-payload", frames: chatSSEFrames})
				} else {
					m, f = newExecManager(t)
				}
				req := executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{Model: "minimax/chat-model", AuthProvider: ProviderID, AuthAttributes: map[string]string{"api_key": testKey}, SourceFormat: "openai", OriginalRequest: []byte(ccRequestBody), Payload: []byte{}, Stream: stream}, StreamID: "down-empty"}
				if canonical {
					req.Metadata = map[string]any{"canonical_session_id": "explicit-session"}
				}
				before := len(f.callsOf(pluginabi.MethodHostHTTPDo))
				raw, _ := m.HandleCall(pluginabi.MethodExecutorExecute, mustJSON(req))
				if decodeEnv(t, raw).OK {
					t.Fatal("explicit empty payload was replaced with original content")
				}
				if len(f.callsOf(pluginabi.MethodHostHTTPDo)) != before || len(f.callsOf(pluginabi.MethodHostHTTPDoStream)) != 0 {
					t.Fatal("empty effective payload reached upstream")
				}
			})
		}
	}
}
