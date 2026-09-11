package connstr

import "testing"

func TestParseBasic(t *testing.T) {
	c, err := Parse("ircs://irc.libera.chat:6697/#go,#rust?name=Libera")
	if err != nil {
		t.Fatal(err)
	}
	if c.Host != "irc.libera.chat" || c.Port != 6697 || !c.TLS {
		t.Fatalf("conn: %+v", c)
	}
	if c.DisplayName() != "Libera" {
		t.Fatalf("name: %q", c.DisplayName())
	}
	if len(c.Channels) != 2 || c.Channels[0] != "#go" || c.Channels[1] != "#rust" {
		t.Fatalf("channels: %v", c.Channels)
	}
	if c.ID() != "ircs://irc.libera.chat:6697" {
		t.Fatalf("id: %q", c.ID())
	}
}

func TestParseDefaults(t *testing.T) {
	c, err := Parse("irc.libera.chat")
	if err != nil {
		t.Fatal(err)
	}
	if c.TLS || c.Port != 6667 {
		t.Fatalf("expected plain 6667, got %+v", c)
	}
	c2, err := Parse("ircs://example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !c2.TLS || c2.Port != 6697 {
		t.Fatalf("expected tls 6697, got %+v", c2)
	}
}

func TestParsePortQueryOverride(t *testing.T) {
	c, err := Parse("ircs://example.com?port=7000")
	if err != nil {
		t.Fatal(err)
	}
	if c.Port != 7000 {
		t.Fatalf("port: %d", c.Port)
	}
}

func TestParsePassword(t *testing.T) {
	c, err := Parse("irc://nick:hunter2@example.com:6667/#chan")
	if err != nil {
		t.Fatal(err)
	}
	if c.Password != "hunter2" {
		t.Fatalf("password: %q", c.Password)
	}
}

func TestParseErrors(t *testing.T) {
	for _, raw := range []string{"", "ftp://x", "irc://", "irc://host:0", "irc://host:99999", "irc://host/notachannel"} {
		if _, err := Parse(raw); err == nil {
			t.Fatalf("expected error for %q", raw)
		}
	}
}

func TestIdentityIgnoresNameAndChannels(t *testing.T) {
	a, _ := Parse("ircs://irc.libera.chat:6697/#go?name=Libera")
	b, _ := Parse("ircs://irc.libera.chat:6697/#rust")
	if a.ID() != b.ID() {
		t.Fatalf("same server should share identity: %q vs %q", a.ID(), b.ID())
	}
}

func TestParseChannelKeys(t *testing.T) {
	c, err := Parse("ircs://irc.libera.chat:6697/#open,#locked:hunter2,#odd:two:parts?name=L")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Channels) != 3 || c.Channels[0] != "#open" || c.Channels[1] != "#locked" || c.Channels[2] != "#odd" {
		t.Fatalf("channels: %v", c.Channels)
	}
	if len(c.ChannelKeys) != 2 || c.ChannelKeys["#locked"] != "hunter2" || c.ChannelKeys["#odd"] != "two:parts" {
		t.Fatalf("keys: %v", c.ChannelKeys)
	}
	// Keys are per-instance metadata, not identity: same conn id as the
	// bare string.
	bare, _ := Parse("ircs://irc.libera.chat:6697/#open")
	if c.ID() != bare.ID() {
		t.Fatalf("keyed id %q != bare %q", c.ID(), bare.ID())
	}
}

func TestParseSASL(t *testing.T) {
	c, err := Parse("ircs://irc.libera.chat:6697/#go?sasl=acct:p%40ss")
	if err != nil {
		t.Fatal(err)
	}
	if c.SASLUser != "acct" || c.SASLPass != "p@ss" {
		t.Fatalf("sasl = %q / %q", c.SASLUser, c.SASLPass)
	}
	// Credentials are metadata, not identity.
	bare, _ := Parse("ircs://irc.libera.chat:6697/#go")
	if c.ID() != bare.ID() {
		t.Fatalf("sasl id %q != bare %q", c.ID(), bare.ID())
	}
	if _, err := Parse("ircs://h/#a?sasl=nocolon"); err == nil {
		t.Fatal("sasl without colon accepted")
	}
}

// SASL values with ampersands: percent-encoded (%26), raw ('&') and
// both mixed must all yield the same password - url.ParseQuery would
// cut the raw forms at the separator.
func TestParseSASLAmpersand(t *testing.T) {
	cases := []struct {
		raw  string
		pass string
	}{
		{"ircs://h/#a?sasl=acct:p%26ss", "p&ss"},
		{"ircs://h/#a?sasl=acct:p&ss", "p&ss"},
		{"ircs://h/#a?sasl=acct:p%26ss&more", "p&ss&more"},
		{"ircs://h/#a?sasl=acct:p&ss&more", "p&ss&more"},
	}
	for _, c := range cases {
		parsed, err := Parse(c.raw)
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.raw, err)
		}
		if parsed.SASLUser != "acct" || parsed.SASLPass != c.pass {
			t.Fatalf("Parse(%q) = %q / %q, want pass %q", c.raw, parsed.SASLUser, parsed.SASLPass, c.pass)
		}
	}
	// A raw '&' tail still lets later known params parse.
	c, err := Parse("ircs://h/#a?sasl=acct:p&ss&name=X")
	if err != nil || c.SASLPass != "p&ss" || c.Name != "X" {
		t.Fatalf("mixed params: %+v err=%v", c, err)
	}
	// '+' in passwords stays literal (QueryUnescape would space it).
	c, err = Parse("ircs://h/#a?sasl=acct:p+ss")
	if err != nil || c.SASLPass != "p+ss" {
		t.Fatalf("plus: %q err=%v", c.SASLPass, err)
	}
}

// TestParseSASLHash: '#' in a value while a query is open folds into
// the value - url.Parse would cut the password at the first '#' and
// read the tail as a channel. The canonical channel forms (list before
// the query, or a fragment carrying its own '?') keep winning.
func TestParseSASLHash(t *testing.T) {
	cases := []struct {
		raw   string
		pass  string
		chans []string
	}{
		{"ircs://h?sasl=u:pa#ss", "pa#ss", nil},
		{"ircs://h?name=N&sasl=u:pa#ss&more", "pa#ss&more", nil},
		{"ircs://h?sasl=u:p#chan?nick=x", "p", []string{"#chan"}}, // fragment query wins
		{"ircs://h/#chan?sasl=u:pa#ss", "pa#ss", []string{"#chan"}}, // '#' inside fragment-query
	}
	for _, tc := range cases {
		c, err := Parse(tc.raw)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.raw, err)
		}
		if c.SASLPass != tc.pass {
			t.Fatalf("Parse(%q) pass = %q, want %q", tc.raw, c.SASLPass, tc.pass)
		}
		if len(c.Channels) != len(tc.chans) {
			t.Fatalf("Parse(%q) channels = %v, want %v", tc.raw, c.Channels, tc.chans)
		}
		for i, ch := range tc.chans {
			if c.Channels[i] != ch {
				t.Fatalf("Parse(%q) channels = %v, want %v", tc.raw, c.Channels, tc.chans)
			}
		}
	}
	// The folded form leaves no phantom channel.
	c, err := Parse("ircs://h?sasl=u:pa#ss")
	if err != nil || len(c.Channels) != 0 {
		t.Fatalf("phantom channels: %v err=%v", c.Channels, err)
	}
}
