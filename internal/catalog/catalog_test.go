package catalog

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/dillonzq/cpa-minimax/internal/config"
	"github.com/dillonzq/cpa-minimax/internal/modelmeta"
)

const (
	testKey  = "sk-test-key-123"
	testBase = "https://api.minimax.io"
)

// fakeClient is a canned, thread-safe HostClient capturing the last request.
type fakeClient struct {
	mu     sync.Mutex
	resp   pluginapi.HTTPResponse
	err    error
	gotReq *pluginapi.HTTPRequest
	calls  int
}

func (f *fakeClient) Do(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotReq = &req
	f.calls++
	return f.resp, f.err
}

func (f *fakeClient) set(resp pluginapi.HTTPResponse, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resp, f.err = resp, err
}

func listing(ids ...string) pluginapi.HTTPResponse {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprintf(`{"id":%q,"object":"model","created":1,"owned_by":"minimax"}`, id)
	}
	return pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"object":"list","data":[` + strings.Join(parts, ",") + `]}`)}
}

func testCfg() config.Config {
	return config.Config{
		Region:           config.RegionGlobal,
		BaseURL:          testBase,
		ModelPrefix:      config.ModelPrefix{Enabled: true, Value: "minimax"},
		MaxResponseBytes: 1 << 20,
		Catalog:          config.Catalog{Discover: true, StaleWhileUnavailable: true},
	}
}

type protoEntry struct {
	name string
	config.ProtocolOverride
}

// declare adds a models[] entry carrying only protocol settings.
func declare(cfg *config.Config, name string, protocols ...protoEntry) {
	set := make(map[string]config.ProtocolOverride, len(protocols))
	for _, p := range protocols {
		set[p.name] = p.ProtocolOverride
	}
	cfg.Models = append(cfg.Models, config.ModelOverride{Name: name, Protocols: set})
}

// proto switches a protocol on; an empty endpoint keeps the inherited path.
func proto(protocol, endpoint string) protoEntry {
	return protoEntry{protocol, config.ProtocolOverride{Endpoint: endpoint}}
}

// off switches a protocol off.
func off(protocol string) protoEntry {
	disabled := false
	return protoEntry{protocol, config.ProtocolOverride{Enabled: &disabled}}
}

// routesOf lists a record's protocols in order.
func routesOf(rec ModelRecord) []Route {
	out := make([]Route, len(rec.Routes))
	for i, t := range rec.Routes {
		out[i] = t.Protocol
	}
	return out
}

func ids(models []ModelRecord) []string {
	out := make([]string, len(models))
	for i, m := range models {
		out[i] = m.UpstreamID
	}
	return out
}

func mustRefresh(t *testing.T, m *Manager) {
	t.Helper()
	if err := m.Refresh(context.Background(), testBase, testKey); err != nil {
		t.Fatalf("Refresh: unexpected error %v", err)
	}
}

func findModel(t *testing.T, models []ModelRecord, upstreamID string) ModelRecord {
	t.Helper()
	for _, mo := range models {
		if mo.UpstreamID == upstreamID {
			return mo
		}
	}
	t.Fatalf("model %q not found in %v", upstreamID, ids(models))
	return ModelRecord{}
}

var allRoutes = []Route{RouteMessages, RouteChatCompletions, RouteResponses}

func TestRouteEndpointPath(t *testing.T) {
	cases := []struct {
		route Route
		want  string
	}{
		{RouteChatCompletions, "/v1/chat/completions"},
		{RouteMessages, "/anthropic/v1/messages"},
		{RouteResponses, "/v1/responses"},
		{Route("gemini"), ""},
	}
	for _, c := range cases {
		if got := c.route.EndpointPath(); got != c.want {
			t.Errorf("%s.EndpointPath() = %q, want %q", c.route, got, c.want)
		}
	}
}

func TestBuiltinModelsPublishedWithoutRefresh(t *testing.T) {
	m := New(testCfg(), nil)
	models := m.Models()
	if !reflect.DeepEqual(ids(models), []string{"MiniMax-M3.1-Flash", "MiniMax-M3"}) {
		t.Fatalf("models = %v", ids(models))
	}
	flash := models[0]
	if flash.PublicID != "minimax/MiniMax-M3.1-Flash" || flash.DisplayName != "MiniMax M3.1 Flash" || flash.UserDefined {
		t.Errorf("flash identity = %+v", flash)
	}
	if flash.ContextLimit != 1_000_000 || flash.OutputLimit != 524_288 {
		t.Errorf("flash limits = %d/%d", flash.ContextLimit, flash.OutputLimit)
	}
	if !reflect.DeepEqual(routesOf(flash), allRoutes) {
		t.Errorf("flash routes = %v", routesOf(flash))
	}
	if flash.Thinking == nil || flash.Thinking.ZeroAllowed || !reflect.DeepEqual(flash.Thinking.Levels, []string{"low", "medium", "high", "xhigh", "max"}) {
		t.Errorf("flash thinking = %+v", flash.Thinking)
	}
	m3 := models[1]
	if m3.Thinking == nil || !m3.Thinking.ZeroAllowed || m3.Thinking.Levels[0] != "none" {
		t.Errorf("m3 thinking = %+v", m3.Thinking)
	}
}

func TestRefreshRequestShape(t *testing.T) {
	fc := &fakeClient{resp: listing("MiniMax-M3")}
	m := New(testCfg(), fc)
	if err := m.Refresh(context.Background(), "https://api.minimax.cn", testKey); err != nil {
		t.Fatal(err)
	}
	req := fc.gotReq
	if req == nil || req.Method != http.MethodGet || req.URL != "https://api.minimax.cn/v1/models" {
		t.Fatalf("request = %+v", req)
	}
	if req.Headers.Get("Authorization") != "Bearer "+testKey || req.Headers.Get("Accept") != "application/json" {
		t.Fatalf("headers = %v", req.Headers)
	}
}

func TestDiscoveryDisabledSkipsUpstream(t *testing.T) {
	cfg := testCfg()
	cfg.Catalog.Discover = false
	declare(&cfg, "custom-model", proto("messages", ""))
	fc := &fakeClient{resp: listing("MiniMax-M2.7")}
	m := New(cfg, fc)
	mustRefresh(t, m)
	if fc.calls != 0 {
		t.Fatalf("discovery disabled but %d upstream calls made", fc.calls)
	}
	if got := ids(m.Models()); !reflect.DeepEqual(got, []string{"MiniMax-M3.1-Flash", "MiniMax-M3", "custom-model"}) {
		t.Fatalf("models = %v", got)
	}
}

func TestDiscoveryMergesWithBuiltins(t *testing.T) {
	fc := &fakeClient{resp: listing("MiniMax-M3", "minimax-m2.7", "speech-2.6-hd", "hailuo-02", "image-01", "MiniMax-M3.2")}
	m := New(testCfg(), fc)
	mustRefresh(t, m)
	got := ids(m.Models())
	want := []string{"MiniMax-M3", "minimax-m2.7", "MiniMax-M3.2", "MiniMax-M3.1-Flash"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
	// Non-text models are skipped quietly, not reported as unsupported.
	if u := m.Unsupported(); len(u) != 0 {
		t.Errorf("unsupported = %+v", u)
	}
	if w := m.Warnings(); len(w) != 0 {
		t.Errorf("warnings = %v", w)
	}
	// Built-in metadata is matched case-insensitively.
	m27 := findModel(t, m.Models(), "minimax-m2.7")
	if m27.DisplayName != "MiniMax M2.7" || m27.ContextLimit != 204_800 || m27.Thinking != nil {
		t.Errorf("m2.7 metadata = %+v", m27)
	}
	// An unknown text model gets family routes and no invented metadata.
	m32 := findModel(t, m.Models(), "MiniMax-M3.2")
	if m32.DisplayName != "MiniMax-M3.2" || m32.ContextLimit != 0 || m32.Thinking != nil || !reflect.DeepEqual(routesOf(m32), allRoutes) {
		t.Errorf("m3.2 = %+v", m32)
	}
}

func TestRefreshErrorsKeepStaleSnapshot(t *testing.T) {
	cases := []struct {
		name     string
		resp     pluginapi.HTTPResponse
		err      error
		maxBytes int64
		wantCat  string
	}{
		{"http 401", pluginapi.HTTPResponse{StatusCode: 401, Body: []byte("{}")}, nil, 1 << 20, "http 401"},
		{"http 500", pluginapi.HTTPResponse{StatusCode: 500}, nil, 1 << 20, "http 500"},
		{"base_resp failure", pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"base_resp":{"status_code":1004,"status_msg":"invalid api key"}}`)}, nil, 1 << 20, "upstream status 1004"},
		{"malformed json", pluginapi.HTTPResponse{StatusCode: 200, Body: []byte("{not json")}, nil, 1 << 20, "invalid json"},
		{"malformed data", pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"data":{"id":"x"}}`)}, nil, 1 << 20, "invalid json"},
		{"transport error", pluginapi.HTTPResponse{}, errors.New("dial tcp: refused"), 1 << 20, "network error"},
		{"oversized body", pluginapi.HTTPResponse{StatusCode: 200, Body: make([]byte, catalogBudgetFloor+1)}, nil, 64, "response exceeds max-response-bytes"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := testCfg()
			cfg.MaxResponseBytes = c.maxBytes
			fc := &fakeClient{resp: listing("MiniMax-M2.7")}
			m := New(cfg, fc)
			mustRefresh(t, m)

			fc.set(c.resp, c.err)
			err := m.Refresh(context.Background(), testBase, testKey)
			if err == nil || !strings.Contains(err.Error(), c.wantCat) {
				t.Fatalf("error %v missing category %q", err, c.wantCat)
			}
			if strings.Contains(err.Error(), testKey) {
				t.Errorf("error leaks api key: %q", err)
			}
			if got := ids(m.Models()); !reflect.DeepEqual(got, []string{"MiniMax-M2.7", "MiniMax-M3.1-Flash", "MiniMax-M3"}) {
				t.Errorf("stale snapshot lost: %v", got)
			}
		})
	}
}

func TestRefreshErrorWithoutStaleKeepsBuiltins(t *testing.T) {
	cfg := testCfg()
	cfg.Catalog.StaleWhileUnavailable = false
	declare(&cfg, "custom-model", proto("chat-completions", ""))
	fc := &fakeClient{resp: listing("MiniMax-M2.7")}
	m := New(cfg, fc)
	mustRefresh(t, m)

	fc.set(pluginapi.HTTPResponse{StatusCode: 503}, nil)
	if err := m.Refresh(context.Background(), testBase, testKey); err == nil || !strings.Contains(err.Error(), "http 503") {
		t.Fatalf("error = %v", err)
	}
	if got := ids(m.Models()); !reflect.DeepEqual(got, []string{"MiniMax-M3.1-Flash", "MiniMax-M3", "custom-model"}) {
		t.Fatalf("fail-closed must drop only discovered models: %v", got)
	}
	if _, ok := m.Lookup("minimax/MiniMax-M2.7"); ok {
		t.Error("dropped model still in lookup index")
	}

	fc.set(listing("MiniMax-M2.5"), nil)
	mustRefresh(t, m)
	if _, ok := m.Lookup("MiniMax-M2.5"); !ok {
		t.Errorf("recovery failed: %v", ids(m.Models()))
	}
}

func TestRefreshNilClientClassifiedError(t *testing.T) {
	m := New(testCfg(), nil)
	err := m.Refresh(context.Background(), testBase, testKey)
	if err == nil || !strings.Contains(err.Error(), "host client unavailable") {
		t.Fatalf("nil client must yield classified error, got %v", err)
	}
	if got := ids(m.Models()); len(got) != 2 {
		t.Fatalf("nil-client refresh must keep only builtins: %v", got)
	}
}

// An undersized max-response-bytes must not make every refresh fail when the
// listing is larger than the knob; bodies above the floor stay rejected.
func TestFetchBudgetFloor(t *testing.T) {
	body := `{"data":[` + strings.Repeat(`{"id":"speech-2.6-hd"},`, 2048) + `{"id":"MiniMax-M2.7"}]}`
	cfg := testCfg()
	cfg.MaxResponseBytes = 1024
	fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body)}}
	m := New(cfg, fc)
	mustRefresh(t, m)
	if _, ok := m.Lookup("MiniMax-M2.7"); !ok {
		t.Fatalf("undersized max-response-bytes broke refresh: %v", ids(m.Models()))
	}
	fc.set(pluginapi.HTTPResponse{StatusCode: 200, Body: make([]byte, catalogBudgetFloor+1)}, nil)
	if err := m.Refresh(context.Background(), testBase, testKey); err == nil || !strings.Contains(err.Error(), "response exceeds max-response-bytes") {
		t.Fatalf("oversized body (>floor) accepted: err=%v", err)
	}
}

func TestRefreshDataKeyAbsentVsEmpty(t *testing.T) {
	fc := &fakeClient{resp: listing("MiniMax-M2.7")}
	m := New(testCfg(), fc)
	mustRefresh(t, m)

	const driftWarn = `upstream catalog response missing "data" field`
	fc.set(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"object":"list"}`)}, nil)
	mustRefresh(t, m)
	if _, ok := m.Lookup("MiniMax-M2.7"); ok {
		t.Fatal("absent data key kept stale models")
	}
	if w := m.Warnings(); !reflect.DeepEqual(w, []string{driftWarn}) {
		t.Fatalf("warnings = %v, want %q", w, driftWarn)
	}

	fc.set(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"data":[],"base_resp":{"status_code":0}}`)}, nil)
	mustRefresh(t, m)
	if w := m.Warnings(); len(w) != 0 {
		t.Fatalf("intended clear must stay silent, got %v", w)
	}
	if got := ids(m.Models()); len(got) != 2 {
		t.Fatalf("builtins lost: %v", got)
	}
}

func TestDedupFirstWins(t *testing.T) {
	fc := &fakeClient{resp: listing("MiniMax-M2.7", "MiniMax-M2.7")}
	m := New(testCfg(), fc)
	mustRefresh(t, m)
	if got := ids(m.Models()); !reflect.DeepEqual(got, []string{"MiniMax-M2.7", "MiniMax-M3.1-Flash", "MiniMax-M3"}) {
		t.Fatalf("models = %v", got)
	}
	if w := m.Warnings(); len(w) != 1 || !strings.Contains(w[0], `duplicate model "MiniMax-M2.7"`) {
		t.Fatalf("warnings = %v", w)
	}
}

func TestMissingIDDiagnosed(t *testing.T) {
	fc := &fakeClient{resp: pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(`{"data":[{"object":"model"}]}`)}}
	m := New(testCfg(), fc)
	mustRefresh(t, m)
	if u := m.Unsupported(); len(u) != 1 || u[0].Reason != "missing id" {
		t.Fatalf("unsupported = %+v", u)
	}
}

func TestLookupByPrefixedBareAndMiss(t *testing.T) {
	m := New(testCfg(), nil)
	for _, id := range []string{"minimax/MiniMax-M3", "MiniMax-M3"} {
		if rec, ok := m.Lookup(id); !ok || rec.UpstreamID != "MiniMax-M3" {
			t.Errorf("Lookup(%q) = %+v, %v", id, rec, ok)
		}
	}
	for _, id := range []string{"minimax-m3", "other/MiniMax-M3", ""} {
		if _, ok := m.Lookup(id); ok {
			t.Errorf("Lookup(%q) matched", id)
		}
	}
}

func TestPublicIDWithoutPrefix(t *testing.T) {
	cfg := testCfg()
	cfg.ModelPrefix.Enabled = false
	m := New(cfg, nil)
	rec := findModel(t, m.Models(), "MiniMax-M3")
	if rec.PublicID != "MiniMax-M3" {
		t.Fatalf("PublicID = %q", rec.PublicID)
	}
}

func TestSeedFromRebuildsAgainstNewConfig(t *testing.T) {
	fc := &fakeClient{resp: listing("MiniMax-M2.7")}
	old := New(testCfg(), fc)
	mustRefresh(t, old)

	cfg := testCfg()
	cfg.ModelPrefix.Value = "mm"
	declare(&cfg, "MiniMax-M2.7", off("responses"))
	next := New(cfg, fc)
	next.SeedFrom(old)
	rec := findModel(t, next.Models(), "MiniMax-M2.7")
	if rec.PublicID != "mm/MiniMax-M2.7" || !reflect.DeepEqual(routesOf(rec), []Route{RouteMessages, RouteChatCompletions}) {
		t.Fatalf("seeded record = %+v", rec)
	}
	if rec.UserDefined {
		t.Error("seeded discovered model marked user-defined")
	}
}

func TestModelsReturnCopies(t *testing.T) {
	cfg := testCfg()
	declare(&cfg, "bad", off("messages"))
	m := New(cfg, nil)
	models := m.Models()
	models[0].UpstreamID = "mutated"
	if m.Models()[0].UpstreamID == "mutated" {
		t.Error("Models not returning a copy")
	}
	unsup := m.Unsupported()
	unsup[0].Reason = "mutated"
	if m.Unsupported()[0].Reason == "mutated" {
		t.Error("Unsupported not returning a copy")
	}
}

func TestConcurrentRefreshAndReads(t *testing.T) {
	fc := &fakeClient{}
	m := New(testCfg(), fc)
	var writerWG, readerWG sync.WaitGroup
	stop := make(chan struct{})
	for r := 0; r < 3; r++ {
		writerWG.Add(1)
		go func(r int) {
			defer writerWG.Done()
			for i := 0; i < 50; i++ {
				if i%10 == 0 {
					fc.set(pluginapi.HTTPResponse{StatusCode: 500}, nil)
				} else {
					fc.set(listing(fmt.Sprintf("MiniMax-M%d", r)), nil)
				}
				_ = m.Refresh(context.Background(), testBase, testKey)
			}
		}(r)
	}
	for r := 0; r < 3; r++ {
		readerWG.Add(1)
		go func() {
			defer readerWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = m.Models()
					_, _ = m.Lookup("MiniMax-M3")
					_ = m.Unsupported()
					_ = m.Warnings()
				}
			}
		}()
	}
	writerWG.Wait()
	close(stop)
	readerWG.Wait()
}

func TestProtocolSwitchesAndEndpoints(t *testing.T) {
	cfg := testCfg()
	cfg.Protocols = map[string]config.ProtocolOverride{"responses": {Endpoint: "/v1/global-responses"}}
	declare(&cfg, "MiniMax-M3", off("chat-completions"), proto("messages", "/anthropic/v1/custom"))
	declare(&cfg, "MiniMax-M3.1-Flash", off("messages"), off("chat-completions"), off("responses"))
	m := New(cfg, nil)

	m3 := findModel(t, m.Models(), "MiniMax-M3")
	want := []Target{{RouteMessages, "/anthropic/v1/custom"}, {RouteResponses, "/v1/global-responses"}}
	if !reflect.DeepEqual(m3.Routes, want) {
		t.Fatalf("m3 routes = %+v", m3.Routes)
	}
	if m3.UserDefined {
		t.Error("overridden builtin marked user-defined")
	}
	if _, ok := m.Lookup("MiniMax-M3.1-Flash"); ok {
		t.Fatal("fully disabled model still routable")
	}
	u := m.Unsupported()
	if len(u) != 1 || u[0].UpstreamID != "MiniMax-M3.1-Flash" || u[0].Reason != "protocols messages, chat-completions, responses disabled by config" {
		t.Fatalf("unsupported = %+v", u)
	}
	var found bool
	for _, w := range m.Warnings() {
		found = found || w == `model "MiniMax-M3" routed via user configuration (messages /anthropic/v1/custom, responses /v1/global-responses)`
	}
	if !found {
		t.Fatalf("warnings = %v", m.Warnings())
	}
}

func TestDeclaredModels(t *testing.T) {
	cfg := testCfg()
	declare(&cfg, "custom-chat", proto("chat-completions", ""), proto("responses", ""))
	declare(&cfg, "no-route")
	display, ctx := "Custom M3", int64(123)
	cfg.Models = append(cfg.Models, config.ModelOverride{Name: "MiniMax-M3"})
	cfg.Models[len(cfg.Models)-1].DisplayName = &display
	cfg.Models[len(cfg.Models)-1].Context = &ctx
	cfg.Models = append(cfg.Models, config.ModelOverride{Name: "MiniMax-M2.5"})
	m := New(cfg, nil)

	custom := findModel(t, m.Models(), "custom-chat")
	if !custom.UserDefined || !reflect.DeepEqual(routesOf(custom), []Route{RouteChatCompletions, RouteResponses}) || custom.DisplayName != "custom-chat" {
		t.Errorf("custom = %+v", custom)
	}
	m3 := findModel(t, m.Models(), "MiniMax-M3")
	if m3.DisplayName != display || m3.ContextLimit != ctx || m3.OutputLimit != 524_288 || m3.Thinking == nil {
		t.Errorf("override merge = %+v", m3)
	}
	// A declared retiring model is published with its built-in metadata.
	m25 := findModel(t, m.Models(), "MiniMax-M2.5")
	if !m25.UserDefined || m25.DisplayName != "MiniMax M2.5" {
		t.Errorf("m2.5 = %+v", m25)
	}
	if u := m.Unsupported(); len(u) != 1 || u[0].UpstreamID != "no-route" || u[0].Reason != "no route determined" {
		t.Errorf("unsupported = %+v", u)
	}
}

func TestThinkingOverrideMergesWithBuiltin(t *testing.T) {
	cfg := testCfg()
	zero := false
	cfg.Models = []config.ModelOverride{{Name: "MiniMax-M3"}}
	cfg.Models[0].Thinking = &modelmeta.Thinking{ZeroAllowed: &zero}
	m := New(cfg, nil)
	rec := findModel(t, m.Models(), "MiniMax-M3")
	if rec.Thinking == nil || rec.Thinking.ZeroAllowed || len(rec.Thinking.Levels) != 6 {
		t.Fatalf("thinking = %+v", rec.Thinking)
	}
}

func TestDeclaredModelsSurviveDiscoveryFailure(t *testing.T) {
	cfg := testCfg()
	declare(&cfg, "custom-model", proto("messages", ""))
	fc := &fakeClient{resp: listing("custom-model")}
	m := New(cfg, fc)
	mustRefresh(t, m)
	if rec := findModel(t, m.Models(), "custom-model"); rec.UserDefined {
		t.Error("discovered declaration marked user-defined")
	}
	fc.set(pluginapi.HTTPResponse{}, errors.New("down"))
	cfg.Catalog.StaleWhileUnavailable = false
	m2 := New(cfg, fc)
	_ = m2.Refresh(context.Background(), testBase, testKey)
	if rec := findModel(t, m2.Models(), "custom-model"); !rec.UserDefined {
		t.Error("declaration must stay available as user-defined")
	}
}

func TestSwapPublicUpstreamCrossKeyCollision(t *testing.T) {
	for _, order := range [][]string{{"MiniMax-M9", "p/MiniMax-M9"}, {"p/MiniMax-M9", "MiniMax-M9"}} {
		cfg := testCfg()
		cfg.ModelPrefix.Value = "p"
		declare(&cfg, "p/MiniMax-M9", proto("messages", ""))
		fc := &fakeClient{resp: listing(order...)}
		m := New(cfg, fc)
		mustRefresh(t, m)
		models := m.Models()
		var kept []string
		for _, id := range ids(models) {
			if strings.Contains(id, "M9") {
				kept = append(kept, id)
			}
		}
		if !reflect.DeepEqual(kept, order[:1]) {
			t.Fatalf("order %v: kept %v", order, kept)
		}
		u := m.Unsupported()
		if len(u) != 1 || u[0].UpstreamID != order[1] || !strings.Contains(u[0].Reason, "collides") {
			t.Fatalf("order %v: unsupported = %+v", order, u)
		}
	}
}

func TestJoinUpstreamURL(t *testing.T) {
	for _, c := range []struct{ base, endpoint, want string }{
		{"https://api.minimax.io", "/v1/models", "https://api.minimax.io/v1/models"},
		{"https://api.minimax.io/", "/anthropic/v1/messages", "https://api.minimax.io/anthropic/v1/messages"},
		{"https://proxy.example.com/minimax", "/v1/responses", "https://proxy.example.com/minimax/v1/responses"},
	} {
		if got := JoinUpstreamURL(c.base, c.endpoint); got != c.want {
			t.Errorf("JoinUpstreamURL(%q, %q) = %q", c.base, c.endpoint, got)
		}
		if got := (Target{RouteMessages, c.endpoint}).URL(c.base); got != c.want {
			t.Errorf("Target.URL = %q", got)
		}
	}
}

// Joining a base URL with an endpoint must never move the request off the
// configured host/scheme, and parse failures fail closed.
func TestEndpointEscapesBase(t *testing.T) {
	cases := []struct {
		base     string
		endpoint string
		want     bool
	}{
		{"https://api.minimax.io", "@evil.com/x", true},
		{"https://api.minimax.io", "/v1@evil.com/x", false}, // @ lands in path
		{"https://api.minimax.io", "/v1/chat/completions", false},
		{"https://api.minimax.io/", "/v1/responses", false},
		{"https://API.minimax.io", "/v1/chat/completions", false},
		{"https://api.minimax.io", ".evil.com", true},
		{"https://api.minimax.io", ":8443/x", true}, // a port change is another authority
		{"https://api.minimax.io", "\x7f", true},
		{"https://api.minimax.io\x00", "/x", true},
	}
	for _, c := range cases {
		if got := endpointEscapesBase(c.base, Target{RouteChatCompletions, c.endpoint}); got != c.want {
			t.Errorf("endpointEscapesBase(%q, %q) = %v, want %v", c.base, c.endpoint, got, c.want)
		}
	}
}

func TestEndpointEscapeCheckedAgainstAllKeys(t *testing.T) {
	const escapeReason = "resolved endpoint escapes the configured upstream host"
	for _, bareRegion := range []string{config.RegionGlobal, config.RegionCN} {
		t.Run(bareRegion, func(t *testing.T) {
			cfg := testCfg()
			// The shared host has a path that absorbs "@..."; a key override
			// with a bare host must still exclude authority-rewriting endpoints.
			cfg.BaseURL = "https://proxy.example.com/minimax"
			cfg.APIKeys = []config.APIKey{{Region: bareRegion, BaseURL: "https://api.minimax.test"}}
			declare(&cfg, "evil", proto("chat-completions", "@evil.com/x"))
			declare(&cfg, "mixed", proto("chat-completions", "@evil.com/y"), proto("messages", ""))
			m := New(cfg, nil)
			if _, ok := m.Lookup("evil"); ok {
				t.Fatal("authority-rewriting endpoint still routable")
			}
			mixed := findModel(t, m.Models(), "mixed")
			if !reflect.DeepEqual(mixed.Routes, []Target{{RouteMessages, "/anthropic/v1/messages"}}) {
				t.Fatalf("mixed routes = %+v", mixed.Routes)
			}
			for _, mo := range m.Models() {
				for _, target := range mo.Routes {
					for _, key := range append([]config.APIKey{{Region: config.RegionGlobal}, {Region: config.RegionCN}}, cfg.APIKeys...) {
						base := cfg.UpstreamBaseURL(key)
						p, err := url.Parse(target.URL(base))
						b, _ := url.Parse(base)
						if err != nil || !strings.EqualFold(p.Host, b.Host) {
							t.Fatalf("routable %s escapes %s: %v", mo.UpstreamID, base, err)
						}
					}
				}
			}
			u := m.Unsupported()
			if len(u) != 1 || u[0].UpstreamID != "evil" || u[0].Reason != escapeReason {
				t.Fatalf("unsupported = %+v", u)
			}
			var found bool
			for _, w := range m.Warnings() {
				found = found || w == `model "mixed" chat-completions: `+escapeReason
			}
			if !found {
				t.Fatalf("warnings = %v", m.Warnings())
			}
		})
	}
}

func TestRecordTargetAndSupports(t *testing.T) {
	rec := findModel(t, New(testCfg(), nil).Models(), "MiniMax-M3")
	if tgt, ok := rec.Target(RouteResponses); !ok || tgt.EndpointPath != "/v1/responses" {
		t.Errorf("Target(responses) = %+v, %v", tgt, ok)
	}
	if rec.Supports(Route("gemini")) {
		t.Error("gemini is not an upstream route")
	}
}
