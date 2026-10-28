package plugin

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/dillonzq/apix/reasoning"
	"github.com/dillonzq/cpa-minimax/internal/catalog"
	"github.com/dillonzq/cpa-minimax/internal/errclass"
)

// upstreamRequestBody decodes the JSON body of the last bridged upstream call.
func upstreamRequestBody(t *testing.T, f *fakeCaller, method string) map[string]any {
	t.Helper()
	calls := f.callsOf(method)
	if len(calls) == 0 {
		t.Fatalf("no %s calls recorded", method)
	}
	wire := decodePayload(t, calls[len(calls)-1])
	var body map[string]any
	if err := json.Unmarshal(wireBody(t, wire, "body"), &body); err != nil {
		t.Fatalf("upstream body decode: %v", err)
	}
	return body
}

func assertField(t *testing.T, body map[string]any, key string, want any) {
	t.Helper()
	got, ok := body[key]
	if want == nil {
		if ok {
			t.Fatalf("%s = %v, want absent; body=%v", key, got, body)
		}
		return
	}
	if !ok || got != want {
		t.Fatalf("%s = %v (present=%t), want %v; body=%v", key, got, ok, want, body)
	}
}

// testSuffix parses a model name the way a client would write it, so the cases
// below exercise the same parser the executor uses.
func testSuffix(model string) reasoning.Suffix {
	return reasoning.ParseSuffix(model)
}

func TestExecuteChatRouteThinkingSuffix(t *testing.T) {
	// CPA parses model(value) for its own executors only, so the plugin must
	// both strip the suffix for routing AND apply the reasoning control
	// itself, with the suffix winning over the body's own control.
	for _, tc := range []struct {
		name  string
		model string
		body  string
		want  any // nil means no reasoning_effort in the upstream body
	}{
		{"level overrides body control", "minimax/chat-model(high)",
			`{"model":"chat-model(high)","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`, "high"},
		{"level is case-insensitive", "minimax/chat-model(HIGH)",
			`{"model":"chat-model(HIGH)","messages":[{"role":"user","content":"hi"}]}`, "high"},
		{"bare upstream id", "chat-model(xhigh)",
			`{"model":"chat-model(xhigh)","messages":[{"role":"user","content":"hi"}]}`, "xhigh"},
		{"numeric budget uses the fixed thresholds", "minimax/chat-model(1024)",
			`{"model":"chat-model(1024)","messages":[{"role":"user","content":"hi"}]}`, "low"},
		{"none disables reasoning", "minimax/chat-model(none)",
			`{"model":"chat-model(none)","messages":[{"role":"user","content":"hi"}]}`, "none"},
		{"zero budget disables reasoning", "minimax/chat-model(0)",
			`{"model":"chat-model(0)","messages":[{"role":"user","content":"hi"}]}`, "none"},
		{"auto removes the body control", "minimax/chat-model(auto)",
			`{"model":"chat-model(auto)","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`, nil},
		{"minus one is auto", "minimax/chat-model(-1)",
			`{"model":"chat-model(-1)","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`, nil},
		{"unrecognized value strips and keeps the body control", "minimax/chat-model(turbo)",
			`{"model":"chat-model(turbo)","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`, "low"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, f := newExecManager(t)
			env := mustExecute(t, m, tc.model, "openai", []byte(tc.body))
			if !env.OK {
				t.Fatalf("envelope = %+v", env.Error)
			}
			body := upstreamRequestBody(t, f, pluginabi.MethodHostHTTPDo)
			assertField(t, body, "model", "chat-model")
			assertField(t, body, "reasoning_effort", tc.want)
		})
	}
}

func TestExecuteMessagesRouteThinkingSuffix(t *testing.T) {
	for _, tc := range []struct {
		name         string
		model        string
		format       string
		body         string
		wantThinking map[string]any // nil means absent
		wantEffort   any            // output_config.effort; nil means absent
	}{
		{
			// The claude source carries both an explicit budget and an
			// adaptive effort: the suffix must replace both, and the
			// display preference survives the adaptive override.
			name: "level replaces budget and effort", model: "minimax/messages-alt(high)", format: "claude",
			body: `{"model":"messages-alt(high)","max_tokens":65536,"messages":[{"role":"user","content":"hi"}],` +
				`"thinking":{"type":"enabled","budget_tokens":3000,"display":"omitted"},"output_config":{"effort":"low"}}`,
			wantThinking: map[string]any{"type": "adaptive", "display": "omitted"},
			wantEffort:   "high",
		},
		{
			// Adaptive thinking has no budget, so a numeric suffix maps to
			// the level its budget falls in.
			name: "numeric suffix maps to its level", model: "minimax/messages-alt(16384)", format: "claude",
			body:         `{"model":"messages-alt(16384)","max_tokens":65536,"messages":[{"role":"user","content":"hi"}]}`,
			wantThinking: map[string]any{"type": "adaptive"},
			wantEffort:   "high",
		},
		{
			// CPA drops display when thinking is disabled: display only
			// applies to an active block.
			name: "none disables thinking and drops display", model: "minimax/messages-alt(none)", format: "claude",
			body: `{"model":"messages-alt(none)","max_tokens":4096,"messages":[{"role":"user","content":"hi"}],` +
				`"thinking":{"type":"adaptive","display":"summarized"},"output_config":{"effort":"high"}}`,
			wantThinking: map[string]any{"type": "disabled"},
		},
		{
			name: "auto removes the control", model: "minimax/messages-alt(auto)", format: "claude",
			body: `{"model":"messages-alt(auto)","max_tokens":4096,"messages":[{"role":"user","content":"hi"}],` +
				`"thinking":{"type":"enabled","budget_tokens":3000}}`,
		},
		{
			name: "unrecognized value leaves the control", model: "minimax/messages-alt(turbo)", format: "claude",
			body: `{"model":"messages-alt(turbo)","max_tokens":4096,"messages":[{"role":"user","content":"hi"}],` +
				`"thinking":{"type":"enabled","budget_tokens":3000}}`,
			wantThinking: map[string]any{"type": "enabled", "budget_tokens": float64(3000)},
		},
		{
			name: "cross-protocol level becomes adaptive effort", model: "minimax/messages-alt(high)", format: "openai",
			body:         `{"model":"messages-alt(high)","max_tokens":65536,"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`,
			wantThinking: map[string]any{"type": "adaptive"},
			wantEffort:   "high",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, f := newExecManager(t)
			env := mustExecute(t, m, tc.model, tc.format, []byte(tc.body))
			if !env.OK {
				t.Fatalf("envelope = %+v", env.Error)
			}
			body := upstreamRequestBody(t, f, pluginabi.MethodHostHTTPDo)
			assertField(t, body, "model", "messages-alt")
			got, _ := body["thinking"].(map[string]any)
			if tc.wantThinking == nil {
				assertField(t, body, "thinking", nil)
			} else if !reflect.DeepEqual(got, tc.wantThinking) {
				t.Fatalf("thinking = %v, want %v", got, tc.wantThinking)
			}
			if got := dig(t, body, "output_config.effort"); got != tc.wantEffort {
				t.Fatalf("output_config.effort = %v, want %v", got, tc.wantEffort)
			}
		})
	}
}

func TestExecuteResponsesRouteThinkingSuffix(t *testing.T) {
	for _, tc := range []struct {
		name        string
		model       string
		format      string
		body        string
		want        any // nil means no reasoning.effort in the upstream body
		wantSummary any
	}{
		{
			name: "native responses keeps sibling reasoning fields", model: "minimax/responses-model(medium)", format: "openai-response",
			body: `{"model":"responses-model(medium)","input":"hi","reasoning":{"summary":"auto","effort":"low"}}`,
			want: "medium", wantSummary: "auto",
		},
		{
			name: "auto removes only the effort", model: "minimax/responses-model(auto)", format: "openai-response",
			body: `{"model":"responses-model(auto)","input":"hi","reasoning":{"effort":"low"}}`,
		},
		{
			name: "cross-protocol none overrides the body", model: "minimax/responses-model(none)", format: "openai",
			body: `{"model":"responses-model(none)","input":"hi","reasoning_effort":"low"}`,
			want: "none",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, f := newExecManager(t)
			env := mustExecute(t, m, tc.model, tc.format, []byte(tc.body))
			if !env.OK {
				t.Fatalf("envelope = %+v", env.Error)
			}
			body := upstreamRequestBody(t, f, pluginabi.MethodHostHTTPDo)
			assertField(t, body, "model", "responses-model")
			reasoning, _ := body["reasoning"].(map[string]any)
			if tc.want == nil {
				if effort, has := reasoning["effort"]; has {
					t.Fatalf("reasoning.effort survived the suffix override: %v", effort)
				}
				return
			}
			if reasoning["effort"] != tc.want {
				t.Fatalf("reasoning.effort = %v, want %v; body=%v", reasoning["effort"], tc.want, body)
			}
			if tc.wantSummary != nil && reasoning["summary"] != tc.wantSummary {
				t.Fatalf("reasoning.summary = %v, want %v", reasoning["summary"], tc.wantSummary)
			}
		})
	}
}

// TestExecuteThinkingSuffixSupersedesBodyControl covers the conversion that
// reads the client's reasoning control before the override runs: an
// unrepresentable value must not fail a request whose suffix already replaced
// it, and the same request without a suffix must still fail.
func TestExecuteThinkingSuffixSupersedesBodyControl(t *testing.T) {
	const messagesRoute = "minimax/messages-alt"
	const chatRoute = "minimax/chat-model"
	const responsesRoute = "minimax/responses-model"
	for _, tc := range []struct {
		name      string
		model     string
		upstream  string
		format    string
		body      string
		errorOnly bool   // without the suffix the request must fail
		wantPath  string // dotted path of the overridden control
		want      any
	}{
		{
			name: "openai effort with no budget equivalent", model: messagesRoute, upstream: "messages-alt", format: "openai",
			body:      `{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"turbo"}`,
			errorOnly: true, wantPath: "output_config.effort", want: "high",
		},
		{
			name: "responses effort with no budget equivalent", model: messagesRoute, upstream: "messages-alt", format: "openai-response",
			body:      `{"model":"m","input":"hi","reasoning":{"effort":"turbo","summary":"auto"}}`,
			errorOnly: true, wantPath: "output_config.effort", want: "medium",
		},
		{
			name: "claude thinking type with no chat equivalent", model: chatRoute, upstream: "chat-model", format: "claude",
			body:      `{"model":"m","max_tokens":4096,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"bogus"}}`,
			errorOnly: true, wantPath: "reasoning_effort", want: "low",
		},
		{
			name: "claude thinking type with no responses equivalent", model: responsesRoute, upstream: "responses-model", format: "claude",
			body:      `{"model":"m","max_tokens":4096,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"bogus"}}`,
			errorOnly: true, wantPath: "reasoning.effort", want: "low",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, f := newExecManager(t)
			suffixed := tc.model + "(high)"
			if tc.format == "openai-response" {
				suffixed = tc.model + "(medium)"
			}
			if tc.format == "claude" {
				suffixed = tc.model + "(low)"
			}
			env := mustExecute(t, m, suffixed, tc.format, []byte(tc.body))
			if !env.OK {
				t.Fatalf("suffix must supersede the body control, envelope = %+v", env.Error)
			}
			body := upstreamRequestBody(t, f, pluginabi.MethodHostHTTPDo)
			assertField(t, body, "model", tc.upstream)
			if got := dig(t, body, tc.wantPath); got != tc.want {
				t.Fatalf("%s = %v, want %v; body=%v", tc.wantPath, got, tc.want, body)
			}
			if tc.errorOnly {
				// Baseline: the same body without a suffix keeps failing
				// instead of silently dropping the client's control.
				base, _ := newExecManager(t)
				failed := mustExecute(t, base, tc.model, tc.format, []byte(tc.body))
				if failed.OK || failed.Error == nil || failed.Error.Code != string(errclass.ClassUnsupported) {
					t.Fatalf("baseline without suffix = %+v", failed.Error)
				}
			}
		})
	}
}

// dig resolves a dotted JSON path in a decoded object.
func dig(t *testing.T, body map[string]any, path string) any {
	t.Helper()
	node := any(body)
	for _, key := range strings.Split(path, ".") {
		obj, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		node = obj[key]
	}
	return node
}

func TestExecuteThinkingSuffixWithPrefixDisabled(t *testing.T) {
	// With the provider prefix off the client sends the bare upstream ID plus
	// the suffix; routing and the override must work identically.
	f := &fakeCaller{responder: wrapWithCatalog(multiRouteCatalog, upstreamRouter(t, map[string]string{
		"/v1/chat/completions": ccResponseBody,
	}))}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	if resp, err := m.HandleCall("plugin.reconfigure",
		lifecycleRequestBody(testValidYAML+"model-prefix:\n  enabled: false\n")); err != nil {
		t.Fatalf("reconfigure: %v", err)
	} else if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("reconfigure envelope = %+v", env.Error)
	}
	env := mustExecute(t, m, "chat-model(high)", "openai", []byte(`{"model":"chat-model(high)","messages":[{"role":"user","content":"hi"}]}`))
	if !env.OK {
		t.Fatalf("envelope = %+v", env.Error)
	}
	body := upstreamRequestBody(t, f, pluginabi.MethodHostHTTPDo)
	assertField(t, body, "model", "chat-model")
	assertField(t, body, "reasoning_effort", "high")
}

func TestExecuteThinkingSuffixLiteralIDCollidesWithBaseModel(t *testing.T) {
	// A real collision: both the base model and a literal parenthesized ID are
	// routable. CPA matches the base model first, so the stripped name wins and
	// the recognized suffix is applied instead of reaching the literal ID.
	const catalogBody = `{"data":[{"id":"chat-model"},{"id":"chat-model(low)"},{"id":"chat-model(kid)"}]}`
	f := &fakeCaller{responder: wrapWithCatalog(catalogBody, upstreamRouter(t, map[string]string{
		"/v1/chat/completions": ccResponseBody,
	}))}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}

	// A recognized value resolves the base model and overrides the control.
	env := mustExecute(t, m, "minimax/chat-model(low)", "openai", []byte(ccRequestBody))
	if !env.OK {
		t.Fatalf("envelope = %+v", env.Error)
	}
	body := upstreamRequestBody(t, f, pluginabi.MethodHostHTTPDo)
	assertField(t, body, "model", "chat-model")
	assertField(t, body, "reasoning_effort", "low")

	// An unrecognized value resolves the base model too and is stripped, while
	// the body keeps its own control: the literal ID never wins.
	env = mustExecute(t, m, "minimax/chat-model(kid)", "openai",
		[]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"medium"}`))
	if !env.OK {
		t.Fatalf("envelope = %+v", env.Error)
	}
	body = upstreamRequestBody(t, f, pluginabi.MethodHostHTTPDo)
	assertField(t, body, "model", "chat-model")
	assertField(t, body, "reasoning_effort", "medium")
}

func TestExecuteThinkingSuffixKeepsLiteralCatalogID(t *testing.T) {
	// The stripped name does not resolve here, so the literal parenthesized
	// catalog ID stays reachable: it keeps its identity and no override. This
	// is a plugin lookup-layer fallback only; CPA resolves the requested model
	// against its auth registry before the plugin sees it.
	const catalogBody = `{"data":[{"id":"MiniMax-M2.7(kid)"}]}`
	f := &fakeCaller{responder: wrapWithCatalog(catalogBody, upstreamRouter(t, map[string]string{
		"/v1/chat/completions": ccResponseBody,
	}))}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	env := mustExecute(t, m, "minimax/MiniMax-M2.7(kid)", "openai", []byte(ccRequestBody))
	if !env.OK {
		t.Fatalf("envelope = %+v", env.Error)
	}
	body := upstreamRequestBody(t, f, pluginabi.MethodHostHTTPDo)
	assertField(t, body, "model", "MiniMax-M2.7(kid)")
	assertField(t, body, "reasoning_effort", nil)
}

func TestExecuteThinkingSuffixUsesInterceptorPayload(t *testing.T) {
	// The suffix is read from the requested model while the body comes from the
	// host's effective Payload, so an interceptor rewrite of the body's model
	// field does not change the routing or the override.
	m, f := newExecManager(t)
	req, _ := json.Marshal(executorRequest{ExecutorRequest: pluginapi.ExecutorRequest{
		AuthProvider: ProviderID, AuthAttributes: map[string]string{"api_key": testKey},
		Model: "minimax/chat-model(high)", SourceFormat: "openai", Format: "openai",
		OriginalRequest: []byte(`{"model":"chat-model","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`),
		Payload:         []byte(`{"model":"chat-model(high)","messages":[{"role":"user","content":"rewritten"}],"reasoning_effort":"low"}`),
	}})
	resp, err := m.HandleCall("executor.execute", req)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("envelope = %+v", env.Error)
	}
	body := upstreamRequestBody(t, f, pluginabi.MethodHostHTTPDo)
	assertField(t, body, "model", "chat-model")
	assertField(t, body, "reasoning_effort", "high")
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("payload messages = %v", body["messages"])
	}
	if content := msgs[0].(map[string]any)["content"]; content != "rewritten" {
		t.Fatalf("effective payload not used: %v", content)
	}
}

func TestExecuteStreamThinkingSuffix(t *testing.T) {
	chatFrames := []string{
		`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}
	// Each route gets its own protocol's terminal events, so a completed stream
	// is asserted: a suffix that broke the conversion would fail the clean close.
	messagesFrames := []string{
		"event: message_start\n" + `data: {"type":"message_start","message":{"id":"m1","role":"assistant","content":[]}}` + "\n\n",
		"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n",
	}
	responsesFrames := []string{
		"event: response.created\n" + `data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n",
		"event: response.completed\n" + `data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","output":[]}}` + "\n\n",
	}
	for _, tc := range []struct {
		name   string
		model  string
		format string
		body   string
		path   string
		want   any
		frames []string
	}{
		{
			name: "chat route", model: "minimax/chat-model(high)", format: "openai",
			body: `{"model":"chat-model(high)","stream":true,"messages":[{"role":"user","content":"hi"}],"reasoning_effort":"low"}`,
			path: "reasoning_effort", want: "high", frames: chatFrames,
		},
		{
			name: "messages route", model: "minimax/messages-alt(high)", format: "claude",
			body: `{"model":"messages-alt(high)","stream":true,"max_tokens":65536,"messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled","budget_tokens":1000}}`,
			path: "output_config.effort", want: "high", frames: messagesFrames,
		},
		{
			name: "responses route", model: "minimax/responses-model(none)", format: "openai-response",
			body: `{"model":"responses-model(none)","stream":true,"input":"hi","reasoning":{"effort":"low"}}`,
			path: "reasoning.effort", want: "none", frames: responsesFrames,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, f := newStreamManager(t, streamScript{upstreamID: "up-suffix", frames: tc.frames})
			resp, err := m.HandleCall("executor.execute_stream",
				execStreamReqBody(tc.model, tc.format, []byte(tc.body), "down-suffix"))
			if err != nil {
				t.Fatalf("execute_stream: %v", err)
			}
			env := decodeEnv(t, resp)
			if !env.OK {
				t.Fatalf("envelope = %+v", env.Error)
			}
			m.bridge.WaitForInFlight(5 * time.Second)
			body := upstreamRequestBody(t, f, pluginabi.MethodHostHTTPDoStream)
			if got := dig(t, body, tc.path); got != tc.want {
				t.Fatalf("%s = %v, want %v; body=%v", tc.path, got, tc.want, body)
			}
			// The suffix must not derail the stream: both sides close once
			// without an error after the protocol's terminal event.
			assertCleanStreamClose(t, f)
		})
	}
}

func TestApplyReasoningSuffixSeamBranches(t *testing.T) {
	// Without a recognized suffix the body stays byte-identical: no suffix must
	// not rewrite a request at all.
	body := []byte(`{"model":"m","reasoning_effort":"low"}`)
	if got, eErr := applyReasoningSuffix(catalog.RouteChatCompletions, body, testSuffix("m")); eErr != nil || string(got) != string(body) {
		t.Fatalf("no suffix = %q, %v", got, eErr)
	}
	if got, eErr := applyReasoningSuffix(catalog.RouteChatCompletions, body, testSuffix("m(turbo)")); eErr != nil || string(got) != string(body) {
		t.Fatalf("unrecognized suffix = %q, %v", got, eErr)
	}
	// An unknown route has no reasoning control to override.
	if got, eErr := applyReasoningSuffix(catalog.Route("weird"), body, testSuffix("m(high)")); eErr != nil || string(got) != string(body) {
		t.Fatalf("unknown route = %q, %v", got, eErr)
	}
	// Defensive parse failures are classified translation errors, never a send.
	for _, in := range [][]byte{[]byte("{bad"), []byte("null")} {
		if _, eErr := applyReasoningSuffix(catalog.RouteChatCompletions, in, testSuffix("m(high)")); eErr == nil || eErr.Class != errclass.ClassTranslation {
			t.Fatalf("malformed body %q = %v", in, eErr)
		}
	}
	// Containers with the wrong JSON shape are replaced or left alone, not fatal.
	if got, eErr := applyReasoningSuffix(catalog.RouteResponses, []byte(`{"reasoning":"auto"}`), testSuffix("m(high)")); eErr != nil || !strings.Contains(string(got), `"effort":"high"`) {
		t.Fatalf("non-object reasoning = %s, %v", got, eErr)
	}
	if got, eErr := applyReasoningSuffix(catalog.RouteMessages, []byte(`{"output_config":"auto"}`), testSuffix("m(none)")); eErr != nil || !strings.Contains(string(got), `"type":"disabled"`) {
		t.Fatalf("non-object output_config = %s, %v", got, eErr)
	}
	// Auto drops the control, including an emptied parent object.
	if got, eErr := applyReasoningSuffix(catalog.RouteResponses, []byte(`{"reasoning":{"effort":"low"}}`), testSuffix("m(auto)")); eErr != nil || strings.Contains(string(got), "reasoning") {
		t.Fatalf("auto reasoning = %s, %v", got, eErr)
	}
	// Replacing the effort drops the budget and keeps every sibling field.
	got, eErr := applyReasoningSuffix(catalog.RouteMessages,
		[]byte(`{"thinking":{"type":"enabled","budget_tokens":1},"output_config":{"effort":"low","other":1}}`), testSuffix("m(high)"))
	if eErr != nil || strings.Contains(string(got), "budget_tokens") || !strings.Contains(string(got), `"effort":"high"`) || !strings.Contains(string(got), `"other"`) {
		t.Fatalf("messages override = %s, %v", got, eErr)
	}
	// A superseded control is dropped only for the conversions that read it.
	if got := stripSupersededReasoning(catalog.RouteMessages, "claude", body); string(got) != string(body) {
		t.Fatalf("native messages body rewritten: %s", got)
	}
	if got := stripSupersededReasoning(catalog.RouteChatCompletions, "openai", body); string(got) != string(body) {
		t.Fatalf("native chat body rewritten: %s", got)
	}
	if got := stripSupersededReasoning(catalog.RouteMessages, "openai", body); strings.Contains(string(got), "reasoning_effort") {
		t.Fatalf("superseded chat effort kept: %s", got)
	}
	if got := stripSupersededReasoning(catalog.RouteMessages, "openai", []byte("{bad")); string(got) != "{bad" {
		t.Fatalf("malformed body rewritten: %s", got)
	}
}

func TestExecuteThinkingSuffixUnknownModelIs404(t *testing.T) {
	m, _ := newExecManager(t)
	env := mustExecute(t, m, "minimax/nope(high)", "openai", []byte(ccRequestBody))
	if env.OK || env.Error == nil || env.Error.Code != string(errclass.ClassInvalidModel) || env.Error.HTTPStatus != http.StatusNotFound {
		t.Fatalf("envelope = %+v", env.Error)
	}
}
