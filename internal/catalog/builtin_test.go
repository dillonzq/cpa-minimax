package catalog

import (
	"reflect"
	"testing"
)

func TestBuiltinTableCoversPublishedModels(t *testing.T) {
	for _, id := range publishedModels {
		meta := builtinMetadata(id)
		if meta.DisplayName == nil || meta.Context == nil || meta.Output == nil || meta.Thinking == nil {
			t.Errorf("%s: incomplete builtin metadata %+v", id, meta)
		}
		if familyRoutes(id) == nil {
			t.Errorf("%s: published model has no family route", id)
		}
	}
	if got := builtinMetadata("MINIMAX-M3"); got.DisplayName == nil || *got.DisplayName != "MiniMax M3" {
		t.Errorf("lookup must be case-insensitive: %+v", got)
	}
	if got := builtinMetadata("unknown"); got.DisplayName != nil || got.Thinking != nil {
		t.Errorf("unknown model metadata = %+v", got)
	}
}

// The M3 none level must not leak into Flash's shared level slice.
func TestBuiltinLevelsAreIndependent(t *testing.T) {
	flash := builtinMetadata("MiniMax-M3.1-Flash").Thinking.Levels
	m3 := builtinMetadata("MiniMax-M3").Thinking.Levels
	if !reflect.DeepEqual(flash, []string{"low", "medium", "high", "xhigh", "max"}) {
		t.Fatalf("flash levels = %v", flash)
	}
	if !reflect.DeepEqual(m3, []string{"none", "low", "medium", "high", "xhigh", "max"}) {
		t.Fatalf("m3 levels = %v", m3)
	}
}

func TestFamilyRoutes(t *testing.T) {
	want := []Route{RouteMessages, RouteChatCompletions, RouteResponses}
	for _, id := range []string{"MiniMax-M3", "minimax-m2.7", "MiniMax-M3.1-Flash", "MINIMAX-M4-preview"} {
		if got := familyRoutes(id); !reflect.DeepEqual(got, want) {
			t.Errorf("familyRoutes(%q) = %v", id, got)
		}
	}
	for _, id := range []string{"speech-2.6-hd", "hailuo-02", "image-01", "MiniMax-Text-01", "abab6.5s-chat", ""} {
		if got := familyRoutes(id); got != nil {
			t.Errorf("familyRoutes(%q) = %v, want nil", id, got)
		}
	}
}

func TestThinkingFamilies(t *testing.T) {
	cases := []struct {
		id               string
		always, adaptive bool
	}{
		{"MiniMax-M3.1-Flash", true, false},
		{"minimax-m3.1-flash-preview", true, false},
		{"MiniMax-M3", false, true},
		{"MiniMax-M3.2", false, true},
		{"MiniMax-M2.7", false, false},
		{"custom-model", false, false},
	}
	for _, c := range cases {
		if got := IsAlwaysThinking(c.id); got != c.always {
			t.Errorf("IsAlwaysThinking(%q) = %v", c.id, got)
		}
		if got := IsAdaptiveThinking(c.id); got != c.adaptive {
			t.Errorf("IsAdaptiveThinking(%q) = %v", c.id, got)
		}
	}
}
