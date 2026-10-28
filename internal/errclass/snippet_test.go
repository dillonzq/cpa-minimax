package errclass

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRedactedSnippetKeepsUTF8(t *testing.T) {
	got := RedactedSnippet("a" + strings.Repeat("余额不足", 100))
	if !utf8.ValidString(got) {
		t.Fatalf("snippet split a rune: %q", got)
	}
}
