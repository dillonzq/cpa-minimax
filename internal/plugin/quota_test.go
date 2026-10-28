package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/dillonzq/cpa-minimax/internal/config"
)

// remainsBody is the observed /v1/token_plan/remains shape: a 4h interval
// window and a weekly window, times in Unix milliseconds.
const remainsBody = `{"model_remains":[{"model_name":"MiniMax-M*","start_time":1778616000000,"end_time":1778630400000,` +
	`"remains_time":12241883,"current_interval_total_count":1500,"current_interval_usage_count":1200,` +
	`"current_weekly_total_count":15000,"current_weekly_usage_count":3000,"weekly_start_time":1778457600000,` +
	`"weekly_end_time":1779062400000,"weekly_remains_time":444241883}],"base_resp":{"status_code":0,"status_msg":"success"}}`

func loadQuotaConfig(t *testing.T, yamlText string) config.Config {
	t.Helper()
	cfg, err := config.Load([]byte(yamlText))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// quotaManager answers host.http.do by URL; unknown URLs return 404.
func quotaManager(t *testing.T, cfg config.Config, byURL map[string]pluginapi.HTTPResponse) (*Manager, *fakeCaller) {
	t.Helper()
	f := &fakeCaller{responder: func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return hostOK(map[string]any{}), nil
		}
		var req pluginapi.HTTPRequest
		_ = json.Unmarshal(payload, &req)
		if resp, ok := byURL[req.URL]; ok {
			return hostOK(resp), nil
		}
		return hostOK(pluginapi.HTTPResponse{StatusCode: http.StatusNotFound, Body: []byte(`{"error":"not found"}`)}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	m.cfg = cfg
	return m, f
}

func fetchQuota(t *testing.T, m *Manager, attrs map[string]string) (pluginapi.QuotaFetchResponse, error) {
	t.Helper()
	return m.FetchQuota(context.Background(), pluginapi.QuotaFetchRequest{Provider: ProviderID, Attributes: attrs})
}

func requestedURLs(f *fakeCaller) []string {
	var urls []string
	for _, c := range f.callsOf(pluginabi.MethodHostHTTPDo) {
		var req pluginapi.HTTPRequest
		_ = json.Unmarshal(c.payload, &req)
		urls = append(urls, req.URL)
	}
	return urls
}

func TestQuotaRegistration(t *testing.T) {
	m := NewManager(nil)
	var registration registrationResult
	decodeResult(t, registrationEnvelope(), &registration)
	if !registration.Capabilities.QuotaProvider || registration.Capabilities.ManagementAPI {
		t.Fatal("incorrect quota capabilities")
	}
	var desc pluginapi.QuotaDescribeResponse
	decodeResult(t, mustHandle(t, m, pluginabi.MethodQuotaDescribe, []byte(`{}`)), &desc)
	if len(desc.SupportedProviders) != 1 || desc.SupportedProviders[0] != ProviderID || desc.SupportsReset || desc.DisplayName != "MiniMax" {
		t.Fatalf("incorrect description: %+v", desc)
	}
	for _, method := range []string{pluginabi.MethodManagementRegister, pluginabi.MethodManagementHandle, pluginabi.MethodQuotaReset} {
		var env pluginabi.Envelope
		json.Unmarshal(mustHandle(t, m, method, []byte(`{}`)), &env)
		if env.OK {
			t.Fatalf("obsolete/unsupported operation succeeded: %s", method)
		}
	}
}

func TestQuotaSubscriptionUsesSelectedCredentialAndRegion(t *testing.T) {
	cfg := loadQuotaConfig(t, "api-keys:\n  - value: sk-cp-not-selected\n")
	m, f := quotaManager(t, cfg, map[string]pluginapi.HTTPResponse{
		"https://api.minimax.cn/v1/token_plan/remains": {StatusCode: 200, Body: []byte(remainsBody)},
	})
	body, _ := json.Marshal(pluginapi.QuotaFetchRequest{Provider: ProviderID, Attributes: map[string]string{"api_key": "sk-cp-selected", "region": "cn", "key_type": "subscription"}})
	var got pluginapi.QuotaFetchResponse
	decodeResult(t, mustHandle(t, m, pluginabi.MethodQuotaFetch, body), &got)
	calls := f.callsOf(pluginabi.MethodHostHTTPDo)
	var req pluginapi.HTTPRequest
	_ = json.Unmarshal(calls[0].payload, &req)
	if len(calls) != 1 || req.Headers.Get("Authorization") != "Bearer sk-cp-selected" {
		t.Fatalf("wrong credential: %v", req.Headers)
	}
	if got.Subscription == nil || got.Subscription.Plan != "M Plan" || got.Subscription.TierName != "China" {
		t.Fatalf("subscription = %+v", got.Subscription)
	}
	if len(got.Groups) != 1 || got.Groups[0].DisplayName != "MiniMax-M*" || len(got.Groups[0].Buckets) != 2 {
		t.Fatalf("groups = %+v", got.Groups)
	}
	rolling, weekly := got.Groups[0].Buckets[0], got.Groups[0].Buckets[1]
	// The default usage-count semantics read *_usage_count as remaining.
	if rolling.Window != "rolling" || rolling.RemainingFraction != 0.8 || rolling.ResetTime != time.UnixMilli(1778630400000).UTC().Format(time.RFC3339Nano) {
		t.Fatalf("rolling = %+v", rolling)
	}
	if !strings.Contains(rolling.Description, "1200 of 1500") || !strings.Contains(rolling.Description, "4h window") {
		t.Fatalf("rolling description = %q", rolling.Description)
	}
	if weekly.Window != "weekly" || weekly.RemainingFraction != 0.2 || weekly.ResetTime != time.UnixMilli(1779062400000).UTC().Format(time.RFC3339Nano) {
		t.Fatalf("weekly = %+v", weekly)
	}
}

func TestQuotaUsageCountUsedSemantics(t *testing.T) {
	got, err := normalizeQuota([]byte(remainsBody), config.RegionGlobal, config.UsageCountUsed, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b := got.Groups[0].Buckets
	if got.Subscription.TierName != "Global" || b[0].RemainingFraction != 0.2 || b[1].RemainingFraction != 0.8 {
		t.Fatalf("used semantics = %+v", got)
	}
}

func TestQuotaWindowsOmittedAndValidated(t *testing.T) {
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	// A weekly total of 0 means no weekly limit; remains_time backs a missing end_time.
	got, err := normalizeQuota([]byte(`{"model_remains":[{"model_name":"MiniMax-M*","current_interval_total_count":100,"current_interval_usage_count":25,"remains_time":60000,"current_weekly_total_count":0,"current_weekly_usage_count":0}]}`), config.RegionGlobal, config.UsageCountRemaining, now)
	if err != nil || len(got.Groups) != 1 || len(got.Groups[0].Buckets) != 1 {
		t.Fatalf("weekly zero not omitted: %+v %v", got, err)
	}
	if b := got.Groups[0].Buckets[0]; b.RemainingFraction != 0.25 || b.ResetTime != now.Add(time.Minute).Format(time.RFC3339Nano) {
		t.Fatalf("bucket = %+v", b)
	}
	for _, body := range []string{
		`{}`, `not json`, `{"model_remains":[]}`,
		`{"model_remains":[{"current_interval_total_count":0,"current_interval_usage_count":0}]}`,
		`{"model_remains":[{"current_interval_total_count":10,"current_interval_usage_count":11}]}`,
		`{"model_remains":[{"current_interval_total_count":10,"current_interval_usage_count":-1}]}`,
		`{"model_remains":[{"current_interval_total_count":"10","current_interval_usage_count":1}]}`,
	} {
		if _, err := normalizeQuota([]byte(body), config.RegionGlobal, config.UsageCountRemaining, now); err == nil {
			t.Fatalf("invalid response accepted: %s", body)
		}
	}
}

func TestQuotaFallsBackToLegacyEndpoint(t *testing.T) {
	cfg := loadQuotaConfig(t, "api-keys:\n  - value: sk-cp-plan\n")
	m, f := quotaManager(t, cfg, map[string]pluginapi.HTTPResponse{
		"https://api.minimax.io/v1/api/openplatform/coding_plan/remains": {StatusCode: 200, Body: []byte(remainsBody)},
	})
	got, err := fetchQuota(t, m, map[string]string{"api_key": "sk-cp-plan"})
	if err != nil || len(got.Groups) != 1 {
		t.Fatalf("legacy fallback failed: %+v %v", got, err)
	}
	urls := requestedURLs(f)
	if len(urls) != 2 || urls[0] != "https://api.minimax.io/v1/token_plan/remains" || urls[1] != "https://api.minimax.io/v1/api/openplatform/coding_plan/remains" {
		t.Fatalf("endpoint order = %v", urls)
	}
}

func TestQuotaBaseRespAuthErrorStops(t *testing.T) {
	cfg := loadQuotaConfig(t, "api-keys:\n  - value: sk-cp-plan\n")
	m, f := quotaManager(t, cfg, map[string]pluginapi.HTTPResponse{
		"https://api.minimax.io/v1/token_plan/remains": {StatusCode: 200, Body: []byte(`{"base_resp":{"status_code":1004,"status_msg":"login fail"}}`)},
	})
	if _, err := fetchQuota(t, m, map[string]string{"api_key": "sk-cp-plan"}); err == nil || !strings.Contains(err.Error(), "1004") {
		t.Fatalf("base_resp 1004 accepted: %v", err)
	}
	if len(requestedURLs(f)) != 1 {
		t.Fatal("auth failure retried another endpoint")
	}
}

func TestQuotaAPIKeyReportsPlanWithoutUpstreamCall(t *testing.T) {
	cfg := loadQuotaConfig(t, "region: cn\napi-keys:\n  - value: sk-api-key\n")
	m, f := quotaManager(t, cfg, nil)
	got, err := fetchQuota(t, m, map[string]string{"api_key": "sk-api-key"})
	if err != nil || got.Subscription == nil || got.Subscription.Plan != "Pay-as-you-go" || got.Subscription.TierName != "China" || len(got.Groups) != 0 {
		t.Fatalf("api quota = %+v %v", got, err)
	}
	if len(requestedURLs(f)) != 0 {
		t.Fatal("api key quota called upstream")
	}
}

func TestQuotaRejectsInvalidCredentials(t *testing.T) {
	m, _ := quotaManager(t, loadQuotaConfig(t, "api-keys:\n  - value: sk-cp-plan\n"), nil)
	for _, req := range []pluginapi.QuotaFetchRequest{
		{Provider: "other", Attributes: map[string]string{"api_key": "selected"}},
		{Provider: ProviderID},
		{Provider: ProviderID, Attributes: map[string]string{"api_key": "selected", "region": "mars"}},
	} {
		if _, err := m.FetchQuota(context.Background(), req); err == nil {
			t.Fatalf("invalid credential accepted: %+v", req)
		}
	}
}

func TestQuotaErrorsRedacted(t *testing.T) {
	cfg := loadQuotaConfig(t, "api-keys:\n  - value: sk-cp-secret\n")
	for _, response := range []pluginapi.HTTPResponse{{StatusCode: 401, Body: []byte("sk-cp-secret")}, {StatusCode: http.StatusOK, Body: []byte("sk-cp-secret")}} {
		m, _ := quotaManager(t, cfg, map[string]pluginapi.HTTPResponse{"https://api.minimax.io/v1/token_plan/remains": response})
		_, err := fetchQuota(t, m, map[string]string{"api_key": "sk-cp-secret"})
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unredacted or missing error: %v", err)
		}
	}
}

func TestReadableAuthFilenameAtColdStartDoesNotDuplicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Personal.json")
	raw := []byte(`{"type":"minimax","api_key":"existing","label":"Personal","disabled":true}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	f := &fakeCaller{responder: func(method string, _ []byte) ([]byte, error) {
		if method == pluginabi.MethodHostAuthList {
			return hostOK(hostAuthListResponse{Files: []pluginapi.HostAuthFileEntry{{Name: "Personal.json", Provider: ProviderID, Path: path}}}), nil
		}
		return hostOK(map[string]any{}), nil
	}}
	m := NewManager(NewHostBridge(f.call))
	if err := m.materializeAuthRecords(context.Background(), config.Config{APIKeys: []config.APIKey{{Value: "existing"}}}); err != nil {
		t.Fatal(err)
	}
	if len(f.callsOf(pluginabi.MethodHostAuthSave)) != 0 {
		t.Fatal("duplicated existing credential")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(raw) {
		t.Fatal("modified host metadata")
	}
}

func TestQuotaBaseURLOverrides(t *testing.T) {
	for _, tc := range []struct{ name, yaml, want string }{
		{"shared override", "base-url: https://shared.test\napi-keys:\n  - value: sk-cp-selected\n    region: cn\n", "https://shared.test"},
		{"key override", "base-url: https://shared.test\napi-keys:\n  - value: sk-cp-selected\n    base-url: https://key.test/anthropic/v1\n", "https://key.test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := loadQuotaConfig(t, tc.yaml)
			want := tc.want + "/v1/token_plan/remains"
			m, f := quotaManager(t, cfg, map[string]pluginapi.HTTPResponse{want: {StatusCode: 200, Body: []byte(remainsBody)}})
			if _, err := fetchQuota(t, m, map[string]string{"api_key": "sk-cp-selected", "base_url": "https://stale.test"}); err != nil {
				t.Fatal(err)
			}
			if got := requestedURLs(f); len(got) != 1 || got[0] != want {
				t.Fatalf("requests = %v, want %s", got, want)
			}
		})
	}
}
