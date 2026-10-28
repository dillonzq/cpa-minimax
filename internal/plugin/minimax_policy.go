package plugin

import (
	"encoding/json"
	"strings"

	"github.com/dillonzq/cpa-minimax/internal/catalog"
)

// normalizeMiniMaxRequest applies MiniMax compatibility rules to a translated
// upstream body. Run after translation and suffix application so the
// effective control wins. Rules follow the compatible-endpoint docs:
//   - Chat Completions: reasoning_split defaults to true, so thinking arrives
//     in reasoning_content instead of <think> tags inside content. M3.1 Flash
//     rejects false.
//   - M3.1 Flash: thinking is always on; thinking.type must be adaptive and
//     effort none is rejected, so none maps to the lowest level, as CPA does
//     for models that disallow zero.
//   - M3: Messages thinking is off unless requested. An effort enables
//     adaptive thinking; effort none disables it explicitly.
//
// Malformed bodies pass through; validation belongs to the protocol layer.
func normalizeMiniMaxRequest(route catalog.Route, model string, body []byte) []byte {
	always, adaptive := catalog.IsAlwaysThinking(model), catalog.IsAdaptiveThinking(model)
	if route != catalog.RouteChatCompletions && !always && !adaptive {
		return body
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(body, &doc) != nil || doc == nil {
		return body
	}
	effort := newEffortField(route, doc)
	switch route {
	case catalog.RouteChatCompletions:
		if raw, ok := doc["reasoning_split"]; !ok || always && string(raw) != "true" {
			doc["reasoning_split"] = json.RawMessage(`true`)
		}
		if always {
			if thinkingType(doc["thinking"]) == "disabled" {
				delete(doc, "thinking")
			}
			effort.raiseNone()
		}
	case catalog.RouteMessages:
		typ := thinkingType(doc["thinking"])
		switch {
		case always:
			effort.raiseNone()
			if _, ok := doc["thinking"]; ok && typ != "adaptive" {
				doc["thinking"] = json.RawMessage(`{"type":"adaptive"}`)
			}
		case effort.value() == "none":
			effort.remove()
			doc["thinking"] = json.RawMessage(`{"type":"disabled"}`)
		case typ == "enabled" || typ == "" && effort.value() != "":
			// Budget-style thinking from other protocols maps to adaptive;
			// the effort carries the depth.
			doc["thinking"] = json.RawMessage(`{"type":"adaptive"}`)
		}
	case catalog.RouteResponses:
		if always {
			effort.raiseNone()
		}
	}
	effort.store(doc)
	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return out
}

func thinkingType(raw json.RawMessage) string {
	var thinking struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(raw, &thinking)
	return strings.ToLower(strings.TrimSpace(thinking.Type))
}

// effortField locates a route's effort control: Chat reasoning_effort,
// Messages output_config.effort, or Responses reasoning.effort.
type effortField struct {
	parent    string // "" for a top-level field
	key       string
	container map[string]json.RawMessage
	changed   bool
}

func newEffortField(route catalog.Route, doc map[string]json.RawMessage) *effortField {
	f := &effortField{container: doc}
	switch route {
	case catalog.RouteMessages:
		f.parent, f.key = "output_config", "effort"
	case catalog.RouteResponses:
		f.parent, f.key = "reasoning", "effort"
	default:
		f.key = "reasoning_effort"
	}
	if f.parent != "" {
		var nested map[string]json.RawMessage
		if json.Unmarshal(doc[f.parent], &nested) != nil {
			nested = nil
		}
		f.container = nested
	}
	return f
}

func (f *effortField) value() string {
	if f.container == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(jsonString(f.container[f.key])))
}

func (f *effortField) raiseNone() {
	if f.value() == "none" {
		f.container[f.key] = json.RawMessage(`"low"`)
		f.changed = true
	}
}

func (f *effortField) remove() {
	if f.container != nil {
		delete(f.container, f.key)
		f.changed = true
	}
}

func (f *effortField) store(doc map[string]json.RawMessage) {
	if !f.changed || f.parent == "" {
		return
	}
	if len(f.container) == 0 {
		delete(doc, f.parent)
		return
	}
	doc[f.parent], _ = json.Marshal(f.container)
}
