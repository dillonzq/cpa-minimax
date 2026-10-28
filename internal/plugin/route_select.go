package plugin

import "github.com/dillonzq/cpa-minimax/internal/catalog"

// routePreference orders upstream protocols for each host input format: the
// format's own protocol first (no conversion), then the others by increasing
// semantic loss of the request conversion (apix docs/protocols/llm.md).
// Gemini has no MiniMax endpoint; it converts to Messages, which MiniMax
// recommends for thinking blocks and interleaved thinking.
var routePreference = map[string][]catalog.Route{
	"openai":          {catalog.RouteChatCompletions, catalog.RouteResponses, catalog.RouteMessages},
	"openai-response": {catalog.RouteResponses, catalog.RouteChatCompletions, catalog.RouteMessages},
	"claude":          {catalog.RouteMessages, catalog.RouteResponses, catalog.RouteChatCompletions},
	"gemini":          {catalog.RouteMessages, catalog.RouteResponses, catalog.RouteChatCompletions},
}

// selectRoute picks the record's upstream for one request. An unknown format
// takes the primary route so conversion reports the unsupported format.
func selectRoute(rec catalog.ModelRecord, format string) catalog.Target {
	for _, route := range routePreference[format] {
		if t, ok := rec.Target(route); ok {
			return t
		}
	}
	return rec.Routes[0]
}
