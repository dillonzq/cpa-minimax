package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/dillonzq/cpa-minimax/internal/catalog"
	"github.com/dillonzq/cpa-minimax/internal/config"
)

const (
	cooledRefreshKey  = "sk-refresh-cooled-key"
	goodRefreshKey    = "sk-refresh-good-key"
	refreshTwoKeyYAML = "api-keys:\n" +
		"  - value: " + cooledRefreshKey + "\n" +
		"  - value: " + goodRefreshKey + "\n" +
		"base-url: https://catalog.test\n"
)

// authRecorder answers host.http.do, records every Authorization header, and
// rejects the cooled first key 401-style while any other key gets a good
// catalog body.
type authRecorder struct {
	mu    sync.Mutex
	auths []string
	urls  []string
	logs  [][]byte
	// body, when non-empty, replaces testCatalogJSON as the served catalog.
	body []byte
}

func (r *authRecorder) call(method string, payload []byte) ([]byte, error) {
	if method == pluginabi.MethodHostHTTPOperationOpen {
		return hostOK(httpOperationRequest{OperationID: "catalog-operation"}), nil
	}

	if method == pluginabi.MethodHostLog {
		r.mu.Lock()
		r.logs = append(r.logs, append([]byte(nil), payload...))
		r.mu.Unlock()
		return hostOK(map[string]any{}), nil
	}
	if method != pluginabi.MethodHostHTTPDo {
		return hostOK(map[string]any{}), nil
	}
	var wire struct {
		URL     string      `json:"url"`
		Headers http.Header `json:"headers"`
	}
	if err := json.Unmarshal(payload, &wire); err != nil {
		return hostErr("test", "undecodable payload"), nil
	}
	auth := wire.Headers.Get("Authorization")
	r.mu.Lock()
	r.auths = append(r.auths, auth)
	r.urls = append(r.urls, wire.URL)
	r.mu.Unlock()
	if auth == "Bearer "+cooledRefreshKey {
		return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusUnauthorized}), nil
	}
	body := testCatalogJSON
	if len(r.body) > 0 {
		body = string(r.body)
	}
	return hostOK(pluginapi.HTTPResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       []byte(body),
	}), nil
}

func (r *authRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.auths...)
}

func (r *authRecorder) logCalls() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.logs...)
}

func twoKeyFixture(t *testing.T) (config.Config, *catalog.Manager, *HostBridge, *authRecorder) {
	t.Helper()
	cfg, err := config.Load([]byte(refreshTwoKeyYAML))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	rec := &authRecorder{}
	bridge := NewHostBridge(rec.call)
	return cfg, catalog.New(cfg, bridge), bridge, rec
}

func discoveredModels(models []catalog.ModelRecord) []catalog.ModelRecord {
	var out []catalog.ModelRecord
	for _, rec := range models {
		if rec.UpstreamID != "MiniMax-M3" && rec.UpstreamID != "MiniMax-M3.1-Flash" {
			out = append(out, rec)
		}
	}
	return out
}

// TestRefreshUsesFirstDiscoveryKey pins catalog refresh to the deterministic
// discovery key: it does not try another configured key when that key fails,
// and a failed refresh still serves the built-in models.
func TestRefreshUsesFirstDiscoveryKey(t *testing.T) {
	cfg, mgr, bridge, rec := twoKeyFixture(t)
	if err := refreshOnce(context.Background(), mgr, bridge, time.Second, cfg); err == nil {
		t.Fatal("expected first configured key failure")
	}
	auths := rec.seen()
	if len(auths) != 1 || auths[0] != "Bearer "+cooledRefreshKey || rec.urls[0] != "https://catalog.test/v1/models" {
		t.Fatalf("refresh auths = %v urls = %v; want one first-key attempt", auths, rec.urls)
	}
	if got := mgr.Models(); len(discoveredModels(got)) != 0 || len(got) != 2 {
		t.Fatalf("failed refresh changed catalog: %+v", got)
	}
}

// TestRefreshPrefersAPIKeyInItsRegion pins discovery to the first API key and
// to that key's regional host.
func TestRefreshPrefersAPIKeyInItsRegion(t *testing.T) {
	for _, tc := range []struct{ name, extra, want string }{
		{"region default", "", "https://api.minimax.cn/v1/models"},
		{"shared override", "base-url: https://shared.test/v1\n", "https://shared.test/v1/models"},
		{"key override", "    base-url: https://key.test/anthropic/v1\nbase-url: https://shared.test\n", "https://key.test/v1/models"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Load([]byte("api-keys:\n  - value: sk-cp-plan\n  - value: " + goodRefreshKey + "\n    region: cn\n" + tc.extra))
			if err != nil {
				t.Fatal(err)
			}
			rec := &authRecorder{body: []byte(`{"object":"list","data":[{"id":"MiniMax-M2.7","object":"model"}]}`)}
			bridge := NewHostBridge(rec.call)
			mgr := catalog.New(cfg, bridge)
			if err := refreshOnce(context.Background(), mgr, bridge, time.Second, cfg); err != nil {
				t.Fatal(err)
			}
			if auths := rec.seen(); len(auths) != 1 || auths[0] != "Bearer "+goodRefreshKey || rec.urls[0] != tc.want {
				t.Fatalf("discovery auths = %v urls = %v", auths, rec.urls)
			}
			if got := discoveredModels(mgr.Models()); len(got) != 1 || got[0].UpstreamID != "MiniMax-M2.7" {
				t.Fatalf("discovered = %+v", got)
			}
		})
	}
}

// TestRefreshUnsupportedLogQuotesNewlineIDs pins F2: a declared model whose
// ID carries control characters must reach the host.log "models" field
// quoted (%q), so no raw newline can forge log lines (CWE-117).
func TestRefreshUnsupportedLogQuotesNewlineIDs(t *testing.T) {
	const evilID = "evil\n2027-01-01 ERROR forged host.log line"
	cfg, err := config.Load([]byte("api-keys:\n  - value: sk-refresh-evil-key\n" +
		"base-url: https://catalog.test\n" +
		"models:\n  - name: \"evil\\n2027-01-01 ERROR forged host.log line\"\n"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	rec := &authRecorder{body: []byte(`{"data":[{"id":"evil\n2027-01-01 ERROR forged host.log line"}]}`)}
	bridge := NewHostBridge(rec.call)
	mgr := catalog.New(cfg, bridge)
	if err := refreshOnce(context.Background(), mgr, bridge, time.Second, cfg); err != nil {
		t.Fatalf("refreshOnce: %v", err)
	}
	logCalls := rec.logCalls()
	if len(logCalls) != 1 {
		t.Fatalf("host.log calls = %d, want exactly one unsupported-models warn", len(logCalls))
	}
	var req struct {
		Level   string         `json:"level"`
		Message string         `json:"message"`
		Fields  map[string]any `json:"fields"`
	}
	if err := json.Unmarshal(logCalls[0], &req); err != nil {
		t.Fatalf("decode host.log payload: %v", err)
	}
	if req.Message != "unsupported models excluded from routable catalog" {
		t.Fatalf("unexpected log message: %q", req.Message)
	}
	modelsField, _ := req.Fields["models"].(string)
	if strings.ContainsRune(modelsField, '\n') {
		t.Fatalf("models log field contains raw newline (log injection): %q", modelsField)
	}
	if want := fmt.Sprintf("%q: ", evilID); !strings.Contains(modelsField, want) {
		t.Fatalf("models field must quote the upstream ID; got %q, want prefix %q", modelsField, want)
	}
}
