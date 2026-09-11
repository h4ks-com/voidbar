// Package connstr parses IRC connection strings - the Voidbar equivalent of
// a Discord invite. A network is defined solely by its string:
//
//	ircs://irc.libera.chat:6697/#go,#rust?name=Libera
//
// Identity is host:port (+ TLS from the scheme or ?tls=); name and channels
// are display/join metadata and do not change the identity.
package connstr

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var (
	ErrInvalid    = errors.New("invalid IRC connection string")
	ErrNoHost     = errors.New("connection string has no host")
	ErrBadPort    = errors.New("invalid port")
	ErrBadChannel = errors.New("invalid channel")
	ErrBadNick    = errors.New("invalid nick")
)

var nickRe = regexp.MustCompile(`^[A-Za-z0-9_\-\[\]\\` + "`" + `^{}|]{1,32}$`)

// Conn is a parsed connection string.
type Conn struct {
	Host     string
	Port     int
	TLS      bool
	Name     string // display name, optional
	Password string // server password, optional
	Channels []string
	// Nick is the IRC nickname to connect with (?nick=voidsnm);
	// empty falls back to the bouncer account username.
	Nick string
	// ChannelKeys carries per-channel +k keys from the inline
	// "#chan:key" token syntax, keyed by lowercased channel name.
	ChannelKeys map[string]string
	// SASL credentials (?sasl=user:pass): SASL PLAIN account auth,
	// used instead of the server password when present.
	SASLUser string
	SASLPass string
}

// Parse parses an IRC connection string.
func Parse(raw string) (*Conn, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ErrInvalid
	}

	// Tolerate a missing scheme, e.g. "irc.libera.chat:6697/#go".
	hasScheme := strings.Contains(raw, "://")
	if !hasScheme {
		raw = "irc://" + raw
	}

	// A '#' while a query is already open belongs to the last value:
	// url.Parse would treat it as the fragment separator, cutting a
	// pasted password ("?sasl=u:pa#ss") at the first '#'. The canonical
	// channel form puts the list before the query ("host/#chan?a=b") or
	// carries its own query ("host?a=b#chan?c=d"); a bare tail after an
	// open query is a value, so the '#'s fold back (escaped, and the
	// tolerant parser unescapes them).
	if q := strings.IndexByte(raw, '?'); q >= 0 {
		if h := strings.IndexByte(raw[q+1:], '#'); h >= 0 {
			h += q + 1
			if !strings.Contains(raw[h+1:], "?") {
				raw = raw[:h] + strings.ReplaceAll(raw[h:], "#", "%23")
			}
		}
	}

	u, err := url.Parse(raw)
	if err != nil {
		return nil, ErrInvalid
	}

	c := &Conn{}
	switch u.Scheme {
	case "irc":
		c.TLS = false
	case "ircs":
		c.TLS = true
	default:
		return nil, fmt.Errorf("%w: unknown scheme %q", ErrInvalid, u.Scheme)
	}

	if u.Hostname() == "" {
		return nil, ErrNoHost
	}
	c.Host = u.Hostname()

	if p := u.Port(); p != "" {
		port, err := strconv.Atoi(p)
		if err != nil || port < 1 || port > 65535 {
			return nil, ErrBadPort
		}
		c.Port = port
	} else if c.TLS {
		c.Port = 6697
	} else {
		c.Port = 6667
	}

	if pw, ok := u.User.Password(); ok {
		c.Password = pw
	}

	// IRC channels live after '#', which URL syntax treats as a fragment:
	// "irc://host/#go,#rust?name=Libera". So the channel list comes from the
	// fragment when present (its own trailing query split off), otherwise
	// from the path.
	channelSource := strings.TrimPrefix(u.Path, "/")
	fragParams := ""
	if u.Fragment != "" {
		frag := u.Fragment
		if i := strings.IndexByte(frag, '?'); i >= 0 {
			fragParams = frag[i+1:]
			frag = frag[:i]
		}
		// url.Parse drops the leading '#', so fragment channels need it back.
		channelSource = "#" + frag
	}
	for _, ch := range strings.Split(channelSource, ",") {
		ch = strings.TrimSpace(ch)
		if ch == "" {
			continue
		}
		// Inline +k key: "#chan:secret". The first colon splits - channel
		// names never contain one, keys may.
		key := ""
		if i := strings.IndexByte(ch, ':'); i >= 0 {
			ch, key = ch[:i], ch[i+1:]
		}
		if !strings.HasPrefix(ch, "#") && !strings.HasPrefix(ch, "&") {
			return nil, fmt.Errorf("%w: %q", ErrBadChannel, ch)
		}
		c.Channels = append(c.Channels, ch)
		if key != "" {
			if c.ChannelKeys == nil {
				c.ChannelKeys = map[string]string{}
			}
			c.ChannelKeys[strings.ToLower(ch)] = key
		}
	}

	// Query params, plus those split from the fragment tail. The query
	// is parsed tolerantly: a raw '&' inside a value (SASL passwords
	// are full of them) must not cut the value short.
	q := parseQueryTolerant(u.RawQuery)
	if fragParams != "" {
		for k, v := range parseQueryTolerant(fragParams) {
			if _, exists := q[k]; !exists {
				q[k] = v
			}
		}
	}
	if name := q["name"]; name != "" {
		c.Name = name
	}
	if nick := q["nick"]; nick != "" {
		if !nickRe.MatchString(nick) {
			return nil, fmt.Errorf("%w: %q", ErrBadNick, nick)
		}
		c.Nick = nick
	}
	if v := q["tls"]; v != "" && (v == "1" || v == "true") {
		c.TLS = true
	}
	if p := q["port"]; p != "" {
		port, err := strconv.Atoi(p)
		if err != nil || port < 1 || port > 65535 {
			return nil, ErrBadPort
		}
		c.Port = port
	}
	if sasl := q["sasl"]; sasl != "" {
		// SASL PLAIN credentials: "user:pass", first colon splits
		// (passwords may contain colons, usernames may not).
		if i := strings.IndexByte(sasl, ':'); i > 0 {
			c.SASLUser, c.SASLPass = sasl[:i], sasl[i+1:]
		} else {
			return nil, fmt.Errorf("%w: sasl must be user:pass", ErrInvalid)
		}
	}
	return c, nil
}

// knownQueryKeys are the parameters Parse understands; a query segment
// with one of these keys starts a new pair, anything else belongs to
// the previous value.
var knownQueryKeys = map[string]bool{
	"name": true, "nick": true, "tls": true, "port": true, "sasl": true,
}

// parseQueryTolerant splits a raw query the way users paste it: a
// value containing a raw '&' ("?sasl=u:p&ss!x") would lose its tail to
// url.ParseQuery, so segments that don't start a known pair rejoin the
// previous value with the literal '&'. Values unescape %XX via
// PathUnescape - QueryUnescape would silently turn passwords' '+' into
// spaces - and malformed escapes stay raw rather than failing the
// whole string.
func parseQueryTolerant(raw string) map[string]string {
	out := map[string]string{}
	lastKey := ""
	for _, seg := range strings.Split(raw, "&") {
		k, v, hasEq := strings.Cut(seg, "=")
		if hasEq && knownQueryKeys[strings.ToLower(k)] {
			lastKey = strings.ToLower(k)
			out[lastKey] = unescapeLoose(v)
			continue
		}
		if lastKey != "" {
			out[lastKey] += "&" + unescapeLoose(seg)
		}
	}
	return out
}

// unescapeLoose path-unescapes, falling back to the raw text for
// malformed sequences.
func unescapeLoose(s string) string {
	if u, err := url.PathUnescape(s); err == nil {
		return u
	}
	return s
}

// ID returns the canonical, identity-determining form: host:port + TLS flag.
// Everything else (name, channels, password) is per-instance metadata.
func (c *Conn) ID() string {
	scheme := "irc"
	if c.TLS {
		scheme = "ircs"
	}
	return fmt.Sprintf("%s://%s:%d", scheme, c.Host, c.Port)
}

// DisplayName returns the configured name or a sensible default.
func (c *Conn) DisplayName() string {
	if c.Name != "" {
		return c.Name
	}
	return c.Host
}
