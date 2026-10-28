// Package config loads and validates the minimax plugin configuration.
package config

import (
	"fmt"
	"maps"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/dillonzq/cpa-minimax/internal/modelmeta"

	"gopkg.in/yaml.v3"
)

// Regions served by MiniMax. Keys only work against their own region.
const (
	RegionGlobal = "global"
	RegionCN     = "cn"
)

// Credential types. A subscription key belongs to an M Plan (formerly Coding
// Plan / Token Plan); an API key is billed pay-as-you-go from the balance.
const (
	KeyTypeSubscription = "subscription"
	KeyTypeAPI          = "api"
)

// Usage-count semantics for the subscription remains endpoint. MiniMax
// acknowledged that current_*_usage_count reports the remaining count.
const (
	UsageCountRemaining = "remaining"
	UsageCountUsed      = "used"
)

// Defaults.
const (
	DefaultRegion           = RegionGlobal
	DefaultGlobalBaseURL    = "https://api.minimax.io"
	DefaultCNBaseURL        = "https://api.minimax.cn"
	DefaultModelPrefix      = "minimax"
	DefaultRefreshInterval  = 15 * time.Minute
	DefaultRequestTimeout   = 5 * time.Minute
	DefaultMaxResponseBytes = int64(67108864) // 64 MiB
	// SubscriptionKeyPrefix is the observed M Plan key prefix. It only
	// decides the default type; an explicit type always wins.
	SubscriptionKeyPrefix = "sk-cp-"
)

// DefaultQuotaEndpoints are tried in order for subscription usage.
var DefaultQuotaEndpoints = []string{"/v1/token_plan/remains", "/v1/api/openplatform/coding_plan/remains"}

// DefaultPriority ranks subscription keys above pay-as-you-go keys, so CPA
// spends plan usage first and falls back to the balance while plans cool down.
var DefaultPriority = map[string]int{KeyTypeSubscription: 1, KeyTypeAPI: 0}

type ModelPrefix struct {
	Enabled bool
	Value   string
}

// APIKey is one configured credential. Type and Region are always resolved.
type APIKey struct {
	Name     string
	Value    string
	Type     string
	Region   string
	BaseURL  string // optional per-key override
	Priority int
}

type Catalog struct {
	Discover              bool
	RefreshInterval       time.Duration
	StaleWhileUnavailable bool
}

type Quota struct {
	UsageCount string
	Endpoints  []string
}

type Config struct {
	Models           []ModelOverride
	Region           string
	BaseURL          string
	baseURLDefaulted bool // distinguish a region-derived default from a shared override
	ModelPrefix      ModelPrefix
	APIKeys          []APIKey
	Catalog          Catalog
	Protocols        map[string]ProtocolOverride // endpoints for every model; switches live on models
	Quota            Quota
	AllowHTTP        bool
	RequestTimeout   time.Duration
	MaxResponseBytes int64
}

type ModelOverride struct {
	Name string `yaml:"name"`
	// Protocols patches the built-in protocol set, keyed by protocol: an
	// entry switches its protocol on unless Enabled is false, and a
	// non-empty Endpoint overrides the global path.
	Protocols          map[string]ProtocolOverride `yaml:"protocols"`
	modelmeta.Metadata `yaml:",inline"`
}

// ProtocolOverride is one protocols entry.
type ProtocolOverride struct {
	Enabled  *bool  `yaml:"enabled"`
	Endpoint string `yaml:"endpoint"`
}

// IsEnabled reports whether the entry switches its protocol on.
func (p ProtocolOverride) IsEnabled() bool {
	return p.Enabled == nil || *p.Enabled
}

// rawConfig mirrors the YAML shape; pointer fields distinguish "unset"
// (apply default) from explicitly-set values including "" (validate as-is).
// Unknown fields are ignored (host may pass extra keys).
type rawConfig struct {
	Models      []ModelOverride             `yaml:"models"`
	Region      *string                     `yaml:"region"`
	BaseURL     *string                     `yaml:"base-url"`
	ModelPrefix rawPrefix                   `yaml:"model-prefix"`
	APIKeys     []rawKey                    `yaml:"api-keys"`
	Catalog     rawCatalog                  `yaml:"catalog"`
	Protocols   map[string]ProtocolOverride `yaml:"protocols"`
	Quota       struct {
		UsageCount *string  `yaml:"usage-count"`
		Endpoints  []string `yaml:"endpoints"`
	} `yaml:"quota"`
	AllowHTTP        bool    `yaml:"allow-http"`
	RequestTimeout   *string `yaml:"request-timeout"`
	MaxResponseBytes *int64  `yaml:"max-response-bytes"`
}

type rawPrefix struct {
	Enabled *bool   `yaml:"enabled"`
	Value   *string `yaml:"value"`
}

type rawKey struct {
	Name     string  `yaml:"name"`
	Value    string  `yaml:"value"`
	Type     string  `yaml:"type"`
	Region   string  `yaml:"region"`
	BaseURL  *string `yaml:"base-url"`
	Priority *int    `yaml:"priority"`
}

type rawCatalog struct {
	Discover              *bool   `yaml:"discover"`
	RefreshInterval       *string `yaml:"refresh-interval"`
	StaleWhileUnavailable *bool   `yaml:"stale-while-unavailable"`
}

var validProtocols = map[string]bool{"chat-completions": true, "messages": true, "responses": true}

// Load decodes YAML, expands ${VAR} references in api-key values only,
// applies defaults, and validates. Decode errors never echo decoded node
// values — a malformed entry (e.g. a bare-scalar API key) must not leak into
// the invalid_config envelope the host logs.
func Load(yamlBytes []byte) (Config, error) {
	var raw rawConfig
	if err := yaml.Unmarshal(yamlBytes, &raw); err != nil {
		if n := regexp.MustCompile(`line (\d+)`).FindStringSubmatch(err.Error()); n != nil {
			return Config{}, fmt.Errorf("decode config: invalid YAML structure near line %s", n[1])
		}
		return Config{}, fmt.Errorf("decode config: invalid YAML structure")
	}
	region, ok := NormalizeRegion(orDefault(raw.Region, DefaultRegion))
	if !ok {
		return Config{}, fmt.Errorf("region: must be %q or %q", RegionGlobal, RegionCN)
	}
	keys := make([]APIKey, len(raw.APIKeys))
	for i, k := range raw.APIKeys {
		value := strings.TrimSpace(expandEnv(k.Value))
		keyRegion := region
		if strings.TrimSpace(k.Region) != "" {
			if keyRegion, ok = NormalizeRegion(k.Region); !ok {
				return Config{}, fmt.Errorf("api-keys[%d].region: must be %q or %q", i, RegionGlobal, RegionCN)
			}
		}
		keyType, ok := ResolveKeyType(value, k.Type)
		if !ok {
			return Config{}, fmt.Errorf("api-keys[%d].type: must be %q or %q", i, KeyTypeSubscription, KeyTypeAPI)
		}
		baseURL := ""
		if k.BaseURL != nil {
			baseURL = normalizeBaseURL(*k.BaseURL)
			if err := validateURL(fmt.Sprintf("api-keys[%d].base-url", i), baseURL, raw.AllowHTTP); err != nil {
				return Config{}, err
			}
		}
		keys[i] = APIKey{BaseURL: baseURL, Value: value, Name: strings.TrimSpace(k.Name), Type: keyType, Region: keyRegion, Priority: orDefault(k.Priority, DefaultPriority[keyType])}
	}
	refreshInterval, err := parseDuration("catalog.refresh-interval", raw.Catalog.RefreshInterval, DefaultRefreshInterval)
	if err != nil {
		return Config{}, err
	}
	requestTimeout, err := parseDuration("request-timeout", raw.RequestTimeout, DefaultRequestTimeout)
	if err != nil {
		return Config{}, err
	}
	if requestTimeout <= 0 {
		return Config{}, fmt.Errorf("request-timeout: must be positive")
	}
	c := Config{
		Models:           raw.Models,
		Region:           region,
		BaseURL:          normalizeBaseURL(orDefault(raw.BaseURL, defaultBaseURL(region))),
		baseURLDefaulted: raw.BaseURL == nil,
		ModelPrefix: ModelPrefix{
			Enabled: orDefault(raw.ModelPrefix.Enabled, true),
			Value:   orDefault(raw.ModelPrefix.Value, DefaultModelPrefix),
		},
		APIKeys: keys,
		Catalog: Catalog{
			Discover:              orDefault(raw.Catalog.Discover, true),
			RefreshInterval:       refreshInterval,
			StaleWhileUnavailable: orDefault(raw.Catalog.StaleWhileUnavailable, true),
		},
		Protocols: raw.Protocols,
		Quota: Quota{
			UsageCount: strings.ToLower(strings.TrimSpace(orDefault(raw.Quota.UsageCount, UsageCountRemaining))),
			Endpoints:  raw.Quota.Endpoints,
		},
		AllowHTTP:        raw.AllowHTTP,
		RequestTimeout:   requestTimeout,
		MaxResponseBytes: orDefault(raw.MaxResponseBytes, DefaultMaxResponseBytes),
	}
	if c.Quota.Endpoints == nil {
		c.Quota.Endpoints = slices.Clone(DefaultQuotaEndpoints)
	}
	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// NormalizeRegion maps accepted region spellings to RegionGlobal or RegionCN.
func NormalizeRegion(raw string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "global", "intl", "international", "io":
		return RegionGlobal, true
	case "cn", "china", "mainland":
		return RegionCN, true
	}
	return "", false
}

// ResolveKeyType returns the credential type. An empty declaration detects
// subscription keys by their observed prefix and defaults to an API key.
func ResolveKeyType(key, declared string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(declared)) {
	case "":
		if strings.HasPrefix(strings.TrimSpace(key), SubscriptionKeyPrefix) {
			return KeyTypeSubscription, true
		}
		return KeyTypeAPI, true
	case "subscription", "plan", "m-plan", "token-plan", "coding-plan":
		return KeyTypeSubscription, true
	case "api", "pay-as-you-go", "payg":
		return KeyTypeAPI, true
	}
	return "", false
}

// UpstreamBaseURL resolves a key override, then an explicit shared base-url,
// then the default host for the credential's region.
func (c Config) UpstreamBaseURL(key APIKey) string {
	if key.BaseURL != "" {
		return key.BaseURL
	}
	region, ok := NormalizeRegion(key.Region)
	if !ok {
		region = c.Region
	}
	if c.BaseURL != "" && (!c.baseURLDefaulted || region == c.Region) {
		return c.BaseURL
	}
	return defaultBaseURL(region)
}

func defaultBaseURL(region string) string {
	if region == RegionCN {
		return DefaultCNBaseURL
	}
	return DefaultGlobalBaseURL
}

// NormalizeCredentialBaseURL validates and normalizes a persisted key override.
func NormalizeCredentialBaseURL(raw string, allowHTTP bool) (string, error) {
	base := normalizeBaseURL(raw)
	if err := validateURL("base_url", base, allowHTTP); err != nil {
		return "", err
	}
	return base, nil
}

// Key returns the configured entry for a key value.
func (c Config) Key(value string) (APIKey, bool) {
	for _, k := range c.APIKeys {
		if k.Value == value {
			return k, true
		}
	}
	return APIKey{}, false
}

// normalizeBaseURL drops trailing slashes and a trailing /v1 or /anthropic:
// endpoint paths carry their own prefixes.
func normalizeBaseURL(raw string) string {
	base := strings.TrimRight(strings.TrimSpace(raw), "/")
	for _, suffix := range []string{"/v1", "/anthropic"} {
		base = strings.TrimSuffix(base, suffix)
	}
	return base
}

// PublicID returns the client-facing model ID.
func PublicID(c Config, upstreamID string) string {
	if c.ModelPrefix.Enabled {
		return c.ModelPrefix.Value + "/" + upstreamID
	}
	return upstreamID
}

func (c Config) validate() error {
	if err := validateProtocols("protocols", c.Protocols, false); err != nil {
		return err
	}
	seenModels := map[string]bool{}
	for i, model := range c.Models {
		if strings.TrimSpace(model.Name) == "" || seenModels[model.Name] {
			return fmt.Errorf("models[%d].name: must be non-empty and unique", i)
		}
		seenModels[model.Name] = true
		if model.Context != nil && *model.Context < 0 || model.Output != nil && *model.Output < 0 {
			return fmt.Errorf("models[%d]: token limits must be non-negative", i)
		}
		if model.Thinking != nil {
			t := model.Thinking
			if t.Min != nil && *t.Min < 0 || t.Max != nil && *t.Max < 0 || t.Min != nil && t.Max != nil && *t.Min > *t.Max {
				return fmt.Errorf("models[%d].thinking: invalid budget range", i)
			}
		}
		if err := validateProtocols(fmt.Sprintf("models[%d].protocols", i), model.Protocols, true); err != nil {
			return err
		}
	}
	if err := validateURL("base-url", c.BaseURL, c.AllowHTTP); err != nil {
		return err
	}
	if len(c.APIKeys) == 0 {
		return fmt.Errorf("api-keys: at least one key is required")
	}
	seen := make(map[string]bool, len(c.APIKeys))
	for i, k := range c.APIKeys {
		if k.Value == "" {
			return fmt.Errorf("api-keys[%d].value: expanded to empty", i)
		}
		if seen[k.Value] {
			return fmt.Errorf("api-keys: duplicate key values are not allowed")
		}
		seen[k.Value] = true
	}
	if c.Catalog.RefreshInterval < time.Minute {
		return fmt.Errorf("catalog.refresh-interval: must be at least 1m")
	}
	if c.MaxResponseBytes <= 0 {
		return fmt.Errorf("max-response-bytes: must be positive")
	}
	if c.ModelPrefix.Enabled && !validPrefix(c.ModelPrefix.Value) {
		return fmt.Errorf("model-prefix.value: invalid provider-ID characters %q", c.ModelPrefix.Value)
	}
	if c.Quota.UsageCount != UsageCountRemaining && c.Quota.UsageCount != UsageCountUsed {
		return fmt.Errorf("quota.usage-count: must be %q or %q", UsageCountRemaining, UsageCountUsed)
	}
	for i, endpoint := range c.Quota.Endpoints {
		if !strings.HasPrefix(endpoint, "/") {
			return fmt.Errorf("quota.endpoints[%d]: must start with /", i)
		}
	}
	return nil
}

// validateProtocols checks one protocols map. Only model entries may switch
// a protocol; global entries set paths.
func validateProtocols(field string, protocols map[string]ProtocolOverride, switchable bool) error {
	for _, name := range slices.Sorted(maps.Keys(protocols)) {
		p := protocols[name]
		if !validProtocols[name] {
			return fmt.Errorf("%s: unsupported protocol %q", field, name)
		}
		if p.Enabled != nil && !switchable {
			return fmt.Errorf("%s.%s: only endpoint is allowed; switch protocols under models[].protocols", field, name)
		}
		if p.Endpoint != "" && !strings.HasPrefix(p.Endpoint, "/") {
			return fmt.Errorf("%s.%s.endpoint: must start with /", field, name)
		}
	}
	return nil
}

func validateURL(name, raw string, allowHTTP bool) error {
	if raw == "" {
		return fmt.Errorf("%s: must not be empty", name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: invalid URL", name)
	}
	if u.Scheme == "" || u.Host == "" {
		// Echo only scheme://host — the configured string may embed
		// userinfo credentials (https://user:key@host).
		return fmt.Errorf("%s: invalid URL %s://%s", name, u.Scheme, u.Host)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return fmt.Errorf("%s: must not contain query, fragment, or userinfo", name)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !allowHTTP {
			return fmt.Errorf("%s: http scheme requires allow-http", name)
		}
	default:
		return fmt.Errorf("%s: unsupported scheme %q; use https", name, u.Scheme)
	}
	return nil
}

func validPrefix(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		alnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if i == 0 && !alnum {
			return false
		}
		if !alnum && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func parseDuration(name string, p *string, def time.Duration) (time.Duration, error) {
	if p == nil {
		return def, nil
	}
	d, err := time.ParseDuration(*p)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid duration", name)
	}
	return d, nil
}

func orDefault[T any](p *T, def T) T {
	if p != nil {
		return *p
	}
	return def
}

// envRef matches only the documented ${NAME} form, so a literal "$" inside a
// key is kept instead of being expanded like os.ExpandEnv's bare $NAME.
var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func expandEnv(value string) string {
	return envRef.ReplaceAllStringFunc(value, func(ref string) string {
		return os.Getenv(ref[2 : len(ref)-1])
	})
}
