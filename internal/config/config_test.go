package config

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func requireErrContains(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err.Error(), want)
	}
}

// withKey satisfies the at-least-one-key rule for fixtures that target a
// later validation check.
const withKey = "api-keys:\n  - value: sk-dummy\n"

func TestModelMetadataOverrides(t *testing.T) {
	c, err := Load([]byte(withKey + `models:
  - name: MiniMax-Test
    display-name: Custom
    max-context-length: 200000
    max-tokens: 0
    input-modalities: []
    output-modalities: [text]
    thinking:
      min: 0
      max: 1000
      zero-allowed: false
      dynamic-allowed: true
      levels: []
`))
	if err != nil {
		t.Fatal(err)
	}
	m := c.Models[0]
	if m.Name != "MiniMax-Test" || m.DisplayName == nil || *m.DisplayName != "Custom" || m.Context == nil || *m.Context != 200000 ||
		m.Output == nil || *m.Output != 0 || m.InputModalities == nil || len(m.InputModalities) != 0 ||
		m.Thinking == nil || m.Thinking.ZeroAllowed == nil || *m.Thinking.ZeroAllowed || m.Thinking.Levels == nil {
		t.Fatalf("CPA override presence lost: %+v", m)
	}
}

func TestModelMetadataValidation(t *testing.T) {
	for _, yaml := range []string{
		"models: [{name: ''}]", "models: [{name: m}, {name: m}]",
		"models: [{name: m, max-context-length: -1}]", "models: [{name: m, thinking: {min: 10, max: 1}}]",
	} {
		if _, err := Load([]byte(withKey + yaml)); err == nil {
			t.Errorf("accepted invalid metadata config %s", yaml)
		}
	}
}

func TestLoadMinimalAppliesAllDefaults(t *testing.T) {
	c, err := Load([]byte(withKey))
	if err != nil {
		t.Fatalf("Load(minimal) failed: %v", err)
	}
	if c.Region != RegionGlobal {
		t.Errorf("Region = %q", c.Region)
	}
	if c.BaseURL != DefaultGlobalBaseURL {
		t.Errorf("BaseURL = %q", c.BaseURL)
	}
	if !c.ModelPrefix.Enabled || c.ModelPrefix.Value != "minimax" {
		t.Errorf("ModelPrefix = %+v", c.ModelPrefix)
	}
	want := []APIKey{{Value: "sk-dummy", Type: KeyTypeAPI, Region: RegionGlobal, Priority: 0}}
	if !reflect.DeepEqual(c.APIKeys, want) {
		t.Errorf("APIKeys = %+v", c.APIKeys)
	}
	if !c.Catalog.Discover || c.Catalog.RefreshInterval != 15*time.Minute || !c.Catalog.StaleWhileUnavailable {
		t.Errorf("Catalog = %+v", c.Catalog)
	}
	if c.Quota.UsageCount != UsageCountRemaining || !reflect.DeepEqual(c.Quota.Endpoints, DefaultQuotaEndpoints) {
		t.Errorf("Quota = %+v", c.Quota)
	}
	if c.Protocols != nil {
		t.Errorf("Protocols = %+v", c.Protocols)
	}
	if c.AllowHTTP {
		t.Errorf("AllowHTTP = true")
	}
	if c.RequestTimeout != 5*time.Minute {
		t.Errorf("RequestTimeout = %v", c.RequestTimeout)
	}
	if c.MaxResponseBytes != 67108864 {
		t.Errorf("MaxResponseBytes = %d", c.MaxResponseBytes)
	}
}

// The default endpoint list must be copied: mutating one config's list must
// never leak into later loads.
func TestLoadDefaultQuotaEndpointsAreCopied(t *testing.T) {
	c, err := Load([]byte(withKey))
	if err != nil {
		t.Fatal(err)
	}
	c.Quota.Endpoints[0] = "/mutated"
	if DefaultQuotaEndpoints[0] != "/v1/token_plan/remains" {
		t.Fatalf("default endpoints mutated: %v", DefaultQuotaEndpoints)
	}
}

func TestLoadFullConfigHonorsEveryField(t *testing.T) {
	t.Setenv("TEST_MM_KEY", "test-secret-value")
	doc := `
enabled: true
future-key: 1
region: cn
base-url: http://localhost:9090/v1
model-prefix:
  enabled: true
  value: myprefix
api-keys:
  - value: ${TEST_MM_KEY}
    name: Work
    base-url: https://api.minimaxi.com/anthropic/
  - value: sk-cp-plan
    region: intl
    priority: 5
catalog:
  discover: false
  refresh-interval: 1h
  stale-while-unavailable: false
protocols:
  responses:
    endpoint: /v1/global-responses
  messages: {}
quota:
  usage-count: USED
  endpoints: [/v1/custom/remains]
models:
  - name: MiniMax-M3
    protocols:
      responses:
        endpoint: /v1/responses
      messages:
        enabled: true
      chat-completions:
        enabled: false
allow-http: true
request-timeout: 5m
max-response-bytes: 1024
`
	c, err := Load([]byte(doc))
	if err != nil {
		t.Fatalf("Load(full) failed: %v", err)
	}
	if c.Region != RegionCN {
		t.Errorf("Region = %q", c.Region)
	}
	if c.BaseURL != "http://localhost:9090" {
		t.Errorf("BaseURL = %q", c.BaseURL)
	}
	if !c.ModelPrefix.Enabled || c.ModelPrefix.Value != "myprefix" {
		t.Errorf("ModelPrefix = %+v", c.ModelPrefix)
	}
	wantKeys := []APIKey{
		{Name: "Work", Value: "test-secret-value", BaseURL: "https://api.minimaxi.com", Type: KeyTypeAPI, Region: RegionCN, Priority: 0},
		{Value: "sk-cp-plan", Type: KeyTypeSubscription, Region: RegionGlobal, Priority: 5},
	}
	if !reflect.DeepEqual(c.APIKeys, wantKeys) {
		t.Errorf("APIKeys = %+v", c.APIKeys)
	}
	if c.Catalog.Discover || c.Catalog.RefreshInterval != time.Hour || c.Catalog.StaleWhileUnavailable {
		t.Errorf("Catalog = %+v", c.Catalog)
	}
	if c.Quota.UsageCount != UsageCountUsed || !reflect.DeepEqual(c.Quota.Endpoints, []string{"/v1/custom/remains"}) {
		t.Errorf("Quota = %+v", c.Quota)
	}
	wantGlobal := map[string]ProtocolOverride{"responses": {Endpoint: "/v1/global-responses"}, "messages": {}}
	if !reflect.DeepEqual(c.Protocols, wantGlobal) {
		t.Errorf("Protocols = %+v", c.Protocols)
	}
	on, off := true, false
	want := map[string]ProtocolOverride{
		"responses":        {Endpoint: "/v1/responses"},
		"messages":         {Enabled: &on},
		"chat-completions": {Enabled: &off},
	}
	if len(c.Models) != 1 || !reflect.DeepEqual(c.Models[0].Protocols, want) {
		t.Errorf("Models = %+v", c.Models)
	}
	if !c.AllowHTTP || c.RequestTimeout != 5*time.Minute || c.MaxResponseBytes != 1024 {
		t.Errorf("execution settings = %v %v %d", c.AllowHTTP, c.RequestTimeout, c.MaxResponseBytes)
	}
}

func TestNormalizeRegion(t *testing.T) {
	for raw, want := range map[string]string{
		"global": RegionGlobal, " Global ": RegionGlobal, "intl": RegionGlobal, "international": RegionGlobal, "io": RegionGlobal,
		"cn": RegionCN, "CN": RegionCN, "china": RegionCN, "mainland": RegionCN,
	} {
		if got, ok := NormalizeRegion(raw); !ok || got != want {
			t.Errorf("NormalizeRegion(%q) = %q, %v; want %q", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"", "us", "eu"} {
		if _, ok := NormalizeRegion(raw); ok {
			t.Errorf("NormalizeRegion(%q) accepted", raw)
		}
	}
}

func TestPerKeyRegionOverridesDefault(t *testing.T) {
	c, err := Load([]byte("region: china\napi-keys:\n  - value: a\n  - value: b\n    region: global\n  - value: c\n    region: \" \"\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := []string{c.APIKeys[0].Region, c.APIKeys[1].Region, c.APIKeys[2].Region}
	if !reflect.DeepEqual(got, []string{RegionCN, RegionGlobal, RegionCN}) {
		t.Fatalf("regions = %v", got)
	}
}

func TestResolveKeyType(t *testing.T) {
	tests := []struct {
		key, declared, want string
	}{
		{"sk-cp-abc", "", KeyTypeSubscription},
		{"  sk-cp-abc", "", KeyTypeSubscription},
		{"sk-api-abc", "", KeyTypeAPI},
		{"eyJhbGciOi", "", KeyTypeAPI},
		{"sk-api-abc", "subscription", KeyTypeSubscription},
		{"sk-api-abc", "Plan", KeyTypeSubscription},
		{"x", "m-plan", KeyTypeSubscription},
		{"x", "token-plan", KeyTypeSubscription},
		{"x", "coding-plan", KeyTypeSubscription},
		{"sk-cp-abc", "api", KeyTypeAPI},
		{"x", "pay-as-you-go", KeyTypeAPI},
		{"x", " PAYG ", KeyTypeAPI},
	}
	for _, tt := range tests {
		if got, ok := ResolveKeyType(tt.key, tt.declared); !ok || got != tt.want {
			t.Errorf("ResolveKeyType(%q, %q) = %q, %v; want %q", tt.key, tt.declared, got, ok, tt.want)
		}
	}
	if _, ok := ResolveKeyType("x", "free"); ok {
		t.Error("unknown type accepted")
	}
}

func TestKeyPriorityDefaultsAndOverrides(t *testing.T) {
	c, err := Load([]byte(`api-keys:
  - value: sk-cp-plan
  - value: sk-api
  - value: sk-cp-low
    priority: -2
  - value: sk-api-high
    priority: 9
  - value: sk-explicit-plan
    type: subscription
`))
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, k := range c.APIKeys {
		got = append(got, k.Priority)
	}
	if !reflect.DeepEqual(got, []int{1, 0, -2, 9, 1}) {
		t.Fatalf("priorities = %v", got)
	}
	if DefaultPriority[KeyTypeSubscription] <= DefaultPriority[KeyTypeAPI] {
		t.Fatal("subscription keys must outrank API keys by default")
	}
}

func TestConfigKeyLookup(t *testing.T) {
	c, err := Load([]byte("api-keys:\n  - value: a\n    name: A\n  - value: b\n"))
	if err != nil {
		t.Fatal(err)
	}
	if k, ok := c.Key("a"); !ok || k.Name != "A" {
		t.Errorf("Key(a) = %+v, %v", k, ok)
	}
	if _, ok := c.Key("missing"); ok {
		t.Error("Key(missing) found")
	}
}

func TestBaseURLTrimAndResolve(t *testing.T) {
	for _, base := range []string{"https://proxy.example.com", "https://proxy.example.com/", "https://proxy.example.com/v1", "https://proxy.example.com/v1/", "https://proxy.example.com/anthropic", "https://proxy.example.com/anthropic/v1"} {
		c, err := Load([]byte("base-url: " + base + "\n" + withKey))
		if err != nil {
			t.Fatalf("Load(%s) failed: %v", base, err)
		}
		for _, region := range []string{RegionGlobal, RegionCN, "", "mars", "international"} {
			if got := c.UpstreamBaseURL(APIKey{Region: region}); got != "https://proxy.example.com" {
				t.Errorf("base %s, region %s: got %q", base, region, got)
			}
		}
	}
	for _, tc := range []struct {
		name, yaml, want string
		keyIndex         int
	}{
		{"global default", withKey, DefaultGlobalBaseURL, 0},
		{"key http allowed", "allow-http: true\napi-keys:\n  - value: a\n    base-url: http://localhost:9090/v1\n", "http://localhost:9090", 0},
		{"cn default", "region: china\n" + withKey, DefaultCNBaseURL, 0},
		{"key region default", "api-keys:\n  - value: a\n    region: cn\n", DefaultCNBaseURL, 0},
		{"mixed region default", "region: cn\napi-keys:\n  - value: a\n    region: intl\n", DefaultGlobalBaseURL, 0},
		{"shared override", "base-url: https://shared.example/v1beta\napi-keys:\n  - value: a\n    region: cn\n", "https://shared.example/v1beta", 0},
		{"explicit default host overrides region", "base-url: https://api.minimax.io\napi-keys:\n  - value: a\n    region: cn\n", DefaultGlobalBaseURL, 0},
		{"key override", "base-url: https://shared.example\napi-keys:\n  - value: a\n    base-url: https://key.example/anthropic/v1/\n  - value: b\n", "https://key.example", 0},
		{"other key inherits shared", "base-url: https://shared.example\napi-keys:\n  - value: a\n    base-url: https://key.example\n  - value: b\n", "https://shared.example", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load([]byte(tc.yaml))
			if err != nil {
				t.Fatal(err)
			}
			if got := c.UpstreamBaseURL(c.APIKeys[tc.keyIndex]); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	for _, region := range []string{"", "mars", "cn"} {
		c, err := Load([]byte("region: cn\n" + withKey))
		if err != nil {
			t.Fatal(err)
		}
		if got := c.UpstreamBaseURL(APIKey{Region: region}); got != DefaultCNBaseURL {
			t.Errorf("fallback for %q = %q", region, got)
		}
	}
}

func TestProtocolEntryForms(t *testing.T) {
	doc := withKey + "models:\n  - name: m\n    protocols:\n      responses:\n      messages: {}\n      chat-completions: {enabled: false}\n"
	c, err := Load([]byte(doc))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	got := c.Models[0].Protocols
	for name, want := range map[string]bool{"responses": true, "messages": true, "chat-completions": false} {
		p, ok := got[name]
		if !ok || p.IsEnabled() != want {
			t.Errorf("%s: entry = %+v (present %v), want enabled %v", name, p, ok, want)
		}
	}
}

func TestLoadEnvExpansion(t *testing.T) {
	t.Setenv("TEST_MM_KEY", "expanded-secret")
	c, err := Load([]byte("api-keys:\n  - value: ${TEST_MM_KEY}\n"))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if len(c.APIKeys) != 1 || c.APIKeys[0].Value != "expanded-secret" {
		t.Fatalf("APIKeys = %+v", c.APIKeys)
	}
}

func TestLoadIgnoresUnknownFields(t *testing.T) {
	doc := "routing:\n  strategy: round-robin\nfuture-field: https://example.invalid\ncatalog-url: https://x\nmodels-dev: {enabled: true}\n" + withKey
	if _, err := Load([]byte(doc)); err != nil {
		t.Fatalf("unknown fields must be ignored: %v", err)
	}
}

func TestPrefixDisabledSkipsValueValidation(t *testing.T) {
	doc := `
model-prefix:
  enabled: false
  value: "has space"
` + withKey
	c, err := Load([]byte(doc))
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got := PublicID(c, "MiniMax-M3"); got != "MiniMax-M3" {
		t.Errorf("PublicID = %q", got)
	}
}

func TestPublicID(t *testing.T) {
	enabled := Config{ModelPrefix: ModelPrefix{Enabled: true, Value: "minimax"}}
	if got := PublicID(enabled, "MiniMax-M3"); got != "minimax/MiniMax-M3" {
		t.Errorf("PublicID(enabled) = %q", got)
	}
	disabled := Config{ModelPrefix: ModelPrefix{Enabled: false}}
	if got := PublicID(disabled, "MiniMax-M3"); got != "MiniMax-M3" {
		t.Errorf("PublicID(disabled) = %q", got)
	}
}

func TestLoadRejections(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"invalid yaml syntax", "[unclosed", "decode config"},
		{"unknown anchor has no line info", "request-timeout: *nope\n", "decode config: invalid YAML structure"},
		{"bare-scalar api key never leaks", "api-keys:\n  - sk-live-secret-123\n", "decode config: invalid YAML structure"},
		{"keyless config rejected", "", "api-keys: at least one key is required"},
		{"unknown default region", "region: us\n" + withKey, `region: must be "global" or "cn"`},
		{"unknown key region", "api-keys:\n  - value: a\n    region: eu\n", `api-keys[0].region: must be "global" or "cn"`},
		{"unknown key type", "api-keys:\n  - value: a\n    type: free\n", `api-keys[0].type: must be "subscription" or "api"`},
		{"non-integer priority", "api-keys:\n  - value: a\n    priority: high\n", "decode config: invalid YAML structure near line"},
		{"empty base-url", "base-url: \"\"\n" + withKey, "base-url: must not be empty"},
		{"malformed base-url", "base-url: \"::::\"\n" + withKey, "base-url: invalid URL"},
		{"scheme-less base-url", "base-url: api.minimax.io\n" + withKey, "base-url: invalid URL"},
		{"http base-url without allow-http", "base-url: http://localhost:9090\n" + withKey, "base-url: http scheme requires allow-http"},
		{"ftp base-url", "base-url: ftp://files.example.com\n" + withKey, `base-url: unsupported scheme "ftp"`},
		{"ftp base-url with allow-http", "allow-http: true\nbase-url: ftp://files.example.com\n" + withKey, `base-url: unsupported scheme "ftp"`},
		{"userinfo credentials never echoed", "base-url: https://user:sekret@/v1\n" + withKey, "base-url: invalid URL https://"},
		{"empty key base-url", "api-keys:\n  - value: a\n    base-url: \"\"\n", "api-keys[0].base-url: must not be empty"},
		{"malformed key base-url", "api-keys:\n  - value: a\n    base-url: \"::::\"\n", "api-keys[0].base-url: invalid URL"},
		{"http key base-url", "api-keys:\n  - value: a\n    base-url: http://localhost:9090\n", "api-keys[0].base-url: http scheme requires allow-http"},
		{"ftp key base-url", "allow-http: true\napi-keys:\n  - value: a\n    base-url: ftp://files.example.com\n", "api-keys[0].base-url: unsupported scheme"},
		{"query key base-url", "api-keys:\n  - value: a\n    base-url: https://host/v1?tenant=x\n", "api-keys[0].base-url: must not contain query"},
		{"fragment key base-url", "api-keys:\n  - value: a\n    base-url: https://host/#fragment\n", "api-keys[0].base-url: must not contain query, fragment, or userinfo"},
		{"userinfo key base-url", "api-keys:\n  - value: a\n    base-url: https://user:secret@host/v1\n", "api-keys[0].base-url: must not contain query, fragment, or userinfo"},
		{"quota usage-count unknown", "quota:\n  usage-count: total\n" + withKey, `quota.usage-count: must be "remaining" or "used"`},
		{"quota endpoint without slash", "quota:\n  endpoints: [v1/remains]\n" + withKey, "quota.endpoints[0]: must start with /"},
		{"request-timeout zero rejected", "request-timeout: 0s\n" + withKey, "request-timeout: must be positive"},
		{
			"duplicate protocol keys",
			"protocols:\n  messages: {}\n  messages: {}\n",
			"decode config: invalid YAML structure near line",
		},
		{
			"protocol entry not a mapping",
			"models:\n  - name: bad-model\n    protocols:\n      responses: /v1/x\n" + withKey,
			"decode config: invalid YAML structure near line",
		},
		{
			"model protocol bool shorthand",
			"models:\n  - name: bad-model\n    protocols:\n      responses: false\n" + withKey,
			"decode config: invalid YAML structure near line",
		},
		{
			"global protocol unsupported",
			"protocols:\n  grpc: {}\n" + withKey,
			`protocols: unsupported protocol "grpc"`,
		},
		{
			"gemini is not an upstream protocol",
			"protocols:\n  gemini: {}\n" + withKey,
			`protocols: unsupported protocol "gemini"`,
		},
		{
			"global protocol enabled field",
			"protocols:\n  responses:\n    enabled: true\n" + withKey,
			"protocols.responses: only endpoint is allowed; switch protocols under models[].protocols",
		},
		{
			"global endpoint without leading slash",
			"protocols:\n  responses:\n    endpoint: v1/x\n" + withKey,
			"protocols.responses.endpoint: must start with /",
		},
		{
			"key expands to empty",
			"api-keys:\n  - value: ${TEST_MM_DEFINITELY_UNSET_VAR}\n",
			"api-keys[0].value: expanded to empty",
		},
		{
			"second key expands to empty",
			"api-keys:\n  - value: literal-ok\n  - value: ${TEST_MM_DEFINITELY_UNSET_VAR}\n",
			"api-keys[1].value: expanded to empty",
		},
		{"duplicate keys", "api-keys:\n  - value: a\n  - value: ' a '\n", "api-keys: duplicate key values are not allowed"},
		{"refresh-interval missing unit", "catalog:\n  refresh-interval: 5\n", "catalog.refresh-interval: invalid duration"},
		{"refresh-interval negative", "catalog:\n  refresh-interval: -5m\n" + withKey, "catalog.refresh-interval: must be at least 1m"},
		{"refresh-interval below floor", "catalog:\n  refresh-interval: 1ms\n" + withKey, "catalog.refresh-interval: must be at least 1m"},
		{"request-timeout unparsable", "request-timeout: bogus\n", "request-timeout: invalid duration"},
		{"request-timeout explicitly empty", `request-timeout: ""`, "request-timeout: invalid duration"},
		{"max-response-bytes zero rejected", "max-response-bytes: 0\n" + withKey, "max-response-bytes: must be positive"},
		{"max-response-bytes negative rejected", "max-response-bytes: -5\n" + withKey, "max-response-bytes: must be positive"},
		{
			"model protocols in list form",
			"models:\n  - name: bad-model\n    protocols:\n      - protocol: responses\n" + withKey,
			"decode config: invalid YAML structure near line",
		},
		{
			"model protocol unsupported",
			"models:\n  - name: bad-model\n    protocols:\n      grpc: {}\n" + withKey,
			`models[0].protocols: unsupported protocol "grpc"`,
		},
		{
			"model endpoint without leading slash",
			"models:\n  - name: bad-model\n    protocols:\n      responses:\n        endpoint: v1/x\n" + withKey,
			"models[0].protocols.responses.endpoint: must start with /",
		},
		{"prefix enabled empty value", "model-prefix:\n  enabled: true\n  value: \"\"\n" + withKey, "model-prefix.value: invalid provider-ID characters"},
		{"prefix with space", "model-prefix:\n  value: has space\n" + withKey, "model-prefix.value: invalid provider-ID characters"},
		{"prefix leading dash", "model-prefix:\n  value: -lead\n" + withKey, "model-prefix.value: invalid provider-ID characters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load([]byte(tt.yaml))
			requireErrContains(t, err, tt.wantErr)
			if tt.name == "key expands to empty" && strings.Contains(err.Error(), "${") {
				t.Errorf("error leaks environment reference: %q", err.Error())
			}
		})
	}
}

// TestLoadErrorsNeverLeakDecodedValues pins the security property: decode
// and validation failures must not echo raw scalars (a bare-scalar API key)
// or embedded userinfo credentials into the error the host logs.
func TestLoadErrorsNeverLeakDecodedValues(t *testing.T) {
	cases := []struct {
		name   string
		yaml   string
		secret string
	}{
		{"bare-scalar api key", "api-keys:\n  - sk-live-supersecret-42\n", "sk-live-supersecret-42"},
		{"duplicate api key values", "api-keys:\n  - value: sk-live-dupsecret-7\n  - value: sk-live-dupsecret-7\n", "sk-live-dupsecret-7"},
		{"invalid key type", "api-keys:\n  - value: sk-live-typesecret-8\n    type: bogus\n", "sk-live-typesecret-8"},
		{"userinfo credentials", "base-url: https://admin:p4ssw0rd@/v1\n" + withKey, "p4ssw0rd"},
	}
	for _, tc := range cases {
		_, err := Load([]byte(tc.yaml))
		if err == nil {
			t.Fatalf("%s: expected rejection", tc.name)
		}
		if strings.Contains(err.Error(), tc.secret) {
			t.Fatalf("%s: error leaks secret %q: %v", tc.name, tc.secret, err)
		}
	}
}

// TestValidateURLRejectsQueryFragmentUserinfo pins the fix for bases that
// parse fine but break JoinUpstreamURL: a query or fragment truncates the
// joined path, and userinfo leaks credentials into every upstream URL.
func TestValidateURLRejectsQueryFragmentUserinfo(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		httpOK  bool
		wantErr string
	}{
		{"query rejected naming setting", "https://host/v1?tenant=x", false, "base-url: must not contain query"},
		{"fragment rejected", "https://host/v1#frag", false, "must not contain query, fragment, or userinfo"},
		{"userinfo rejected without echoing credentials", "https://user:key@host/v1", false, "must not contain query, fragment, or userinfo"},
		{"clean https passes", "https://host", false, ""},
		{"http with allow-http passes", "http://localhost:9090", true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateURL("base-url", tt.raw, tt.httpOK)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateURL(%q) = %v, want nil", tt.raw, err)
				}
				return
			}
			requireErrContains(t, err, tt.wantErr)
			if strings.Contains(err.Error(), "key") && strings.Contains(tt.raw, ":key@") {
				t.Errorf("error echoes credentials: %q", err.Error())
			}
		})
	}
}

func TestValidProtocolsMatchCatalogRoutes(t *testing.T) {
	var got []string
	for name := range validProtocols {
		got = append(got, name)
	}
	slices.Sort(got)
	if !reflect.DeepEqual(got, []string{"chat-completions", "messages", "responses"}) {
		t.Fatalf("validProtocols = %v", got)
	}
}

func TestKeyExpandsOnlyBracedEnvRefs(t *testing.T) {
	t.Setenv("CPA_MINIMAX_TEST_KEY", "from-env")
	c, err := Load([]byte("api-keys:\n  - value: \"ab$cd12\"\n  - value: \"${CPA_MINIMAX_TEST_KEY}\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.APIKeys[0].Value != "ab$cd12" || c.APIKeys[1].Value != "from-env" {
		t.Fatalf("keys = %q, %q", c.APIKeys[0].Value, c.APIKeys[1].Value)
	}
}
