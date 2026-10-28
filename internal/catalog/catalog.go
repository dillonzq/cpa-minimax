// Package catalog builds the routable MiniMax model snapshot from the
// built-in model table, optional live /v1/models discovery, and user
// declarations, and serves the last good snapshot.
//
// Protocol resolution: the built-in family table patched by the user's
// models[].protocols switches; a model left without protocols is
// unsupported (excluded from the routable set, kept in diagnostics).
package catalog

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/dillonzq/cpa-minimax/internal/config"
	"github.com/dillonzq/cpa-minimax/internal/modelmeta"
)

// catalogBudgetFloor keeps an undersized max-response-bytes knob from
// freezing stale serving forever: catalogs grow larger than typical
// completion bodies, so the fetch budget never drops below 16 MiB while
// max-response-bytes stays meaningful for completions elsewhere.
const catalogBudgetFloor = 16 << 20

// Route is an upstream protocol a model is served through.
type Route string

const (
	RouteChatCompletions Route = "chat-completions"
	RouteMessages        Route = "messages"
	RouteResponses       Route = "responses"
)

// protocolOrder lists every route; protocols a model declaration adds beyond
// its built-in set follow in this order.
var protocolOrder = []Route{RouteMessages, RouteChatCompletions, RouteResponses}

// routeEndpoints maps each route to its default endpoint path below the
// region base URL (https://platform.minimax.io/docs/api-reference).
var routeEndpoints = map[Route]string{
	RouteChatCompletions: "/v1/chat/completions",
	RouteMessages:        "/anthropic/v1/messages",
	RouteResponses:       "/v1/responses",
}

// ModelsPath is MiniMax's OpenAI-style model listing.
const ModelsPath = "/v1/models"

// EndpointPath returns the route's default upstream endpoint path, or "" for
// unknown routes.
func (r Route) EndpointPath() string {
	return routeEndpoints[r]
}

// Target is one protocol a model supports and the endpoint path serving it.
type Target struct {
	Protocol     Route
	EndpointPath string
}

// URL joins a region base URL and the endpoint path.
func (t Target) URL(baseURL string) string {
	return JoinUpstreamURL(baseURL, t.EndpointPath)
}

// ModelRecord is one routable normalized catalog entry.
type ModelRecord struct {
	PublicID    string
	UpstreamID  string
	DisplayName string
	Description string
	// Routes lists the supported protocols in declaration order; the first
	// is the documented primary endpoint.
	Routes       []Target
	ContextLimit int64
	OutputLimit  int64
	InputModes   []string
	OutputModes  []string
	Thinking     *pluginapi.ThinkingSupport
	UserDefined  bool
}

// Target returns the record's endpoint for a protocol.
func (r ModelRecord) Target(route Route) (Target, bool) {
	for _, t := range r.Routes {
		if t.Protocol == route {
			return t, true
		}
	}
	return Target{}, false
}

// Supports reports whether the record serves a protocol.
func (r ModelRecord) Supports(route Route) bool {
	_, ok := r.Target(route)
	return ok
}

// UnsupportedModel is a model excluded from the routable set, retained for
// diagnostics.
type UnsupportedModel struct {
	UpstreamID string
	Reason     string
}

// HostClient issues HTTP requests through the host (pluginapi.HTTPDo
// semantics).
type HostClient interface {
	Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

// rawModel tolerantly decodes one catalog entry; unknown fields are ignored.
// MiniMax's /v1/models returns only id/object/created/owned_by, so metadata
// comes from the built-in table and user overrides.
type rawModel struct {
	ID          string `json:"id"`
	userDefined bool
}

// Manager owns the catalog snapshot. All accessors are safe for
// concurrent use alongside Refresh.
type Manager struct {
	cfg       config.Config
	client    HostClient
	refreshMu sync.Mutex

	mu sync.Mutex
	// raw retains the discovered upstream entries behind the current
	// snapshot so SeedFrom can rebuild records against a NEW config.
	raw    []rawModel
	models []ModelRecord
	// index maps both PublicID and UpstreamID to their record so ID
	// resolution is O(1); rebuilt atomically with models on every swap.
	index map[string]ModelRecord
	unsup []UnsupportedModel
	warns []string
}

// New returns a Manager serving cfg through the host client. The built-in
// models and user declarations are routable immediately.
func New(cfg config.Config, client HostClient) *Manager {
	m := &Manager{cfg: cfg, client: client}
	m.swap(nil)
	return m
}

// SeedFrom republishes only prev's discovered entries, rebuilt through the
// same resolution/validation path against m's OWN config: routes re-resolve,
// protocol switches re-apply, PublicIDs recompute under m's prefix, and
// endpoints re-check against m's base URLs.
func (m *Manager) SeedFrom(prev *Manager) {
	prev.mu.Lock()
	raw := prev.raw
	prev.mu.Unlock()
	m.swap(raw)
}

// Refresh discovers models from baseURL with apiKey and swaps the snapshot.
// With discovery disabled it only republishes built-in and declared models.
// On failure it returns a classified, key-free error; the previous discovered
// entries keep serving while catalog.stale-while-unavailable is enabled.
// Built-in and declared models stay available either way.
func (m *Manager) Refresh(ctx context.Context, baseURL, apiKey string) error {
	m.refreshMu.Lock()
	defer m.refreshMu.Unlock()
	if !m.cfg.Catalog.Discover {
		m.swap(nil)
		return nil
	}
	if m.client == nil {
		return m.fail("host client unavailable")
	}
	resp, err := m.client.Do(ctx, pluginapi.HTTPRequest{
		Method: http.MethodGet,
		URL:    JoinUpstreamURL(baseURL, ModelsPath),
		Headers: http.Header{
			"Authorization": []string{"Bearer " + apiKey},
			"Accept":        []string{"application/json"},
		},
	})
	if err != nil {
		return m.fail("network error")
	}
	budget := max(m.cfg.MaxResponseBytes, catalogBudgetFloor)
	if int64(len(resp.Body)) > budget {
		return m.fail("response exceeds max-response-bytes")
	}
	if resp.StatusCode != http.StatusOK {
		return m.fail(fmt.Sprintf("http %d", resp.StatusCode))
	}
	var env struct {
		Data     json.RawMessage `json:"data"`
		BaseResp *struct {
			StatusCode int `json:"status_code"`
		} `json:"base_resp"`
	}
	if err := json.Unmarshal(resp.Body, &env); err != nil {
		return m.fail("invalid json")
	}
	if env.BaseResp != nil && env.BaseResp.StatusCode != 0 {
		return m.fail(fmt.Sprintf("upstream status %d", env.BaseResp.StatusCode))
	}
	var entries []rawModel
	var warns []string
	if env.Data == nil {
		warns = append(warns, `upstream catalog response missing "data" field`)
	} else if err := json.Unmarshal(env.Data, &entries); err != nil {
		return m.fail("invalid json")
	}
	m.swap(entries, warns...)
	return nil
}

// fail applies the stale policy and returns the classified error.
func (m *Manager) fail(category string) error {
	m.mu.Lock()
	entries := m.raw
	if !m.cfg.Catalog.StaleWhileUnavailable {
		entries = nil
	}
	m.mu.Unlock()
	m.swap(entries)
	return fmt.Errorf("catalog refresh failed: %s", category)
}

// swap builds the new snapshot from discovered entries, the published
// built-in models, and user declarations, and installs it atomically.
// extraWarns are caller-supplied snapshot diagnostics.
func (m *Manager) swap(entries []rawModel, extraWarns ...string) {
	overrides := make(map[string]config.ModelOverride, len(m.cfg.Models))
	for _, model := range m.cfg.Models {
		overrides[model.Name] = model
	}
	// Keep m.raw limited to discovered entries. Otherwise a removed user model
	// could survive reconfiguration by being carried as stale upstream data.
	combined := make([]rawModel, 0, len(entries)+len(publishedModels)+len(m.cfg.Models))
	combined = append(combined, entries...)
	present := make(map[string]bool, cap(combined))
	for _, entry := range entries {
		present[entry.ID] = true
	}
	for _, id := range publishedModels {
		if !present[id] {
			combined = append(combined, rawModel{ID: id})
			present[id] = true
		}
	}
	for _, model := range m.cfg.Models {
		if !present[model.Name] {
			combined = append(combined, rawModel{ID: model.Name, userDefined: true})
		}
	}
	models := make([]ModelRecord, 0, len(combined))
	index := make(map[string]ModelRecord, len(combined)*2)
	var unsup []UnsupportedModel
	var warns []string
	warns = append(warns, extraWarns...)
	seen := make(map[string]bool, len(combined))
	for _, e := range combined {
		if e.ID == "" {
			unsup = append(unsup, UnsupportedModel{Reason: "missing id"})
			continue
		}
		if seen[e.ID] {
			warns = append(warns, fmt.Sprintf("duplicate model %q ignored; keeping first occurrence", e.ID))
			continue
		}
		seen[e.ID] = true
		override, declared := overrides[e.ID]
		targets, disabled, warn := m.resolve(e.ID, override.Protocols)
		if warn != "" {
			warns = append(warns, warn)
		}
		if len(targets) == 0 {
			// The listing also carries speech, video, and image models. Skip
			// those quietly; report only text-family or declared models.
			if !declared && len(disabled) == 0 {
				continue
			}
			reason := "no route determined"
			if len(disabled) > 0 {
				noun := "protocol"
				if len(disabled) > 1 {
					noun = "protocols"
				}
				reason = fmt.Sprintf("%s %s disabled by config", noun, strings.Join(disabled, ", "))
			}
			unsup = append(unsup, UnsupportedModel{UpstreamID: e.ID, Reason: reason})
			continue
		}
		routes, reason, routeWarns := m.routable(e.ID, targets)
		warns = append(warns, routeWarns...)
		if len(routes) == 0 {
			unsup = append(unsup, UnsupportedModel{UpstreamID: e.ID, Reason: reason})
			continue
		}
		metadata := modelmeta.Merge(builtinMetadata(e.ID), override.Metadata)
		display := modelmeta.Value(metadata.DisplayName)
		if metadata.DisplayName == nil {
			display = e.ID
		}
		rec := ModelRecord{
			PublicID:     config.PublicID(m.cfg, e.ID),
			UpstreamID:   e.ID,
			DisplayName:  display,
			Description:  modelmeta.Value(metadata.Description),
			Routes:       routes,
			ContextLimit: modelmeta.Value(metadata.Context),
			OutputLimit:  modelmeta.Value(metadata.Output),
			InputModes:   metadata.InputModalities,
			OutputModes:  metadata.OutputModalities,
			Thinking:     metadata.Thinking.Support(),
			UserDefined:  e.userDefined,
		}
		// With a prefix enabled, one record's PublicID can equal another
		// record's UpstreamID; first in catalog order wins and the later
		// record is excluded instead of silently misrouting.
		if other, ok := index[rec.PublicID]; ok {
			reason := fmt.Sprintf("public id %q collides with upstream id %q", rec.PublicID, other.UpstreamID)
			warns = append(warns, reason)
			unsup = append(unsup, UnsupportedModel{UpstreamID: e.ID, Reason: reason})
			continue
		}
		if other, ok := index[rec.UpstreamID]; ok {
			reason := fmt.Sprintf("public id %q collides with upstream id %q", other.PublicID, rec.UpstreamID)
			warns = append(warns, reason)
			unsup = append(unsup, UnsupportedModel{UpstreamID: e.ID, Reason: reason})
			continue
		}
		models = append(models, rec)
		index[rec.PublicID] = rec
		index[rec.UpstreamID] = rec
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.raw, m.models, m.index, m.unsup, m.warns = entries, models, index, unsup, warns
}

// resolve builds a model's targets: the family table patched by the model's
// declaration, whose entries switch protocols on or off. Endpoints take the
// model's path, then the global protocols path, then the protocol default.
// disabled lists the built-in protocols switched off.
func (m *Manager) resolve(id string, declared map[string]config.ProtocolOverride) (targets []Target, disabled []string, warn string) {
	routes := slices.Clone(familyRoutes(id))
	for _, r := range protocolOrder {
		if p, ok := declared[string(r)]; ok && p.IsEnabled() && !slices.Contains(routes, r) {
			routes = append(routes, r)
		}
	}
	parts := make([]string, 0, len(routes))
	for _, r := range routes {
		p := declared[string(r)]
		if !p.IsEnabled() {
			disabled = append(disabled, string(r))
			continue
		}
		t := Target{Protocol: r, EndpointPath: cmp.Or(p.Endpoint, m.cfg.Protocols[string(r)].Endpoint, r.EndpointPath())}
		targets = append(targets, t)
		parts = append(parts, string(r)+" "+t.EndpointPath)
	}
	if len(declared) > 0 && len(targets) > 0 {
		warn = fmt.Sprintf("model %q routed via user configuration (%s)", id, strings.Join(parts, ", "))
	}
	return targets, disabled, warn
}

// routable drops targets whose URL leaves any configured upstream host. With
// nothing left it returns the exclusion reason.
func (m *Manager) routable(id string, targets []Target) ([]Target, string, []string) {
	const escapes = "resolved endpoint escapes the configured upstream host"
	routes := make([]Target, 0, len(targets))
	var warns []string
	for _, t := range targets {
		// User endpoints flow through here, so they cannot rewrite the URL
		// host — e.g. "/v1@evil.com/x" against a bare-host base URL turns
		// the base into userinfo and would send credentials to another host.
		escaped := false
		keys := append([]config.APIKey{{Region: config.RegionGlobal}, {Region: config.RegionCN}}, m.cfg.APIKeys...)
		for _, key := range keys {
			if endpointEscapesBase(m.cfg.UpstreamBaseURL(key), t) {
				escaped = true
				break
			}
		}
		if escaped {
			warns = append(warns, fmt.Sprintf("model %q %s: %s", id, t.Protocol, escapes))
			continue
		}
		routes = append(routes, t)
	}
	if len(routes) == 0 {
		return nil, escapes, nil
	}
	return routes, "", warns
}

// JoinUpstreamURL joins a base URL onto an endpoint path and is the single
// source of truth for that join; endpointEscapesBase parses exactly the URL
// this produces.
func JoinUpstreamURL(baseURL, endpoint string) string {
	return strings.TrimSuffix(baseURL, "/") + endpoint
}

// endpointEscapesBase reports whether the target's URL would send the request
// to a different authority or scheme than baseURL. Any parse failure counts
// as escaping (fail closed).
func endpointEscapesBase(baseURL string, t Target) bool {
	base, err := url.Parse(baseURL)
	if err != nil {
		return true
	}
	got, err := url.Parse(t.URL(baseURL))
	if err != nil {
		return true
	}
	return !strings.EqualFold(got.Host, base.Host) || got.Scheme != base.Scheme
}

// Models returns the routable snapshot. The slice is a shallow copy; records
// are rebuilt fresh on every swap and consumers treat them as read-only.
func (m *Manager) Models() []ModelRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ModelRecord, len(m.models))
	copy(out, m.models)
	return out
}

// Lookup resolves an ID to its routable record, accepting the public ID or
// the bare upstream ID; the PublicID key wins when both exist.
func (m *Manager) Lookup(id string) (ModelRecord, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.index[id]
	return rec, ok
}

// Unsupported returns models excluded from the routable set with reasons.
func (m *Manager) Unsupported() []UnsupportedModel {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]UnsupportedModel, len(m.unsup))
	copy(out, m.unsup)
	return out
}

// Warnings returns dedup and override diagnostics.
func (m *Manager) Warnings() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.warns))
	copy(out, m.warns)
	return out
}
