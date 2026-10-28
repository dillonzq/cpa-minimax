package plugin

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/dillonzq/cpa-minimax/internal/catalog"
)

const geminiRequestBody = `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`

func TestSelectRoutePrefersNativeThenLeastLoss(t *testing.T) {
	record := func(routes ...catalog.Route) catalog.ModelRecord {
		rec := catalog.ModelRecord{}
		for _, r := range routes {
			rec.Routes = append(rec.Routes, catalog.Target{Protocol: r, EndpointPath: r.EndpointPath()})
		}
		return rec
	}
	chat, messages, responses := catalog.RouteChatCompletions, catalog.RouteMessages, catalog.RouteResponses
	minimax := record(messages, chat, responses)
	cases := []struct {
		rec    catalog.ModelRecord
		format string
		want   catalog.Route
	}{
		// Same protocol first.
		{minimax, "openai", chat},
		{minimax, "openai-response", responses},
		{minimax, "claude", messages},
		// Gemini has no MiniMax endpoint and prefers the recommended Messages.
		{minimax, "gemini", messages},
		{record(chat, responses), "gemini", responses},
		{record(chat), "gemini", chat},
		// Otherwise the static least-loss order for the client format.
		{record(messages, responses), "openai", responses},
		{record(messages), "openai", messages},
		{record(messages, chat), "openai-response", chat},
		{record(chat, responses), "claude", responses},
		{record(chat), "claude", chat},
		// Unknown formats keep the primary route; conversion reports them.
		{record(messages, chat), "codex", messages},
	}
	for _, c := range cases {
		if got := selectRoute(c.rec, c.format).Protocol; got != c.want {
			t.Errorf("selectRoute(%v, %s) = %s, want %s", c.rec.Routes, c.format, got, c.want)
		}
	}
	own := map[string]catalog.Route{"openai": chat, "openai-response": responses, "claude": messages, "gemini": messages}
	for format, order := range routePreference {
		if len(order) != 3 || order[0] != own[format] {
			t.Errorf("%s preference %v must list every protocol, the native or recommended one first", format, order)
		}
	}
}

// MiniMax models serve Chat, Responses, and Messages, so every client
// format reaches its own upstream protocol without conversion.
func TestExecuteMiniMaxServesEachSourceNatively(t *testing.T) {
	cases := []struct {
		format, body, path, response, marker string
	}{
		{"openai", ccRequestBody, "/v1/chat/completions", ccResponseBody, `"finish_reason":"stop"`},
		{"claude", claudeRequestBody, "/anthropic/v1/messages", claudeResponseBody, `"stop_reason":"end_turn"`},
		{"openai-response", `{"model":"x","input":"hi"}`, "/v1/responses", responsesPassthrough, `"object":"response"`},
	}
	for _, c := range cases {
		t.Run(c.format, func(t *testing.T) {
			f := &fakeCaller{}
			m := NewManager(NewHostBridge(f.call))
			t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
			f.responder = wrapWithCatalog(`{"object":"list","data":[{"id":"MiniMax-M3","object":"model"}]}`, upstreamRouter(t, map[string]string{c.path: c.response}))
			if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
				t.Fatalf("register: %v", err)
			}
			env := mustExecute(t, m, "minimax/MiniMax-M3", c.format, []byte(c.body))
			if !env.OK {
				t.Fatalf("execute: %+v", env.Error)
			}
			var out pluginapi.ExecutorResponse
			if err := json.Unmarshal(env.Result, &out); err != nil || !strings.Contains(string(out.Payload), c.marker) {
				t.Fatalf("payload = %s (%v), want native %s", out.Payload, err, c.marker)
			}
			wire := lastWire(t, f, pluginabi.MethodHostHTTPDo)
			if wire["url"] != "https://api.minimax.io"+c.path {
				t.Fatalf("url = %v, want native %s", wire["url"], c.path)
			}
			assertField(t, upstreamRequestBody(t, f, pluginabi.MethodHostHTTPDo), "model", "MiniMax-M3")
		})
	}
}

func newGeminiManager(t *testing.T, catalogBody string, next func(string, []byte) ([]byte, error)) (*Manager, *fakeCaller) {
	t.Helper()
	f := &fakeCaller{responder: wrapWithCatalog(catalogBody, next)}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	return m, f
}

func TestExecuteGeminiClient(t *testing.T) {
	t.Run("non-stream to a MiniMax model uses Messages", func(t *testing.T) {
		m, f := newGeminiManager(t, `{"data":[{"id":"MiniMax-M3"}]}`, upstreamRouter(t, map[string]string{"/anthropic/v1/messages": claudeResponseBody}))
		env := mustExecute(t, m, "minimax/MiniMax-M3", "gemini", []byte(geminiRequestBody))
		if !env.OK {
			t.Fatalf("execute: %+v", env.Error)
		}
		var out pluginapi.ExecutorResponse
		if err := json.Unmarshal(env.Result, &out); err != nil || !strings.Contains(string(out.Payload), `"candidates"`) || !strings.Contains(string(out.Payload), "hello") {
			t.Fatalf("payload not converted to Gemini: %s (%v)", out.Payload, err)
		}
		wire := lastWire(t, f, pluginabi.MethodHostHTTPDo)
		if wire["url"] != "https://api.minimax.io/anthropic/v1/messages" {
			t.Fatalf("url = %v", wire["url"])
		}
		assertUpstreamHeaders(t, wire, true)
		assertField(t, upstreamRequestBody(t, f, pluginabi.MethodHostHTTPDo), "model", "MiniMax-M3")
	})
	t.Run("stream to a Chat model", func(t *testing.T) {
		m, f := newGeminiManager(t, `{"data":[{"id":"chat-model"}]}`, streamResponder(streamScript{upstreamID: "up-chat", frames: chatSSEFrames}))
		raw, err := m.HandleCall(pluginabi.MethodExecutorExecuteStream, execStreamReqBody("minimax/chat-model", "gemini", []byte(geminiRequestBody), "down-chat"))
		if err != nil || !decodeEnv(t, raw).OK {
			t.Fatalf("execute_stream: %v %s", err, raw)
		}
		if !m.bridge.WaitForInFlight(5 * time.Second) {
			t.Fatal("stream did not drain")
		}
		assertCleanStreamClose(t, f)
		wire := lastWire(t, f, pluginabi.MethodHostHTTPDoStream)
		if wire["url"] != "https://api.minimax.io/v1/chat/completions" {
			t.Fatalf("url = %v", wire["url"])
		}
		body := upstreamRequestBody(t, f, pluginabi.MethodHostHTTPDoStream)
		assertField(t, body, "stream", true)
		assertField(t, body, "model", "chat-model")
		events := emittedEvents(t, f)
		if len(events) == 0 {
			t.Fatal("no Gemini events emitted")
		}
		for _, event := range events {
			// CPA frames Gemini SSE itself: each event is one JSON object.
			var chunk map[string]any
			if err := json.Unmarshal([]byte(event), &chunk); err != nil || chunk["candidates"] == nil {
				t.Fatalf("event %q is not a bare Gemini chunk: %v", event, err)
			}
		}
		if joined := strings.Join(events, "\n"); !strings.Contains(joined, `"text":"Hel"`) || !strings.Contains(joined, `"finishReason":"STOP"`) {
			t.Fatalf("Gemini events = %s", joined)
		}
	})
	t.Run("execute_stream without the Stream flag", func(t *testing.T) {
		m, f := newGeminiManager(t, `{"data":[{"id":"chat-model"}]}`, streamResponder(streamScript{upstreamID: "up-chat", frames: chatSSEFrames}))
		var req executorRequest
		if err := json.Unmarshal(execStreamReqBody("minimax/chat-model", "gemini", []byte(geminiRequestBody), "down-chat"), &req); err != nil {
			t.Fatal(err)
		}
		req.Stream = false
		call, _ := json.Marshal(req)
		raw, err := m.HandleCall(pluginabi.MethodExecutorExecuteStream, call)
		if err != nil || !decodeEnv(t, raw).OK {
			t.Fatalf("execute_stream: %v %s", err, raw)
		}
		if !m.bridge.WaitForInFlight(5 * time.Second) {
			t.Fatal("stream did not drain")
		}
		assertCleanStreamClose(t, f)
		assertField(t, upstreamRequestBody(t, f, pluginabi.MethodHostHTTPDoStream), "stream", true)
	})
	t.Run("non-stream to a Chat model", func(t *testing.T) {
		m, f := newGeminiManager(t, `{"data":[{"id":"chat-model"}]}`, upstreamRouter(t, map[string]string{"/v1/chat/completions": ccResponseBody}))
		env := mustExecute(t, m, "minimax/chat-model", "gemini", []byte(geminiRequestBody))
		if !env.OK {
			t.Fatalf("execute: %+v", env.Error)
		}
		var out pluginapi.ExecutorResponse
		if err := json.Unmarshal(env.Result, &out); err != nil || !strings.Contains(string(out.Payload), `"candidates"`) || !strings.Contains(string(out.Payload), "hello") {
			t.Fatalf("payload not converted to Gemini: %s (%v)", out.Payload, err)
		}
		if stream, ok := upstreamRequestBody(t, f, pluginabi.MethodHostHTTPDo)["stream"]; ok && stream != false {
			t.Fatalf("non-stream request sent stream = %v", stream)
		}
	})
}

func TestUpstreamAuthHeadersPerProtocol(t *testing.T) {
	for _, route := range []catalog.Route{catalog.RouteChatCompletions, catalog.RouteResponses, catalog.RouteMessages} {
		got := upstreamAuthHeaders(route, "k")
		if got.Get("Authorization") != "Bearer k" || got.Get("Content-Type") != "application/json" || got.Get("X-Api-Key") != "" {
			t.Errorf("%s headers = %v", route, got)
		}
		if want := map[bool]string{true: "2023-06-01"}[route == catalog.RouteMessages]; got.Get("Anthropic-Version") != want {
			t.Errorf("%s anthropic-version = %q", route, got.Get("Anthropic-Version"))
		}
	}
}
