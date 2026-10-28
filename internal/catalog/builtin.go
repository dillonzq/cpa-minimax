package catalog

import (
	"strings"

	"github.com/dillonzq/cpa-minimax/internal/modelmeta"
)

// publishedModels are always routable, even without discovery or when the
// listing is unavailable to a subscription key. Retiring M2.x models are
// published only when discovery lists them or the user declares them.
var publishedModels = []string{"MiniMax-M3.1-Flash", "MiniMax-M3"}

// Effort levels accepted by the M3 family on every compatible endpoint
// (Messages output_config.effort, Chat reasoning_effort, Responses
// reasoning.effort).
var m3Levels = []string{"low", "medium", "high", "xhigh", "max"}

// builtinTable holds MiniMax's documented limits, keyed by lower-case ID.
// Unknown fields stay nil so CPA treats them as unknown rather than zero.
var builtinTable = map[string]modelmeta.Metadata{
	"minimax-m3.1-flash": {
		DisplayName: modelmeta.Ptr("MiniMax M3.1 Flash"),
		Description: modelmeta.Ptr("MiniMax M3.1 Flash; thinking is always on."),
		Context:     modelmeta.Ptr[int64](1_000_000),
		Output:      modelmeta.Ptr[int64](524_288),
		// Flash rejects disabled thinking and effort none with HTTP 400.
		Thinking: &modelmeta.Thinking{Levels: m3Levels, ZeroAllowed: modelmeta.Ptr(false)},
	},
	"minimax-m3": {
		DisplayName: modelmeta.Ptr("MiniMax M3"),
		Description: modelmeta.Ptr("MiniMax M3; thinking is adaptive and can be disabled."),
		Context:     modelmeta.Ptr[int64](1_000_000),
		Output:      modelmeta.Ptr[int64](524_288),
		Thinking:    &modelmeta.Thinking{Levels: append([]string{"none"}, m3Levels...), ZeroAllowed: modelmeta.Ptr(true)},
	},
	"minimax-m2.7": {
		DisplayName: modelmeta.Ptr("MiniMax M2.7"),
		Context:     modelmeta.Ptr[int64](204_800),
		Output:      modelmeta.Ptr[int64](204_800),
	},
	"minimax-m2.5": {
		DisplayName: modelmeta.Ptr("MiniMax M2.5"),
		Context:     modelmeta.Ptr[int64](204_800),
		Output:      modelmeta.Ptr[int64](204_800),
	},
}

// builtinMetadata returns the documented metadata for a model, if known.
func builtinMetadata(id string) modelmeta.Metadata {
	return builtinTable[strings.ToLower(id)]
}

// familyRoutes returns the default protocols for a model ID. MiniMax text
// models serve all three compatible endpoints; Messages comes first because
// MiniMax recommends it for thinking blocks and interleaved thinking.
func familyRoutes(id string) []Route {
	if strings.HasPrefix(strings.ToLower(id), "minimax-m") {
		return []Route{RouteMessages, RouteChatCompletions, RouteResponses}
	}
	return nil
}

// IsAlwaysThinking reports models that reject disabled thinking.
func IsAlwaysThinking(id string) bool {
	return strings.HasPrefix(strings.ToLower(id), "minimax-m3.1-flash")
}

// IsAdaptiveThinking reports models whose Messages thinking is off unless
// requested; effort controls alone do not enable it.
func IsAdaptiveThinking(id string) bool {
	lower := strings.ToLower(id)
	return strings.HasPrefix(lower, "minimax-m3") && !IsAlwaysThinking(lower)
}
