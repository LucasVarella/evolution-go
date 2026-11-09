package chatwoot

import "testing"

func TestWhatsAppToChatwoot(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"plain text", "plain text"},
		{"*bold*", "**bold**"},
		{"_italic_", "*italic*"},
		{"~strike~", "~~strike~~"},
		{"```code```", "`code`"},
		{"*bold* and _italic_", "**bold** and *italic*"},
		{"*bold with spaces*", "**bold with spaces**"},
		{"5 * 3 * 2", "5 * 3 * 2"},             // spaces next to markers: not formatting
		{"snake_case_name", "snake_case_name"}, // WhatsApp ignores markers inside words
		{"*a* *b*", "**a** **b**"},
		{"(*bold*)", "(**bold**)"},
		{"*line\nbreak*", "*line\nbreak*"},
		{"price: R$ 5*", "price: R$ 5*"},
	}
	for _, c := range cases {
		if got := WhatsAppToChatwoot(c.in); got != c.want {
			t.Errorf("WhatsAppToChatwoot(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestChatwootToWhatsApp(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"plain text", "plain text"},
		{"**bold**", "*bold*"},
		{"*italic*", "_italic_"},
		{"~~strike~~", "~strike~"},
		{"`code`", "```code```"},
		{"```already code```", "```already code```"},
		{"**bold** and *italic*", "*bold* and _italic_"},
		{"5 * 3", "5 * 3"},
		{"line one\nline two", "line one\nline two"},
	}
	for _, c := range cases {
		if got := ChatwootToWhatsApp(c.in); got != c.want {
			t.Errorf("ChatwootToWhatsApp(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormattingRoundTrip(t *testing.T) {
	for _, s := range []string{"*bold*", "_italic_", "~strike~", "```code```", "*a* _b_ ~c~"} {
		if got := ChatwootToWhatsApp(WhatsAppToChatwoot(s)); got != s {
			t.Errorf("round trip of %q gave %q", s, got)
		}
	}
}
