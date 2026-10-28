package plugin

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/dillonzq/cpa-minimax/internal/catalog"
	"github.com/dillonzq/cpa-minimax/internal/errclass"
)

const minimaxCatalog = `{"object":"list","data":[{"id":"MiniMax-M3","object":"model","created":1,"owned_by":"minimax"}]}`

// newMiniMaxManager registers yamlText and answers every upstream call with
// respond (catalog fetches excluded).
func newMiniMaxManager(t *testing.T, yamlText string, respond func(wire hostHTTPReq) pluginapi.HTTPResponse) (*Manager, *fakeCaller) {
	t.Helper()
	f := &fakeCaller{}
	f.responder = wrapWithCatalog(minimaxCatalog, func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		var wire hostHTTPReq
		_ = json.Unmarshal(payload, &wire)
		return hostOK(respond(wire)), nil
	})
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	if env := decodeEnv(t, mustHandle(t, m, "plugin.register", lifecycleRequestBody(yamlText))); !env.OK {
		t.Fatalf("register: %+v", env.Error)
	}
	return m, f
}

func okJSON(body string) func(hostHTTPReq) pluginapi.HTTPResponse {
	return func(hostHTTPReq) pluginapi.HTTPResponse {
		return pluginapi.HTTPResponse{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body)}
	}
}

func executeAs(t *testing.T, m *Manager, attrs map[string]string, model, format, body string, stream bool) pluginabi.Envelope {
	t.Helper()
	req := executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		AuthProvider: ProviderID, AuthAttributes: attrs, Model: model, SourceFormat: format, Payload: []byte(body), Stream: stream,
	}, StreamID: "down-minimax"}
	return decodeEnv(t, mustHandle(t, m, pluginabi.MethodExecutorExecute, mustJSON(req)))
}

func lastHTTPRequest(t *testing.T, f *fakeCaller, method string) hostHTTPReq {
	t.Helper()
	calls := f.callsOf(method)
	if len(calls) == 0 {
		t.Fatalf("no %s calls", method)
	}
	var wire hostHTTPReq
	if err := json.Unmarshal(calls[len(calls)-1].payload, &wire); err != nil {
		t.Fatal(err)
	}
	return wire
}

func TestExecuteRoutesByCredentialRegion(t *testing.T) {
	cases := []struct {
		name, yaml, region, want string
	}{
		{"global key", "api-keys:\n  - value: sk-global\n", "global", "https://api.minimax.io/anthropic/v1/messages"},
		{"cn key", "api-keys:\n  - value: sk-cn\n", "cn", "https://api.minimax.cn/anthropic/v1/messages"},
		{"missing region uses default", "region: cn\napi-keys:\n  - value: sk-cn\n", "", "https://api.minimax.cn/anthropic/v1/messages"},
		{"shared base-url override", "base-url: https://api.minimaxi.com/anthropic\napi-keys:\n  - value: sk-cn\n", "cn", "https://api.minimaxi.com/anthropic/v1/messages"},
		{"key base-url overrides shared", "base-url: https://shared.test\napi-keys:\n  - value: selected-key\n    region: cn\n    base-url: https://key.test/anthropic/v1/\n", "global", "https://key.test/anthropic/v1/messages"},
		{"key base-url without shared", "api-keys:\n  - value: selected-key\n    base-url: https://key.test\n", "cn", "https://key.test/anthropic/v1/messages"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, f := newMiniMaxManager(t, tc.yaml, okJSON(claudeResponseBody))
			attrs := map[string]string{"api_key": "selected-key", "region": tc.region}
			if env := executeAs(t, m, attrs, "minimax/MiniMax-M3", "claude", claudeRequestBody, false); !env.OK {
				t.Fatalf("execute: %+v", env.Error)
			}
			wire := lastHTTPRequest(t, f, pluginabi.MethodHostHTTPDo)
			if wire.URL != tc.want || wire.Headers.Get("Authorization") != "Bearer selected-key" {
				t.Fatalf("request = %s %v", wire.URL, wire.Headers)
			}
		})
	}
	t.Run("stream", func(t *testing.T) {
		f := &fakeCaller{}
		f.responder = wrapWithCatalog(minimaxCatalog, streamResponder(streamScript{upstreamID: "up-cn", frames: messagesSSEFrames}))
		m := NewManager(NewHostBridge(f.call))
		t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
		mustHandle(t, m, "plugin.register", lifecycleRequestBody("base-url: https://shared.test\napi-keys:\n  - value: sk-cn\n    region: cn\n    base-url: https://stream-key.test/v1\n"))
		if env := executeAs(t, m, map[string]string{"api_key": "sk-cn", "region": "cn"}, "minimax/MiniMax-M3", "claude", `{"max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, true); !env.OK {
			t.Fatalf("execute stream: %+v", env.Error)
		}
		if got := lastHTTPRequest(t, f, pluginabi.MethodHostHTTPDoStream).URL; got != "https://stream-key.test/anthropic/v1/messages" {
			t.Fatalf("stream url = %s", got)
		}
	})
}

func TestMiniMaxRequestPolicy(t *testing.T) {
	cases := []struct {
		name  string
		route catalog.Route
		model string
		body  string
		want  map[string]any
	}{
		{"chat reasoning_split defaults true", catalog.RouteChatCompletions, "MiniMax-M3", `{"model":"MiniMax-M3"}`,
			map[string]any{"model": "MiniMax-M3", "reasoning_split": true}},
		{"chat explicit split kept for M3", catalog.RouteChatCompletions, "MiniMax-M3", `{"reasoning_split":false}`,
			map[string]any{"reasoning_split": false}},
		{"chat split forced for Flash", catalog.RouteChatCompletions, "MiniMax-M3.1-Flash", `{"reasoning_split":false}`,
			map[string]any{"reasoning_split": true}},
		{"chat split default for other models", catalog.RouteChatCompletions, "MiniMax-M2.7", `{}`,
			map[string]any{"reasoning_split": true}},
		{"chat Flash none raised and disabled thinking removed", catalog.RouteChatCompletions, "MiniMax-M3.1-Flash", `{"reasoning_effort":"none","thinking":{"type":"disabled"}}`,
			map[string]any{"reasoning_effort": "low", "reasoning_split": true}},
		{"chat M3 none kept", catalog.RouteChatCompletions, "MiniMax-M3", `{"reasoning_effort":"none"}`,
			map[string]any{"reasoning_effort": "none", "reasoning_split": true}},
		{"messages Flash disabled thinking adaptive", catalog.RouteMessages, "MiniMax-M3.1-Flash", `{"thinking":{"type":"disabled"},"output_config":{"effort":"none"}}`,
			map[string]any{"thinking": map[string]any{"type": "adaptive"}, "output_config": map[string]any{"effort": "low"}}},
		{"messages Flash budget thinking adaptive", catalog.RouteMessages, "MiniMax-M3.1-Flash", `{"thinking":{"type":"enabled","budget_tokens":1024}}`,
			map[string]any{"thinking": map[string]any{"type": "adaptive"}}},
		{"messages Flash without thinking untouched", catalog.RouteMessages, "MiniMax-M3.1-Flash", `{"max_tokens":8}`,
			map[string]any{"max_tokens": float64(8)}},
		{"messages M3 none disables thinking", catalog.RouteMessages, "MiniMax-M3", `{"thinking":{"type":"adaptive"},"output_config":{"effort":"none","format":"x"}}`,
			map[string]any{"thinking": map[string]any{"type": "disabled"}, "output_config": map[string]any{"format": "x"}}},
		{"messages M3 none drops empty output_config", catalog.RouteMessages, "MiniMax-M3", `{"output_config":{"effort":"none"}}`,
			map[string]any{"thinking": map[string]any{"type": "disabled"}}},
		{"messages M3 effort enables adaptive", catalog.RouteMessages, "MiniMax-M3", `{"output_config":{"effort":"high"}}`,
			map[string]any{"thinking": map[string]any{"type": "adaptive"}, "output_config": map[string]any{"effort": "high"}}},
		{"messages M3 budget thinking adaptive", catalog.RouteMessages, "MiniMax-M3", `{"thinking":{"type":"enabled","budget_tokens":2048}}`,
			map[string]any{"thinking": map[string]any{"type": "adaptive"}}},
		{"messages M3 without controls untouched", catalog.RouteMessages, "MiniMax-M3", `{"max_tokens":8}`,
			map[string]any{"max_tokens": float64(8)}},
		{"responses Flash none raised", catalog.RouteResponses, "MiniMax-M3.1-Flash", `{"reasoning":{"effort":"none","summary":"auto"}}`,
			map[string]any{"reasoning": map[string]any{"effort": "low", "summary": "auto"}}},
		{"responses M3 none kept", catalog.RouteResponses, "MiniMax-M3", `{"reasoning":{"effort":"none"}}`,
			map[string]any{"reasoning": map[string]any{"effort": "none"}}},
		{"messages M2 untouched", catalog.RouteMessages, "MiniMax-M2.7", `{"thinking":{"type":"disabled"},"output_config":{"effort":"none"}}`,
			map[string]any{"thinking": map[string]any{"type": "disabled"}, "output_config": map[string]any{"effort": "none"}}},
		{"responses M2 untouched", catalog.RouteResponses, "MiniMax-M2.5", `{"reasoning":{"effort":"none"}}`,
			map[string]any{"reasoning": map[string]any{"effort": "none"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := normalizeMiniMaxRequest(tc.route, tc.model, []byte(tc.body))
			var got map[string]any
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("output %s: %v", out, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %s, want %v", out, tc.want)
			}
		})
	}
	for _, body := range []string{`not json`, `null`, `[1]`} {
		if got := normalizeMiniMaxRequest(catalog.RouteChatCompletions, "MiniMax-M3", []byte(body)); string(got) != body {
			t.Fatalf("malformed body changed: %s -> %s", body, got)
		}
	}
}

// TestExecuteAppliesMiniMaxPolicyAfterSuffix pins the end-to-end order: a
// model-name suffix sets the control, then the MiniMax policy normalizes it.
func TestExecuteAppliesMiniMaxPolicyAfterSuffix(t *testing.T) {
	m, f := newMiniMaxManager(t, "api-keys:\n  - value: "+testKey+"\n", okJSON(claudeResponseBody))
	if env := executeAs(t, m, map[string]string{"api_key": testKey}, "minimax/MiniMax-M3(none)", "claude", claudeRequestBody, false); !env.OK {
		t.Fatalf("execute: %+v", env.Error)
	}
	body := upstreamRequestBody(t, f, pluginabi.MethodHostHTTPDo)
	if thinking, _ := body["thinking"].(map[string]any); thinking["type"] != "disabled" || body["output_config"] != nil {
		t.Fatalf("M3 none body = %v", body)
	}
	if env := executeAs(t, m, map[string]string{"api_key": testKey}, "minimax/MiniMax-M3.1-Flash(none)", "openai", ccRequestBody, false); !env.OK {
		t.Fatalf("execute: %+v", env.Error)
	}
	body = upstreamRequestBody(t, f, pluginabi.MethodHostHTTPDo)
	if body["reasoning_effort"] != "low" || body["reasoning_split"] != true {
		t.Fatalf("Flash none body = %v", body)
	}
}

func TestExecuteBaseRespErrorUnderHTTP200(t *testing.T) {
	cases := []struct {
		code       string
		wantClass  errclass.Class
		wantStatus int
		retryable  bool
	}{
		{"1004", errclass.ClassAuth, 401, true},
		{"1008", errclass.ClassBilling, 402, true},
		{"2056", errclass.ClassQuota, 429, true},
		{"2069", errclass.ClassInvalidModel, 404, false},
		{"9999", errclass.ClassUpstream, 502, true},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			m, _ := newMiniMaxManager(t, "api-keys:\n  - value: "+testKey+"\n", okJSON(`{"base_resp":{"status_code":`+tc.code+`,"status_msg":"failure"}}`))
			env := executeAs(t, m, map[string]string{"api_key": testKey}, "minimax/MiniMax-M3", "openai", ccRequestBody, false)
			if env.OK || env.Error == nil || env.Error.Code != string(tc.wantClass) || env.Error.HTTPStatus != tc.wantStatus || env.Error.Retryable != tc.retryable {
				t.Fatalf("envelope = %+v", env.Error)
			}
			if !strings.Contains(env.Error.Message, tc.code) {
				t.Fatalf("message lost code: %q", env.Error.Message)
			}
		})
	}
}

func TestExecuteMiniMaxErrorCodesDriveCooldownStatus(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		body       string
		wantClass  errclass.Class
		wantStatus int
	}{
		{"balance in 500", 500, `{"type":"error","error":{"type":"api_error","message":"insufficient balance (1008)"}}`, errclass.ClassBilling, 402},
		{"balance in 400", 400, `{"error":{"message":"insufficient balance (1008)","type":"invalid_request_error"}}`, errclass.ClassBilling, 402},
		{"plan usage in 429", 429, `{"type":"error","error":{"type":"rate_limit_error","message":"usage limit exceeded (2056)"}}`, errclass.ClassQuota, 429},
		{"region mismatch in base_resp", 401, `{"base_resp":{"status_code":1004,"status_msg":"login fail"}}`, errclass.ClassAuth, 401},
		{"plan throttle", 400, `{"error":{"message":"rate limit (2062)"}}`, errclass.ClassRateLimit, 429},
		{"overloaded 529", 529, `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`, errclass.ClassUpstream, 503},
		{"plain 401", 401, `{"type":"error","error":{"type":"authentication_error","message":"invalid api key"}}`, errclass.ClassAuth, 401},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newMiniMaxManager(t, "api-keys:\n  - value: "+testKey+"\n", func(hostHTTPReq) pluginapi.HTTPResponse {
				return pluginapi.HTTPResponse{StatusCode: tc.status, Body: []byte(tc.body)}
			})
			env := executeAs(t, m, map[string]string{"api_key": testKey}, "minimax/MiniMax-M3", "claude", claudeRequestBody, false)
			if env.OK || env.Error == nil || env.Error.Code != string(tc.wantClass) || env.Error.HTTPStatus != tc.wantStatus || !env.Error.Retryable {
				t.Fatalf("non-stream envelope = %+v", env.Error)
			}

			f := &fakeCaller{}
			f.responder = wrapWithCatalog(minimaxCatalog, streamResponder(streamScript{startStatus: tc.status, upstreamID: "up-err", frames: []string{tc.body}}))
			sm := NewManager(NewHostBridge(f.call))
			t.Cleanup(func() { _, _ = sm.HandleCall("plugin.shutdown", nil) })
			mustHandle(t, sm, "plugin.register", lifecycleRequestBody("api-keys:\n  - value: "+testKey+"\n"))
			env = executeAs(t, sm, map[string]string{"api_key": testKey}, "minimax/MiniMax-M3", "claude", claudeRequestBody, true)
			if env.OK || env.Error == nil || env.Error.Code != string(tc.wantClass) || env.Error.HTTPStatus != tc.wantStatus {
				t.Fatalf("stream envelope = %+v", env.Error)
			}
		})
	}
}

func countTokensRequest(model, format, body string) []byte {
	return mustJSON(executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		AuthProvider: ProviderID, AuthAttributes: map[string]string{"api_key": testKey, "region": "cn"},
		Model: model, SourceFormat: format, Format: format, Payload: []byte(body),
	}})
}

func TestCountTokensUsesMessagesCounter(t *testing.T) {
	m, f := newMiniMaxManager(t, "api-keys:\n  - value: "+testKey+"\n    region: cn\n    base-url: https://counter-key.test/v1\n", okJSON(`{"input_tokens":42,"extra":true}`))
	body := `{"model":"x","max_tokens":64,"stream":true,"temperature":1,"system":"sys","messages":[{"role":"user","content":"hi"}],"tools":[{"name":"f","input_schema":{"type":"object"}}]}`
	env := decodeEnv(t, mustHandle(t, m, pluginabi.MethodExecutorCountTokens, countTokensRequest("minimax/MiniMax-M3", "claude", body)))
	if !env.OK {
		t.Fatalf("count_tokens: %+v", env.Error)
	}
	var out pluginapi.ExecutorResponse
	decodeResult(t, mustJSON(env), &out)
	if string(out.Payload) != `{"input_tokens":42}` {
		t.Fatalf("payload = %s", out.Payload)
	}
	wire := lastHTTPRequest(t, f, pluginabi.MethodHostHTTPDo)
	if wire.Method != http.MethodPost || wire.URL != "https://counter-key.test/anthropic/v1/messages/count_tokens" ||
		wire.Headers.Get("Authorization") != "Bearer "+testKey || wire.Headers.Get("Anthropic-Version") == "" {
		t.Fatalf("request = %s %s %v", wire.Method, wire.URL, wire.Headers)
	}
	var sent map[string]any
	if err := json.Unmarshal(wire.Body, &sent); err != nil {
		t.Fatal(err)
	}
	for field := range sent {
		switch field {
		case "model", "messages", "system", "tools":
		default:
			t.Fatalf("unexpected counter field %q in %s", field, wire.Body)
		}
	}
	if sent["model"] != "MiniMax-M3" || sent["system"] == nil || sent["tools"] == nil {
		t.Fatalf("counter body = %s", wire.Body)
	}
}

func TestCountTokensRejectsUnsupportedRequests(t *testing.T) {
	m, f := newMiniMaxManager(t, "api-keys:\n  - value: "+testKey+"\n"+testModelsYAML, okJSON(`{"input_tokens":1}`))
	before := len(f.callsOf(pluginabi.MethodHostHTTPDo))
	for _, req := range [][]byte{
		countTokensRequest("minimax/MiniMax-M3", "openai", ccRequestBody),
		countTokensRequest("minimax/chat-model", "claude", claudeRequestBody),
	} {
		env := decodeEnv(t, mustHandle(t, m, pluginabi.MethodExecutorCountTokens, req))
		if env.OK || env.Error == nil || env.Error.Code != string(errclass.ClassUnsupported) {
			t.Fatalf("envelope = %+v", env.Error)
		}
	}
	if env := decodeEnv(t, mustHandle(t, m, pluginabi.MethodExecutorCountTokens, countTokensRequest("minimax/nope", "claude", claudeRequestBody))); env.OK || env.Error.Code != string(errclass.ClassInvalidModel) {
		t.Fatalf("unknown model envelope = %+v", env.Error)
	}
	if got := len(f.callsOf(pluginabi.MethodHostHTTPDo)); got != before {
		t.Fatalf("unsupported count_tokens reached upstream: %d calls", got-before)
	}
}

func TestCountTokensUpstreamFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		resp  pluginapi.HTTPResponse
		class errclass.Class
	}{
		{"status", pluginapi.HTTPResponse{StatusCode: 429, Body: []byte(`{"error":{"message":"usage limit exceeded (2056)"}}`)}, errclass.ClassQuota},
		{"base_resp", pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"base_resp":{"status_code":1004,"status_msg":"login fail"}}`)}, errclass.ClassAuth},
		{"missing count", pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{}`)}, errclass.ClassUpstream},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newMiniMaxManager(t, "api-keys:\n  - value: "+testKey+"\n", func(hostHTTPReq) pluginapi.HTTPResponse { return tc.resp })
			env := decodeEnv(t, mustHandle(t, m, pluginabi.MethodExecutorCountTokens, countTokensRequest("minimax/MiniMax-M3", "claude", claudeRequestBody)))
			if env.OK || env.Error == nil || env.Error.Code != string(tc.class) {
				t.Fatalf("envelope = %+v", env.Error)
			}
		})
	}
}

func TestExecuteStreamJSONErrorUnderHTTP200(t *testing.T) {
	cases := []struct {
		name       string
		frames     []string
		wantClass  errclass.Class
		wantStatus int
	}{
		{"plan usage", []string{`{"base_resp":{"status_code":2056,"status_msg":"usage limit exceeded"}}`}, errclass.ClassQuota, 429},
		{"region mismatch", []string{`{"base_resp":{"status_code":1004,"status_msg":"login fail"}}`}, errclass.ClassAuth, 401},
		{"split across reads", []string{`{"base_resp":{"status_code":1008,`, `"status_msg":"insufficient balance"}}`}, errclass.ClassBilling, 402},
		{"no base_resp", []string{`{"id":"x"}`}, errclass.ClassUpstream, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeCaller{}
			f.responder = wrapWithCatalog(minimaxCatalog, streamResponder(streamScript{upstreamID: "up-json", contentType: "application/json; charset=utf-8", frames: tc.frames}))
			m := NewManager(NewHostBridge(f.call))
			t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
			mustHandle(t, m, "plugin.register", lifecycleRequestBody("api-keys:\n  - value: "+testKey+"\n"))
			env := executeAs(t, m, map[string]string{"api_key": testKey}, "minimax/MiniMax-M3", "claude", claudeRequestBody, true)
			if env.OK || env.Error == nil || env.Error.Code != string(tc.wantClass) || env.Error.HTTPStatus != tc.wantStatus || !env.Error.Retryable {
				t.Fatalf("stream envelope = %+v", env.Error)
			}
			if len(f.callsOf(pluginabi.MethodHostHTTPStreamClose)) == 0 {
				t.Fatal("upstream stream was not closed")
			}
			if len(f.callsOf(pluginabi.MethodHostStreamEmit)) != 0 {
				t.Fatal("error body was emitted downstream")
			}
		})
	}
}

func TestReconfigureUsesCurrentKeyBaseURL(t *testing.T) {
	m, f := newMiniMaxManager(t, "api-keys:\n  - value: selected-key\n    base-url: https://old-key.test\n", okJSON(claudeResponseBody))
	attrs := map[string]string{"api_key": "selected-key", "base_url": "https://old-key.test"}
	for _, base := range []string{"https://new-key.test", ""} {
		yaml := "base-url: https://shared.test\napi-keys:\n  - value: selected-key\n"
		if base != "" {
			yaml += "    base-url: " + base + "\n"
		}
		if env := decodeEnv(t, mustHandle(t, m, "plugin.reconfigure", lifecycleRequestBody(yaml))); !env.OK {
			t.Fatalf("reconfigure: %+v", env.Error)
		}
		if env := executeAs(t, m, attrs, "minimax/MiniMax-M3", "claude", claudeRequestBody, false); !env.OK {
			t.Fatalf("execute: %+v", env.Error)
		}
		want := base
		if want == "" {
			want = "https://shared.test"
		}
		if got := lastHTTPRequest(t, f, pluginabi.MethodHostHTTPDo).URL; got != want+"/anthropic/v1/messages" {
			t.Fatalf("request = %s", got)
		}
	}
}
