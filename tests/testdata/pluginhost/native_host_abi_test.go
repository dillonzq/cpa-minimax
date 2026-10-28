//go:build debug && cgo && (darwin || linux)

package pluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	codexmodels "github.com/router-for-me/CLIProxyAPI/v8/internal/client/codex/models"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const (
	miniMaxPluginID = "cpa-minimax"
	miniMaxProvider = "minimax"
	messagesPath    = "/anthropic/v1/messages"
)

// miniMaxYAML points both regions at the offline upstream. Built-in models are
// published without discovery; extra appends further plugin settings.
func miniMaxYAML(upstream, extra string) []byte {
	return []byte(fmt.Sprintf("api-keys:\n  - value: offline-test-key\nbase-url: %s\nallow-http: true\n%s", upstream, extra))
}

// openMiniMax loads the real C-shared library through the SDK's Unix loader.
func openMiniMax(t *testing.T, authDir string) (*Host, pluginFile, *guardedPluginClient) {
	t.Helper()
	host := New()
	host.runtimeConfig = &config.Config{AuthDir: authDir}
	file := pluginFile{ID: miniMaxPluginID, Path: os.Getenv("CPA_NATIVE_PLUGIN")}
	native, err := defaultPluginLoader().Open(file, host)
	if err != nil {
		t.Fatal(err)
	}
	return host, file, newGuardedPluginClient(native)
}

func miniMaxRequest(model, source, output string, body []byte, stream bool) pluginapi.ExecutorRequest {
	return pluginapi.ExecutorRequest{Model: miniMaxProvider + "/" + model, AuthProvider: miniMaxProvider, AuthAttributes: map[string]string{"api_key": "offline-test-key"}, SourceFormat: source, Format: output, Payload: body, Stream: stream}
}

// Exercise the real C-shared model publication and the host thinking validator,
// then send the host-adjusted body through the plugin. M3 can disable thinking;
// M3.1 Flash cannot, so neither the host nor the plugin may forward none.
func TestMiniMaxNativeThinking(t *testing.T) {
	var mu sync.Mutex
	var lastPath string
	var lastBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		lastPath, lastBody = r.URL.Path, body
		mu.Unlock()
		switch r.URL.Path {
		case messagesPath:
			io.WriteString(w, `{"id":"m1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		case "/v1/chat/completions":
			io.WriteString(w, `{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
		case "/v1/responses":
			io.WriteString(w, `{"id":"r1","object":"response","status":"completed","output":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	host, file, client := openMiniMax(t, t.TempDir())
	defer client.Shutdown()
	plug, err := registerRPCPlugin(context.Background(), host, file.ID, client, pluginabi.MethodPluginRegister, miniMaxYAML(server.URL, "catalog:\n  discover: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	rpc := &rpcPluginAdapter{id: file.ID, host: host, client: client, instance: pluginCallbackInstance(client)}
	for _, forAuth := range []bool{false, true} {
		var response pluginapi.ModelResponse
		if forAuth {
			response, err = plug.Capabilities.ModelProvider.ModelsForAuth(context.Background(), pluginapi.AuthModelRequest{})
		} else {
			response, err = plug.Capabilities.ModelProvider.StaticModels(context.Background(), pluginapi.StaticModelRequest{})
		}
		if err != nil || len(response.Models) != 2 {
			t.Fatalf("MiniMax model publication: %+v, %v", response, err)
		}
		for _, published := range response.Models {
			model := pluginModelInfoToRegistryModelInfo(published)
			flash := strings.Contains(model.ID, "Flash")
			wantLevels := []string{"none", "low", "medium", "high", "xhigh", "max"}
			if flash {
				wantLevels = wantLevels[1:]
			}
			if model.Thinking == nil || model.Thinking.ZeroAllowed == flash || !reflect.DeepEqual(model.Thinking.Levels, wantLevels) {
				t.Fatalf("host thinking metadata: %+v", model.Thinking)
			}
			upstreamID := strings.TrimPrefix(model.ID, miniMaxProvider+"/")
			for _, format := range []string{"openai", "claude", "openai-response"} {
				for _, tc := range []struct{ suffix, effort, want string }{
					{"", "none", "none"}, {"(none)", "high", "none"}, {"(high)", "none", "high"},
				} {
					for _, stream := range []bool{false, true} {
						t.Run(fmt.Sprintf("auth=%t/%s/%s/%s/stream=%t", forAuth, model.ID, format, tc.suffix, stream), func(t *testing.T) {
							control := `"reasoning_effort":%q`
							if format == "claude" {
								control = `"thinking":{"type":"adaptive"},"output_config":{"effort":%q}`
							} else if format == "openai-response" {
								control = `"reasoning":{"effort":%q}`
							}
							body := []byte(fmt.Sprintf(`{"model":%q,"stream":%t,`+control+`}`, model.ID, stream, tc.effort))
							out, err := thinking.ApplyThinkingWithModelInfo(body, body, model.ID+tc.suffix, format, format, miniMaxProvider, model)
							if err != nil {
								t.Fatal(err)
							}
							hostEffort := effortOf(t, format, out)
							// The host still renders a Claude none as disabled thinking
							// for Flash, so only the plugin's upstream body is checked.
							if flash && hostEffort == "none" && format != "claude" {
								t.Fatalf("host forwarded none for Flash: %s", out)
							}
							if !flash && hostEffort != tc.want {
								t.Fatalf("host changed %s to %s: %s", tc.want, hostEffort, out)
							}
							if stream {
								return
							}
							// Execute the host-adjusted body; the host already consumed the suffix.
							payload := []byte(strings.Replace(string(out), `"stream":false,`, `"messages":[{"role":"user","content":"hi"}],"max_tokens":16,`, 1))
							if format == "openai-response" {
								payload = []byte(strings.Replace(string(out), `"stream":false,`, `"input":"hi",`, 1))
							}
							if _, err := rpc.Execute(context.Background(), miniMaxRequest(upstreamID, format, format, payload, false)); err != nil {
								t.Fatal(err)
							}
							mu.Lock()
							path, upstreamBody := lastPath, lastBody
							mu.Unlock()
							upstreamEffort := effortOf(t, map[string]string{messagesPath: "claude", "/v1/chat/completions": "openai", "/v1/responses": "openai-response"}[path], upstreamBody)
							switch {
							case flash && (upstreamEffort == "none" || strings.Contains(string(upstreamBody), `"disabled"`)):
								t.Fatalf("Flash upstream received disabled thinking: %s", upstreamBody)
							case !flash && upstreamEffort != hostEffort:
								t.Fatalf("plugin changed %s to %s: %s", hostEffort, upstreamEffort, upstreamBody)
							case !flash && path == messagesPath && hostEffort != "none" && !strings.Contains(string(upstreamBody), `"thinking":{"type":"adaptive"}`):
								t.Fatalf("M3 effort without adaptive thinking: %s", upstreamBody)
							}
						})
					}
				}
			}
		}
	}
}

// effortOf reads a body's effective effort; disabled Messages thinking is none.
func effortOf(t *testing.T, format string, body []byte) string {
	t.Helper()
	var got struct {
		Effort    string `json:"reasoning_effort"`
		Thinking  struct{ Type string }
		Output    struct{ Effort string } `json:"output_config"`
		Reasoning struct{ Effort string }
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	switch format {
	case "claude":
		if got.Thinking.Type == "disabled" {
			return "none"
		}
		return got.Output.Effort
	case "openai-response":
		return got.Reasoning.Effort
	}
	return got.Effort
}

func TestMiniMaxNativeMetadata(t *testing.T) {
	var discoveryCalls atomic.Int32
	var customExecutions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			discoveryCalls.Add(1)
			if r.Header.Get("Authorization") != "Bearer offline-test-key" {
				t.Error("discovery did not use the configured key")
			}
			// Non-text models in the listing are skipped quietly.
			io.WriteString(w, `{"object":"list","data":[{"id":"MiniMax-M3","object":"model"},{"id":"speech-2.6-hd","object":"model"}]}`)
		case "/v1/responses":
			var request struct {
				Model string `json:"model"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model != "custom-only-model" {
				t.Errorf("custom request model: %q, %v", request.Model, err)
			}
			customExecutions.Add(1)
			io.WriteString(w, `{"id":"resp_custom","object":"response","status":"completed","output":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	host, file, client := openMiniMax(t, t.TempDir())
	defer client.Shutdown()
	yaml := miniMaxYAML(server.URL, "models:\n  - name: MiniMax-M3\n    display-name: User name\n    max-context-length: 300000\n    input-modalities: [text, image]\n    output-modalities: [text]\n")
	plug, err := registerRPCPlugin(context.Background(), host, file.ID, client, pluginabi.MethodPluginRegister, yaml)
	if err != nil {
		t.Fatal(err)
	}
	if discoveryCalls.Load() != 1 {
		t.Fatalf("discovery calls=%d", discoveryCalls.Load())
	}
	for _, forAuth := range []bool{false, true} {
		var response pluginapi.ModelResponse
		if forAuth {
			response, err = plug.Capabilities.ModelProvider.ModelsForAuth(context.Background(), pluginapi.AuthModelRequest{})
		} else {
			response, err = plug.Capabilities.ModelProvider.StaticModels(context.Background(), pluginapi.StaticModelRequest{})
		}
		if err != nil || len(response.Models) != 2 {
			t.Fatalf("native model response: %+v, %v", response, err)
		}
		byID := map[string]*registry.ModelInfo{}
		for _, item := range response.Models {
			byID[item.ID] = pluginModelInfoToRegistryModelInfo(item)
		}
		flash := byID["minimax/MiniMax-M3.1-Flash"]
		if flash == nil || flash.ContextLength != 1_000_000 || flash.MaxCompletionTokens != 524_288 || flash.Thinking == nil || flash.Thinking.ZeroAllowed {
			t.Fatalf("built-in Flash metadata lost: %+v", flash)
		}
		model := byID["minimax/MiniMax-M3"]
		if model == nil || model.DisplayName != "User name" || model.ContextLength != 300000 || model.MaxCompletionTokens != 524_288 ||
			!reflect.DeepEqual(model.SupportedInputModalities, []string{"text", "image"}) || !reflect.DeepEqual(model.SupportedOutputModalities, []string{"text"}) ||
			model.Thinking == nil || !model.Thinking.ZeroAllowed || len(model.Thinking.Levels) != 6 {
			t.Fatalf("host metadata lost: %+v", model)
		}
		reg := registry.GetGlobalRegistry()
		reg.RegisterClient("metadata-test", miniMaxProvider, []*registry.ModelInfo{model})
		payload := codexmodels.BuildResponseForClient(reg.GetAvailableModels("openai"), reg.GetModelProviders, false, "pi")
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		var catalog struct {
			Models []struct {
				Slug    string   `json:"slug"`
				Context int      `json:"context_window"`
				Output  int      `json:"max_tokens"`
				Inputs  []string `json:"input_modalities"`
				Levels  []struct {
					Effort string `json:"effort"`
				} `json:"supported_reasoning_levels"`
			} `json:"models"`
		}
		if err = json.Unmarshal(encoded, &catalog); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, entry := range catalog.Models {
			if entry.Slug != model.ID {
				continue
			}
			found = true
			if entry.Context != 300000 || entry.Output != 524_288 || !reflect.DeepEqual(entry.Inputs, []string{"text", "image"}) || len(entry.Levels) == 0 {
				t.Fatalf("Codex metadata lost: %+v", entry)
			}
		}
		reg.UnregisterClient("metadata-test")
		if !found {
			t.Fatal("model missing from Codex catalog")
		}
	}
	declared := string(yaml) + "  - name: custom-only-model\n    display-name: Custom model\n    max-context-length: 222222\n    max-tokens: 64000\n    input-modalities: [text]\n    thinking:\n      levels: [high]\n    protocols:\n      responses:\n        enabled: true\n"
	plug, err = registerRPCPlugin(context.Background(), host, file.ID, client, pluginabi.MethodPluginReconfigure, []byte(declared))
	if err != nil {
		t.Fatal(err)
	}
	for _, forAuth := range []bool{false, true} {
		var response pluginapi.ModelResponse
		if forAuth {
			response, err = plug.Capabilities.ModelProvider.ModelsForAuth(context.Background(), pluginapi.AuthModelRequest{})
		} else {
			response, err = plug.Capabilities.ModelProvider.StaticModels(context.Background(), pluginapi.StaticModelRequest{})
		}
		if err != nil || len(response.Models) != 3 {
			t.Fatalf("custom catalog: %+v, %v", response, err)
		}
		found := false
		for _, item := range response.Models {
			if item.ID != "minimax/custom-only-model" {
				continue
			}
			found = true
			model := pluginModelInfoToRegistryModelInfo(item)
			if !model.UserDefined || model.DisplayName != "Custom model" || model.ContextLength != 222222 || model.MaxCompletionTokens != 64000 || model.Thinking == nil || !reflect.DeepEqual(model.Thinking.Levels, []string{"high"}) {
				t.Fatalf("custom metadata lost at host: %+v", model)
			}
		}
		if !found {
			t.Fatal("configured custom model missing from native response")
		}
	}
	rpc := &rpcPluginAdapter{id: file.ID, host: host, client: client, instance: pluginCallbackInstance(client)}
	_, err = rpc.Execute(context.Background(), miniMaxRequest("custom-only-model", "openai-response", "openai-response", []byte(`{"input":"hello"}`), false))
	if err != nil || customExecutions.Load() != 1 {
		t.Fatalf("custom execution: %v, upstream calls=%d", err, customExecutions.Load())
	}
}

func TestMiniMaxNativeABI(t *testing.T) {
	started := make(chan string, 16)
	canceled := make(chan string, 16)
	var blockCatalog atomic.Bool
	var observedMu sync.Mutex
	var observed [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			if blockCatalog.Load() {
				started <- "catalog"
				<-r.Context().Done()
				canceled <- "catalog"
				return
			}
			io.WriteString(w, `{"data":[{"id":"MiniMax-M3"}]}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		observedMu.Lock()
		observed = append(observed, body)
		observedMu.Unlock()
		if r.Header.Get("Authorization") != "Bearer offline-test-key" {
			http.Error(w, "missing bearer", 401)
			return
		}
		if r.URL.Path == "/v1/responses" {
			var req struct {
				Stream bool `json:"stream"`
			}
			_ = json.Unmarshal(body, &req)
			if req.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "event: response.created\ndata: {\"response\":{\"id\":\"r1\"}}\n\nevent: response.output_text.delta\ndata: {\"delta\":\"partial\"}\n\nevent: response.completed")
			} else {
				io.WriteString(w, `{"id":"r1","object":"response","status":"completed","output":[]}`)
			}
			return
		}
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
			Stream bool `json:"stream"`
		}
		if json.Unmarshal(body, &req) != nil || len(req.Messages) == 0 {
			http.Error(w, "invalid test body", 400)
			return
		}
		mode := req.Messages[0].Content
		if r.URL.Path == messagesPath && req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\"}}\n\nevent: content_block_delta\ndata: {\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\nevent: message_stop")
			return
		}
		if strings.HasPrefix(mode, "cancel") || mode == "timeout" || mode == "quiesce" || mode == "scope-end" || mode == "backpressure" {
			if req.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
				w.(http.Flusher).Flush()
				if mode == "backpressure" {
					for i := 0; i < 64; i++ {
						io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n")
						w.(http.Flusher).Flush()
					}
				}
			}
			started <- mode
			<-r.Context().Done()
			canceled <- mode
			return
		}
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
			if mode != "truncated" {
				// Deliberately no [DONE] or final newline: legitimate terminal at EOF.
				io.WriteString(w, `data: {"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}]}`)
			}
			return
		}
		io.WriteString(w, `{"id":"c1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	// Unload exactly as the real Unix loader does: close instance resources,
	// call the plugin shutdown export, free host API and dlclose.
	host, file, client := openMiniMax(t, t.TempDir())
	defer client.Shutdown()
	register := func(timeout string) pluginapi.Plugin {
		t.Helper()
		plug, err := registerRPCPlugin(context.Background(), host, file.ID, client, pluginabi.MethodPluginReconfigure, miniMaxYAML(server.URL, "request-timeout: "+timeout+"\n"))
		if err != nil {
			t.Fatal(err)
		}
		return plug
	}
	plug := register("5s")
	rpc := &rpcPluginAdapter{id: file.ID, host: host, client: client, instance: pluginCallbackInstance(client)}
	auth := &coreauth.Auth{Provider: miniMaxProvider, Attributes: map[string]string{"api_key": "offline-test-key", "region": "global"}}
	// prepareExecutorCall/buildExecutorRequest are the actual host input contract.
	adapter := &executorAdapter{host: host, provider: miniMaxProvider, inputFormats: normalizeExecutorFormats(plug.Capabilities.ExecutorInputFormats), outputFormats: normalizeExecutorFormats(plug.Capabilities.ExecutorOutputFormats)}
	request := func(mode string, stream bool) pluginapi.ExecutorRequest {
		body := []byte(fmt.Sprintf(`{"model":"MiniMax-M3","messages":[{"role":"user","content":%q}],"stream":%t}`, mode, stream))
		prepared, err := adapter.prepareExecutorCall(coreexecutor.Request{Model: "minimax/MiniMax-M3", Payload: body}, coreexecutor.Options{Stream: stream, SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: []byte(`{"messages":[{"role":"user","content":"original"}]}`)})
		if err != nil {
			t.Fatal(err)
		}
		return buildExecutorRequest(host, miniMaxProvider, auth, prepared.req, prepared.opts)
	}
	await := func(ch <-chan string, want string) {
		t.Helper()
		select {
		case got := <-ch:
			if got != want {
				t.Fatalf("event %q, want %q", got, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("no %s event", want)
		}
	}
	t.Run("effective payload through host preparation", func(t *testing.T) {
		if _, err := rpc.Execute(context.Background(), request("intercepted", false)); err != nil {
			t.Fatal(err)
		}
		observedMu.Lock()
		body := string(observed[len(observed)-1])
		observedMu.Unlock()
		if !strings.Contains(body, "intercepted") || strings.Contains(body, "original") || !strings.Contains(body, `"reasoning_split":true`) {
			t.Fatalf("host effective payload or MiniMax policy was lost: %s", body)
		}
	})
	t.Run("native Responses file input", func(t *testing.T) {
		req := miniMaxRequest("MiniMax-M3", "openai-response", "openai-response", []byte(`{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file_example"}]}]}`), false)
		if _, err := rpc.Execute(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("explicit empty payload never replays original", func(t *testing.T) {
		for _, stream := range []bool{false, true} {
			req := request("intercepted", stream)
			req.Payload = []byte{}
			observedMu.Lock()
			before := len(observed)
			observedMu.Unlock()
			var err error
			if stream {
				_, err = rpc.ExecuteStream(context.Background(), req)
			} else {
				_, err = rpc.Execute(context.Background(), req)
			}
			if err == nil {
				t.Fatal("empty effective payload replayed original")
			}
			observedMu.Lock()
			after := len(observed)
			observedMu.Unlock()
			if after != before {
				t.Fatal("empty effective payload reached upstream")
			}
		}
	})
	t.Run("incomplete terminal headers fail through ABI", func(t *testing.T) {
		for _, tc := range []struct{ format, body string }{
			{"openai-response", `{"input":"partial","stream":true}`},
			{"claude", `{"messages":[{"role":"user","content":"partial"}],"max_tokens":16,"stream":true}`},
		} {
			resp, err := rpc.ExecuteStream(context.Background(), miniMaxRequest("MiniMax-M3", tc.format, "openai-response", []byte(tc.body), true))
			if err != nil {
				t.Fatal(err)
			}
			var output strings.Builder
			var streamErr error
			for chunk := range resp.Chunks {
				output.Write(chunk.Payload)
				if chunk.Err != nil {
					streamErr = chunk.Err
				}
			}
			if streamErr == nil || strings.Contains(output.String(), "response.completed") {
				t.Fatal("incomplete terminal header reported success")
			}
		}
	})
	t.Run("independent request scopes", func(t *testing.T) {
		ctxOne, cancelOne := context.WithCancel(context.Background())
		defer cancelOne()
		ctxTwo, cancelTwo := context.WithCancel(context.Background())
		defer cancelTwo()
		doneOne, doneTwo := make(chan error, 1), make(chan error, 1)
		go func() { _, err := rpc.Execute(ctxOne, request("cancel-one", false)); doneOne <- err }()
		await(started, "cancel-one")
		go func() { _, err := rpc.Execute(ctxTwo, request("cancel-two", false)); doneTwo <- err }()
		await(started, "cancel-two")
		cancelOne()
		await(canceled, "cancel-one")
		select {
		case <-doneTwo:
			t.Fatal("canceling one request canceled another")
		case <-time.After(30 * time.Millisecond):
		}
		cancelTwo()
		await(canceled, "cancel-two")
		if <-doneOne == nil || <-doneTwo == nil {
			t.Fatal("canceled execution succeeded")
		}
	})
	t.Run("callback scope end without context cancellation", func(t *testing.T) {
		id, closeScope := rpc.openHostCallbackContext(context.Background())
		done := make(chan error, 1)
		go func() {
			_, err := callPlugin[pluginapi.ExecutorResponse](context.Background(), client, pluginabi.MethodExecutorExecute, rpcExecutorRequest{ExecutorRequest: request("scope-end", false), HostCallbackID: id})
			done <- err
		}()
		await(started, "scope-end")
		closeScope()
		await(canceled, "scope-end")
		if <-done == nil {
			t.Fatal("scope end did not cancel HTTP")
		}
	})
	t.Run("stream scope lives past execute return", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		response, err := rpc.ExecuteStream(ctx, request("cancel-stream", true))
		if err != nil {
			t.Fatal(err)
		}
		await(started, "cancel-stream")
		select {
		case chunk := <-response.Chunks:
			if len(chunk.Payload) == 0 || chunk.Err != nil {
				t.Fatal("stream died on executor return")
			}
		case <-time.After(time.Second):
			t.Fatal("no stream data")
		}
		cancel()
		await(canceled, "cancel-stream")
		for range response.Chunks {
		}
	})
	t.Run("legitimate terminal and truncated EOF", func(t *testing.T) {
		for _, mode := range []string{"stream-ok", "truncated"} {
			req := request(mode, true)
			req.Format = "openai-response"
			response, err := rpc.ExecuteStream(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			var output strings.Builder
			var streamErr error
			for chunk := range response.Chunks {
				output.Write(chunk.Payload)
				if chunk.Err != nil {
					streamErr = chunk.Err
				}
			}
			if mode == "truncated" {
				if streamErr == nil || strings.Contains(output.String(), "response.completed") {
					t.Fatal("truncated stream reported success")
				}
			} else if streamErr != nil || strings.Count(output.String(), "event: response.completed") != 1 {
				t.Fatalf("legitimate EOF: %v", streamErr)
			}
		}
	})
	t.Run("plugin request timeout cancels transport", func(t *testing.T) {
		register("100ms")
		_, err := rpc.Execute(context.Background(), request("timeout", false))
		await(started, "timeout")
		await(canceled, "timeout")
		if err == nil {
			t.Fatal("timed out execution succeeded")
		}
		register("5s")
	})
	t.Run("quiesce cancels and drains", func(t *testing.T) {
		response, err := rpc.ExecuteStream(context.Background(), request("backpressure", true))
		if err != nil {
			t.Fatal(err)
		}
		await(started, "backpressure")
		// Stop consuming long enough to park a real host.stream.emit callback.
		time.Sleep(40 * time.Millisecond)
		blockCatalog.Store(true)
		reconfigured := make(chan error, 1)
		go func() {
			_, err := registerRPCPlugin(context.Background(), host, file.ID, client, pluginabi.MethodPluginReconfigure, miniMaxYAML(server.URL, ""))
			reconfigured <- err
		}()
		await(started, "catalog")
		drain := make(chan struct{})
		if _, err := callPlugin[struct{}](context.Background(), client, pluginabi.MethodPluginQuiesce, struct{}{}); err != nil {
			t.Fatal(err)
		}
		// HTTP cancellations may race each other.
		got := map[string]bool{}
		for i := 0; i < 2; i++ {
			select {
			case mode := <-canceled:
				got[mode] = true
			case <-time.After(2 * time.Second):
				t.Fatal("quiesce failed to cancel HTTP")
			}
		}
		if !got["catalog"] || !got["backpressure"] {
			t.Fatalf("unexpected canceled tasks: %v", got)
		}
		<-reconfigured
		go func() {
			for range response.Chunks {
			}
			close(drain)
		}()
		<-drain
		if _, err := rpc.Execute(context.Background(), request("intercepted", false)); err == nil {
			t.Fatal("quiesce admitted new work")
		}
	})
	host.httpOperations.mu.Lock()
	operations := len(host.httpOperations.operations)
	host.httpOperations.mu.Unlock()
	host.httpStreams.mu.Lock()
	streams := len(host.httpStreams.streams)
	host.httpStreams.mu.Unlock()
	if operations != 0 || streams != 0 {
		t.Fatalf("host resource leak: operations=%d streams=%d", operations, streams)
	}
}

// Host unload closes HTTP operations/streams before entering shutdown. Pin that
// ordering with live synchronous and asynchronous FFI work, without quiesce.
func TestMiniMaxNativeUnload(t *testing.T) {
	started, canceled := make(chan bool, 2), make(chan bool, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"data":[{"id":"MiniMax-M3"}]}`)
			return
		}
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
			w.(http.Flusher).Flush()
		}
		started <- req.Stream
		<-r.Context().Done()
		canceled <- req.Stream
	}))
	defer server.Close()
	host, file, client := openMiniMax(t, t.TempDir())
	defer client.Shutdown()
	_, err := registerRPCPlugin(context.Background(), host, file.ID, client, pluginabi.MethodPluginRegister, miniMaxYAML(server.URL, "request-timeout: 30s\n"))
	if err != nil {
		t.Fatal(err)
	}
	rpc := &rpcPluginAdapter{id: file.ID, host: host, client: client, instance: pluginCallbackInstance(client)}
	req := func(stream bool) pluginapi.ExecutorRequest {
		return miniMaxRequest("MiniMax-M3", "openai", "", []byte(fmt.Sprintf(`{"messages":[{"role":"user","content":"pending"}],"stream":%t}`, stream)), stream)
	}
	nonstreamDone := make(chan error, 1)
	go func() { _, err := rpc.Execute(context.Background(), req(false)); nonstreamDone <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("nonstream did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	response, err := rpc.ExecuteStream(ctx, req(true))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("stream did not start")
	}
	unloaded := make(chan struct{})
	go func() {
		host.closeHostHTTPPluginResources(file.ID, pluginCallbackInstance(client))
		shutdownPluginClient(context.Background(), client)
		close(unloaded)
	}()
	select {
	case <-unloaded:
	case <-time.After(2 * time.Second):
		t.Fatal("unload deadlocked with host HTTP closure")
	}
	if <-nonstreamDone == nil {
		t.Fatal("unload let canceled HTTP succeed")
	}
	for i := 0; i < 2; i++ {
		select {
		case <-canceled:
		case <-time.After(time.Second):
			t.Fatal("unload left HTTP active")
		}
	}
	cancel() // host owns downstream cleanup after its callback instance closes
	for range response.Chunks {
	}
	host.httpOperations.mu.Lock()
	operations := len(host.httpOperations.operations)
	host.httpOperations.mu.Unlock()
	host.httpStreams.mu.Lock()
	streams := len(host.httpStreams.streams)
	host.httpStreams.mu.Unlock()
	if operations != 0 || streams != 0 {
		t.Fatal("unload leaked host HTTP resources")
	}
}

// Exercise credential naming through the real v8 host disk listing and
// host.auth.save (which uses os.WriteFile), plus the native plugin ABI.
func TestMiniMaxNativeAuthNames(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.Error(w, "unexpected offline request", 400)
			return
		}
		io.WriteString(w, `{"data":[{"id":"MiniMax-M3"}]}`)
	}))
	defer server.Close()
	dir := t.TempDir()
	original := []byte(`{"type":"minimax","id":"existing-id","api_key":"offline-existing","label":"Custom Work","region":"cn","key_type":"subscription","disabled":true,"custom":"preserved"}`)
	originalPath := filepath.Join(dir, "personal.json")
	if err := os.WriteFile(originalPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	host, file, client := openMiniMax(t, dir)
	defer client.Shutdown()
	yaml := []byte(fmt.Sprintf("api-keys:\n  - value: offline-first\n    name: Personal\n  - value: offline-second\n    name: personal\n  - value: offline-third\n    name: CON\n  - value: offline-fourth\n    name: aux\n  - value: sk-cp-offline-plan\n  - value: offline-api-cn\n    region: cn\nbase-url: %s\nallow-http: true\n", server.URL))
	for _, method := range []string{pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure} {
		if _, err := registerRPCPlugin(context.Background(), host, file.ID, client, method, yaml); err != nil {
			t.Fatal(err)
		}
	}
	after, err := os.ReadFile(originalPath)
	if err != nil || string(after) != string(original) {
		t.Fatal("real host save overwrote existing credential")
	}
	expected := map[string]struct{ label, region, keyType string }{
		"Personal-2.json":     {"Personal", "global", "api"},
		"personal-3.json":     {"personal", "global", "api"},
		"MiniMax-CON.json":    {"CON", "global", "api"},
		"MiniMax-aux.json":    {"aux", "global", "api"},
		"MiniMax-M-Plan.json": {"MiniMax M Plan", "global", "subscription"},
		"MiniMax-API-CN.json": {"MiniMax API CN", "cn", "api"},
	}
	for name, want := range expected {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("credential %s missing: %v", name, err)
		}
		var record struct {
			Type    string `json:"type"`
			Label   string `json:"label"`
			ID      string `json:"id"`
			Region  string `json:"region"`
			KeyType string `json:"key_type"`
		}
		if err := json.Unmarshal(raw, &record); err != nil || record.Type != miniMaxProvider || record.Label != want.label || record.Region != want.region || record.KeyType != want.keyType || !strings.HasPrefix(record.ID, "minimax-key-") {
			t.Fatalf("credential label/identity changed: %s: %s", name, raw)
		}
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != len(expected)+1 {
		t.Fatal("reconfigure duplicated credential files")
	}
	rpc := &rpcPluginAdapter{id: file.ID, host: host, client: client, instance: pluginCallbackInstance(client)}
	parsed, err := rpc.ParseAuth(context.Background(), pluginapi.AuthParseRequest{Provider: miniMaxProvider, FileName: "personal.json", RawJSON: original})
	if err != nil || !parsed.Handled || parsed.Auth.Label != "Custom Work" || parsed.Auth.ID != "existing-id" || string(parsed.Auth.StorageJSON) != string(original) {
		t.Fatal("native parsing replaced custom label/metadata")
	}
	// An unconfigured record keeps its own region and type; subscriptions rank first.
	if attrs := parsed.Auth.Attributes; attrs["region"] != "cn" || attrs["key_type"] != "subscription" || attrs["priority"] != "1" {
		t.Fatalf("record attributes: %v", attrs)
	}
}

// Invalid terminal snapshots must fail through the real host stream bridge,
// for both native passthrough and cross-protocol synthesis, with or without
// the final SSE separator. Valid snapshots must retain one terminal only.
func TestMiniMaxNativeTerminalShapes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"data":[{"id":"MiniMax-M3"}]}`)
			return
		}
		var req struct {
			Input    string `json:"input"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid offline request", 400)
			return
		}
		mode := req.Input
		if len(req.Messages) > 0 {
			mode = req.Messages[0].Content
		}
		parts := strings.Split(mode, "/")
		if len(parts) != 3 {
			http.Error(w, "invalid offline test mode", 400)
			return
		}
		shape, ending, event := parts[0], parts[1], parts[2]
		payload := ""
		if r.URL.Path == messagesPath {
			switch shape {
			case "empty":
				payload = `{}`
			case "unrelated":
				payload = `{"type":"ping"}`
			case "valid":
				payload = `{"type":"message_stop"}`
			}
		} else {
			status := strings.TrimPrefix(event, "response.")
			switch shape {
			case "empty":
				payload = `{}`
			case "empty-response":
				payload = `{"response":{}}`
			case "missing-output":
				payload = fmt.Sprintf(`{"type":%q,"response":{"id":"resp_1","object":"response","status":%q}}`, event, status)
			case "valid":
				payload = fmt.Sprintf(`{"type":%q,"response":{"id":"resp_1","object":"response","status":%q,"output":[]}}`, event, status)
			}
		}
		if payload == "" {
			http.Error(w, "unknown offline shape", 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if r.URL.Path == messagesPath {
			io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\"}}\n\n")
			if shape == "valid" {
				io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
			}
		} else {
			io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n")
		}
		io.WriteString(w, "event: "+event+"\ndata: "+payload)
		if ending == "framed" {
			io.WriteString(w, "\n\n")
		}
	}))
	defer server.Close()
	host, file, client := openMiniMax(t, t.TempDir())
	defer client.Shutdown()
	_, err := registerRPCPlugin(context.Background(), host, file.ID, client, pluginabi.MethodPluginRegister, miniMaxYAML(server.URL, ""))
	if err != nil {
		t.Fatal(err)
	}
	rpc := &rpcPluginAdapter{id: file.ID, host: host, client: client, instance: pluginCallbackInstance(client)}
	for _, route := range []struct {
		source         string
		events, shapes []string
	}{
		{"claude", []string{"message_stop"}, []string{"empty", "unrelated", "valid"}},
		{"openai-response", []string{"response.completed", "response.incomplete"}, []string{"empty", "empty-response", "missing-output", "valid"}},
	} {
		for _, event := range route.events {
			for _, shape := range route.shapes {
				for _, ending := range []string{"framed", "eof"} {
					for _, output := range []string{"openai", "claude", "openai-response"} {
						t.Run(route.source+"/"+event+"/"+shape+"/"+ending+"/"+output, func(t *testing.T) {
							mode := shape + "/" + ending + "/" + event
							var body []byte
							if route.source == "claude" {
								body = []byte(fmt.Sprintf(`{"messages":[{"role":"user","content":%q}],"max_tokens":16,"stream":true}`, mode))
							} else {
								body = []byte(fmt.Sprintf(`{"input":%q,"stream":true}`, mode))
							}
							resp, err := rpc.ExecuteStream(context.Background(), miniMaxRequest("MiniMax-M3", route.source, output, body, true))
							if err != nil {
								t.Fatal(err)
							}
							var events strings.Builder
							var streamErr error
							for chunk := range resp.Chunks {
								events.Write(chunk.Payload)
								if chunk.Err != nil {
									streamErr = chunk.Err
								}
							}
							if shape != "valid" {
								if streamErr == nil || strings.Contains(events.String(), "response.completed") || strings.Contains(events.String(), "response.incomplete") || strings.Contains(events.String(), "message_stop") || strings.Contains(events.String(), `"finish_reason":"stop"`) {
									t.Fatal("invalid terminal reported success through ABI")
								}
							} else {
								if streamErr != nil {
									t.Fatalf("valid terminal failed: %v", streamErr)
								}
								marker := "event: message_stop"
								if output == "openai-response" {
									marker = "event: response.completed"
									if route.source == "openai-response" {
										marker = "event: " + event
									}
								} else if output == "openai" {
									marker = `"finish_reason":"stop"`
									if event == "response.incomplete" {
										marker = `"finish_reason":"length"`
									}
								}
								if strings.Count(events.String(), marker) != 1 {
									t.Fatal("valid terminal missing/duplicated")
								}
							}
						})
					}
				}
			}
		}
	}
}

// miniMaxHTTPRouter registers the loaded plugin with a real CPA auth manager
// and the actual HTTP handlers.
func miniMaxHTTPRouter(t *testing.T, host *Host, file pluginFile, plug pluginapi.Plugin, bootstrapRetries int) (*gin.Engine, *coreauth.Manager) {
	t.Helper()
	records := []capabilityRecord{{id: file.ID, path: file.Path, version: plug.Metadata.Version, meta: plug.Metadata, plugin: plug}}
	host.mu.Lock()
	host.rebuildActivePluginMapsLocked(records)
	host.snapshot.Store(&Snapshot{enabled: true, records: records, quotaSupportedProviders: make(map[string][]string)})
	host.mu.Unlock()
	manager := coreauth.NewManager(nil, nil, nil)
	host.RegisterModels(context.Background(), registry.GetGlobalRegistry())
	host.RegisterExecutors(manager, registry.GetGlobalRegistry())
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{Streaming: sdkconfig.StreamingConfig{BootstrapRetries: bootstrapRetries}}, manager)
	base.SetModelRouterHost(host)
	router := gin.New()
	router.POST("/v1/chat/completions", openai.NewOpenAIAPIHandler(base).ChatCompletions)
	router.POST("/v1/messages", claude.NewClaudeCodeAPIHandler(base).ClaudeMessages)
	router.POST("/v1/responses", openai.NewOpenAIResponsesAPIHandler(base).Responses)
	return router, manager
}

func registerMiniMaxClient(t *testing.T, authID string, models ...string) {
	t.Helper()
	infos := []*registry.ModelInfo{}
	for _, model := range models {
		infos = append(infos, &registry.ModelInfo{ID: miniMaxProvider + "/" + model, Object: "model", OwnedBy: miniMaxProvider})
	}
	registry.GetGlobalRegistry().RegisterClient(authID, miniMaxProvider, infos)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
}

// Exercise the actual HTTP handlers after loading the native plugin. Chat
// handlers add their own SSE framing; Messages/Responses accept complete frames.
// Declared single-protocol models cover every client/upstream conversion.
func TestMiniMaxNativeHTTPStreams(t *testing.T) {
	var upstreamRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"data":[]}`)
			return
		}

		upstreamRequests.Add(1)
		requestBody, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "invalid test request", 400)
			return
		}
		mode := "ok"
		for _, failureMode := range []string{"stream-error", "stream-eof", "stream-empty-error"} {
			if strings.Contains(string(requestBody), failureMode) {
				mode = failureMode
			}
		}

		w.Header().Set("Content-Type", "text/event-stream")
		frame := func(name, data string) {
			if name != "" {
				io.WriteString(w, "event: "+name+"\n")
			}
			io.WriteString(w, "data: "+data+"\n\n")
		}
		afterText := func() bool {
			w.(http.Flusher).Flush()
			if mode == "stream-eof" {
				return true
			}
			if mode == "ok" {
				return false
			}
			message := "offline stream failure"
			if mode == "stream-empty-error" {
				message = ""
			}
			name := "error"
			payload := map[string]any{"type": "error"}
			if r.URL.Path == "/v1/responses" {
				payload["message"], payload["status_code"] = message, 500
			} else {
				payload["error"] = map[string]any{"type": "api_error", "message": message, "code": 500}
				if r.URL.Path == "/v1/chat/completions" {
					name = ""
					delete(payload, "type")
				}
			}
			data, _ := json.Marshal(payload)
			frame(name, string(data))
			return true
		}
		switch r.URL.Path {
		case "/v1/chat/completions":
			frame("", `{"id":"r","model":"chat-only","choices":[{"delta":{"content":"hello"},"finish_reason":null}]}`)
			if afterText() {
				return
			}
			frame("", `{"id":"r","model":"chat-only","choices":[{"delta":{},"finish_reason":"stop"}]}`)
			frame("", "[DONE]")
		case messagesPath:
			frame("message_start", `{"type":"message_start","message":{"id":"r","model":"messages-only"}}`)
			frame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
			frame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`)
			if afterText() {
				return
			}
			frame("content_block_stop", `{"type":"content_block_stop","index":0}`)
			frame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`)
			frame("message_stop", `{"type":"message_stop"}`)
		case "/v1/responses":
			frame("response.created", `{"type":"response.created","response":{"id":"r","model":"responses-only"}}`)
			frame("response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg","role":"assistant","content":[]}}`)
			frame("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg","output_index":0,"content_index":0,"delta":"hello"}`)
			if afterText() {
				return
			}
			frame("response.completed", `{"type":"response.completed","response":{"id":"r","model":"responses-only","object":"response","status":"completed","output":[{"type":"message","id":"msg","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	host, file, client := openMiniMax(t, t.TempDir())
	defer client.Shutdown()
	models := "models:\n  - name: chat-only\n    protocols:\n      chat-completions: {}\n  - name: messages-only\n    protocols:\n      messages: {}\n  - name: responses-only\n    protocols:\n      responses: {}\n"
	plug, err := registerRPCPlugin(context.Background(), host, file.ID, client, pluginabi.MethodPluginRegister, miniMaxYAML(upstream.URL, models))
	if err != nil {
		t.Fatal(err)
	}
	// Enable safe pre-first-byte retries; these cases must still call upstream once.
	router, manager := miniMaxHTTPRouter(t, host, file, plug, 1)
	auth, err := manager.Register(context.Background(), &coreauth.Auth{ID: "offline-http", Provider: miniMaxProvider, Attributes: map[string]string{"api_key": "offline-test-key"}, Metadata: map[string]any{"disable_cooling": true}})
	if err != nil {
		t.Fatal(err)
	}
	upstreamModels := []string{"chat-only", "messages-only", "responses-only"}
	registerMiniMaxClient(t, auth.ID, upstreamModels...)

	for _, model := range upstreamModels {
		for _, endpoint := range []string{"chat/completions", "messages", "responses"} {
			for _, mode := range []string{"ok", "stream-error", "stream-eof", "stream-empty-error"} {
				t.Run(model+"/"+endpoint+"/"+mode, func(t *testing.T) {
					before := upstreamRequests.Load()
					body := fmt.Sprintf(`{"model":"minimax/%s","stream":true,"max_tokens":16,"messages":[{"role":"user","content":%q}]}`, model, mode)
					if endpoint == "responses" {
						body = fmt.Sprintf(`{"model":"minimax/%s","stream":true,"input":%q}`, model, mode)
					}
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					req := httptest.NewRequest("POST", "/v1/"+endpoint, strings.NewReader(body)).WithContext(ctx)
					req.Header.Set("Content-Type", "application/json")
					recorder := httptest.NewRecorder()
					router.ServeHTTP(recorder, req)
					wire := recorder.Body.String()
					if recorder.Code != 200 || !strings.Contains(wire, "hello") || strings.Contains(wire, "data: data:") {
						t.Fatalf("HTTP %d: %s", recorder.Code, wire)
					}
					if upstreamRequests.Load()-before != 1 {
						t.Fatalf("stream retried after output: %s", wire)
					}
					if mode != "ok" {
						if strings.Contains(wire, "[DONE]") || strings.Contains(wire, `"finish_reason":"`) || strings.Contains(wire, "event: message_stop") || strings.Contains(wire, "event: response.completed") || strings.Contains(wire, "event: response.incomplete") {
							t.Fatalf("failed stream reported successful termination: %s", wire)
						}
						errorPos := strings.Index(wire, `"error"`)
						if errorPos < strings.Index(wire, "hello") {
							t.Fatalf("body not followed by an error: %s", wire)
						}
						if mode == "stream-error" && !strings.Contains(wire, "offline stream failure") {
							t.Fatalf("upstream error lost: %s", wire)
						}
						return
					}
					terminal := "event: response.completed"
					if endpoint == "chat/completions" {
						terminal = "data: [DONE]"
					} else if endpoint == "messages" {
						terminal = "event: message_stop"
					}
					if strings.Count(wire, terminal) != 1 {
						t.Fatalf("terminal count: %s", wire)
					}
				})
			}
		}
	}
}

// A subscription key that exhausts its window (base_resp 2056, HTTP 429) must
// enter CPA quota cooldown while the request completes on the pay-as-you-go
// key. Credentials are parsed by the plugin, so priorities are its defaults.
func TestMiniMaxNativeCredentialRotation(t *testing.T) {
	var planCalls, apiCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"data":[]}`)
			return
		}
		switch r.Header.Get("Authorization") {
		case "Bearer sk-cp-offline-plan":
			planCalls.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"usage limit exceeded (2056)"},"base_resp":{"status_code":2056,"status_msg":"usage limit exceeded"}}`)
		case "Bearer offline-api":
			apiCalls.Add(1)
			io.WriteString(w, `{"id":"c1","object":"chat.completion","model":"MiniMax-M3","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`)
		default:
			http.Error(w, "unknown key", 401)
		}
	}))
	defer upstream.Close()
	host, file, client := openMiniMax(t, t.TempDir())
	defer client.Shutdown()
	yaml := []byte(fmt.Sprintf("api-keys:\n  - value: offline-api\n  - value: sk-cp-offline-plan\nbase-url: %s\nallow-http: true\n", upstream.URL))
	plug, err := registerRPCPlugin(context.Background(), host, file.ID, client, pluginabi.MethodPluginRegister, yaml)
	if err != nil {
		t.Fatal(err)
	}
	router, manager := miniMaxHTTPRouter(t, host, file, plug, 0)
	rpc := &rpcPluginAdapter{id: file.ID, host: host, client: client, instance: pluginCallbackInstance(client)}
	for _, cred := range []struct{ id, key, priority string }{{"offline-api", "offline-api", "0"}, {"offline-plan", "sk-cp-offline-plan", "1"}} {
		raw := []byte(fmt.Sprintf(`{"type":"minimax","id":%q,"api_key":%q}`, cred.id, cred.key))
		parsed, err := rpc.ParseAuth(context.Background(), pluginapi.AuthParseRequest{Provider: miniMaxProvider, FileName: cred.id + ".json", RawJSON: raw})
		if err != nil || !parsed.Handled || parsed.Auth.Attributes["priority"] != cred.priority {
			t.Fatalf("parse %s: %+v, %v", cred.id, parsed.Auth.Attributes, err)
		}
		auth, err := manager.Register(context.Background(), &coreauth.Auth{ID: parsed.Auth.ID, Provider: miniMaxProvider, Label: parsed.Auth.Label, Attributes: parsed.Auth.Attributes})
		if err != nil {
			t.Fatal(err)
		}
		registerMiniMaxClient(t, auth.ID, "MiniMax-M3")
	}
	send := func() string {
		t.Helper()
		body := `{"model":"minimax/MiniMax-M3","messages":[{"role":"user","content":"hi"}]}`
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, req)
		if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), "hello") {
			t.Fatalf("HTTP %d: %s", recorder.Code, recorder.Body.String())
		}
		return recorder.Body.String()
	}
	send()
	if planCalls.Load() != 1 || apiCalls.Load() != 1 {
		t.Fatalf("first request: plan calls=%d api calls=%d; want subscription first, then fallback", planCalls.Load(), apiCalls.Load())
	}
	plan, ok := manager.GetByID("offline-plan")
	if !ok {
		t.Fatal("subscription auth missing")
	}
	cooling := plan.Quota.Exceeded || !plan.NextRetryAfter.IsZero()
	for _, state := range plan.ModelStates {
		cooling = cooling || state.Quota.Exceeded || !state.NextRetryAfter.IsZero()
	}
	if !cooling {
		t.Fatalf("2056 did not cool the subscription down: %+v", plan)
	}
	send()
	if planCalls.Load() != 1 || apiCalls.Load() != 2 {
		t.Fatalf("cooling subscription was retried: plan calls=%d api calls=%d", planCalls.Load(), apiCalls.Load())
	}
}
