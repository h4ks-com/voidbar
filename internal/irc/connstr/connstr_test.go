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
