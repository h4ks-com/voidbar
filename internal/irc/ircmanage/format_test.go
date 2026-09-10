package ircmanage

import (
	"strings"
	"testing"
)

func TestMircToMarkdown(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"\x02bold\x02 text", "**bold** text"},
		{"\x1ditalic\x1d", "*italic*"},
		{"\x1funder\x1f", "__under__"},
		{"\x1estrike\x1e", "~~strike~~"},
		{"\x11mono\x11", "`mono`"},
		{"\x02bo\x1dld it\x02al\x1d", "**bo*ld it****al*"}, // overlap stays balanced
		{"\x02a\x0fb", "**a**b"},                         // reset closes
		{"\x02unclosed", "**unclosed**"},                  // dangling closes at end
		{"a\x03", "a"},                                    // bare color byte
		{"\x034red\x03 plain", "red plain"},               // fg color
		{"\x031,2fg bg\x03 back", "fg bg back"},
		{"\x0312,10x\x03 y", "x y"}, // two-digit colors
		{"\x04FF0000hex\x04 after", "hex after"},
		{"\x04ABCDEF,000000x\x04 y", "x y"},
		{"\x16reverse\x16", "reverse"}, // no markdown counterpart
		{"\x02line one\nplain\x02", "**line one**\nplain"}, // state per line
		{"\x02a\x02\x02b\x02", "**a****b**"},
	}
	for _, c := range cases {
		if got := mircToMarkdown(c.in); got != c.want {
			t.Errorf("mircToMarkdown(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMircBalanced(t *testing.T) {
	// Whatever the interleaving, delimiters must pair: every markdown
	// delimiter is a repetition of one marker character, so parity of
	// total marker characters pins it (substring counting would trip
	// over * vs ** adjacencies).
	in := "\x02a\x1db\x02c\x1fd\x02e\x1ff\x1dg\x02h\x11i\x1ej\x11k\x1el"
	out := mircToMarkdown(in)
	for _, marker := range []string{"*", "_", "~", "`"} {
		if n := strings.Count(out, marker); n%2 != 0 {
			t.Errorf("marker %q unpaired (%d) in %q", marker, n, out)
		}
	}
}

func TestFormatIRCTypes(t *testing.T) {
	if got := FormatIRC("\x01ACTION waves goodbye\x01"); got != "*waves goodbye*" {
		t.Errorf("ACTION = %q", got)
	}
	if got := FormatIRC("\x01ACTION\x01"); got != "**" { // empty action body
		t.Errorf("bare ACTION = %q", got)
	}
	if got := FormatIRC("\x01VERSION\x01"); got != "VERSION" {
		t.Errorf("other CTCP = %q", got)
	}
	if got := FormatIRC("\x01ACTION \x02runs\x02\x01"); got != "***runs***" {
		t.Errorf("ACTION + bold = %q", got)
	}
	if got := FormatIRC("\x02bold\x02 and \x034colored"); got != "**bold** and colored" {
		t.Errorf("mixed = %q", got)
	}
	if got := FormatIRC("plain text"); got != "plain text" {
		t.Errorf("plain passthrough = %q", got)
	}
}
