package plugin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/dillonzq/cpa-minimax/internal/config"
)

func mustLoad(t *testing.T, yamlText string) config.Config {
	t.Helper()
	cfg, err := config.Load([]byte(yamlText))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

func parseRecord(t *testing.T, cfg config.Config, record string) (pluginapi.AuthParseResponse, error) {
	t.Helper()
	return (authProvider{cfg: cfg}).ParseAuth(context.Background(), pluginapi.AuthParseRequest{Provider: ProviderID, FileName: "key.json", RawJSON: []byte(record)})
}

func TestAuthParseCredentialAttributes(t *testing.T) {
	cfg := mustLoad(t, "region: global\napi-keys:\n"+
		"  - value: configured-plan\n    type: subscription\n    region: cn\n    priority: 9\n"+
		"  - value: sk-cp-configured\n")
	cases := []struct {
		name, record                       string
		wantRegion, wantType, wantPriority string
	}{
		{"config wins over record", `{"type":"minimax","api_key":"configured-plan","region":"global","key_type":"api"}`, "cn", "subscription", "9"},
		{"configured sk-cp- auto-detects", `{"type":"minimax","api_key":"sk-cp-configured"}`, "global", "subscription", "1"},
		{"record fields without config", `{"type":"minimax","api_key":"file-only","region":"china","key_type":"payg"}`, "cn", "api", "0"},
		{"unconfigured sk-cp- defaults", `{"type":"minimax","api_key":"sk-cp-file"}`, "global", "subscription", "1"},
		{"unconfigured api defaults", `{"type":"minimax","api_key":"sk-api-file"}`, "global", "api", "0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := parseRecord(t, cfg, tc.record)
			if err != nil || !parsed.Handled {
				t.Fatalf("parse = %+v, %v", parsed, err)
			}
			a := parsed.Auth.Attributes
			if a["region"] != tc.wantRegion || a["key_type"] != tc.wantType || a["priority"] != tc.wantPriority {
				t.Fatalf("attributes = %v", a)
			}
		})
	}
	// The plugin default region applies to records without one.
	parsed, err := parseRecord(t, mustLoad(t, "region: cn\napi-keys:\n  - value: other\n"), `{"type":"minimax","api_key":"file-only"}`)
	if err != nil || parsed.Auth.Attributes["region"] != "cn" {
		t.Fatalf("default region = %v, %v", parsed.Auth.Attributes, err)
	}
	for _, record := range []string{
		`{"type":"minimax","api_key":"sk-x","region":"mars"}`,
		`{"type":"minimax","api_key":"sk-x","key_type":"enterprise"}`,
		`{"type":"minimax","api_key":"sk-x","base_url":"https://user:secret@host"}`,
		`{"type":"minimax","api_key":"sk-x","base_url":"http://host"}`,
	} {
		if parsed, err := parseRecord(t, cfg, record); err == nil || parsed.Handled {
			t.Fatalf("invalid record accepted: %s", record)
		}
	}
}

func TestAccountLabelDefaults(t *testing.T) {
	cfg := mustLoad(t, "api-keys:\n"+
		"  - value: sk-cp-one\n"+
		"  - value: sk-cp-two\n"+
		"  - value: api-one\n"+
		"  - value: api-cn\n    region: cn\n"+
		"  - value: named\n    name: Work\n")
	want := map[string]string{
		"sk-cp-one": "MiniMax M Plan 1",
		"sk-cp-two": "MiniMax M Plan 2",
		"api-one":   "MiniMax API",
		"api-cn":    "MiniMax API CN",
		"named":     "Work",
	}
	for key, label := range want {
		entry, _ := cfg.Key(key)
		if got := accountLabel(cfg, key, "", entry); got != label {
			t.Errorf("%s label = %q, want %q", key, got, label)
		}
	}
	entry, _ := cfg.Key("api-one")
	if got := accountLabel(cfg, "api-one", "Existing", entry); got != "Existing" {
		t.Fatalf("existing label lost: %q", got)
	}
	if got := accountLabel(cfg, "named", "Existing", config.APIKey{}); got != "Work" {
		t.Fatalf("configured name lost precedence: %q", got)
	}
	if got := accountLabel(config.Config{}, "x", "", config.APIKey{Type: config.KeyTypeSubscription, Region: config.RegionCN}); got != "MiniMax M Plan CN" {
		t.Fatalf("unconfigured label = %q", got)
	}
}

func TestMaterializedRecordCarriesRegionTypeAndBaseURL(t *testing.T) {
	cfg := mustLoad(t, "api-keys:\n  - value: sk-cp-plan\n    region: cn\n    base-url: https://key.example/anthropic/v1\n  - value: sk-api\n")
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	if err := m.materializeAuthRecords(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	saves := f.callsOf(pluginabi.MethodHostAuthSave)
	if len(saves) != 2 {
		t.Fatalf("saves = %d", len(saves))
	}
	want := []authRecord{
		{Type: ProviderID, Label: "MiniMax M Plan CN", APIKey: "sk-cp-plan", Region: "cn", KeyType: "subscription", BaseURL: "https://key.example"},
		{Type: ProviderID, Label: "MiniMax API", APIKey: "sk-api", Region: "global", KeyType: "api"},
	}
	for i, call := range saves {
		var req pluginapi.HostAuthSaveRequest
		_ = json.Unmarshal(call.payload, &req)
		var got authRecord
		if err := json.Unmarshal(req.JSON, &got); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(want[i].APIKey))
		want[i].ID = authIDPrefix + hex.EncodeToString(digest[:])
		if got != want[i] {
			t.Fatalf("record %d = %+v, want %+v", i, got, want[i])
		}
	}
	// The record round-trips through ParseAuth without configuration.
	var req pluginapi.HostAuthSaveRequest
	_ = json.Unmarshal(saves[0].payload, &req)
	parsed, err := (authProvider{}).ParseAuth(context.Background(), pluginapi.AuthParseRequest{Provider: ProviderID, RawJSON: req.JSON})
	if err != nil || parsed.Auth.Attributes["region"] != "cn" || parsed.Auth.Attributes["key_type"] != "subscription" || parsed.Auth.Label != "MiniMax M Plan CN" || parsed.Auth.Attributes["base_url"] != "https://key.example" {
		t.Fatalf("round trip = %+v, %v", parsed.Auth, err)
	}
}

func TestDiscoveryKeyPrefersAPIKey(t *testing.T) {
	cfg := mustLoad(t, "api-keys:\n  - value: sk-cp-plan\n  - value: api-first\n    region: cn\n  - value: api-second\n")
	if got := discoveryKey(cfg); got.Value != "api-first" || got.Region != "cn" {
		t.Fatalf("discovery key = %+v", got)
	}
	cfg = mustLoad(t, "api-keys:\n  - value: sk-cp-plan\n  - value: sk-cp-other\n")
	if got := discoveryKey(cfg); got.Value != "sk-cp-plan" {
		t.Fatalf("subscription-only discovery key = %+v", got)
	}
}
