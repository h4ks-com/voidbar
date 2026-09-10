package ircmanage

import "strings"

// FormatIRC shapes inbound IRC text for the Discord side: CTCP ACTION
// becomes an italic action line, mIRC formatting control bytes become
// the Discord markdown subset (bold/italic/underline/strikethrough/mono
// - Discord renders no text colors, so color codes are dropped). Each
// IRC line carries independent formatting state, so the state resets on
// newlines (a joined multiline body behaves like the separate PRIVMSGs
// it was).
func FormatIRC(s string) string {
	if !strings.ContainsAny(s, "\x01\x02\x03\x04\x0f\x11\x16\x1d\x1e\x1f") {
		return s
	}
	action := false
	if strings.HasPrefix(s, "\x01") {
		body := strings.TrimSuffix(strings.TrimPrefix(s, "\x01"), "\x01")
		if body == "ACTION" || strings.HasPrefix(body, "ACTION ") {
			action = true
			s = strings.TrimPrefix(body[6:], " ")
		} else {
			// Some other CTCP request/reply: keep the payload, drop
			// the delimiters.
			s = body
		}
	}
	s = mircToMarkdown(s)
	if action {
		s = "*" + s + "*"
	}
	return s
}

// mIRC formatting control bytes (RFC-ish de-facto set) mapped to their
// Discord markdown delimiters.
var mdDelims = map[byte]string{
	0x02: "**", // bold
	0x1d: "*",  // italics
	0x1f: "__", // underline
	0x1e: "~~", // strikethrough
	0x11: "`",  // monospace
}

// mircToMarkdown converts mIRC control bytes to markdown delimiters.
// mIRC toggles formats independently (bold on, italic on, bold off...),
// but markdown spans must nest - closing a non-innermost format closes
// and reopens everything stacked above it, keeping every delimiter
// paired. Empty spans (a toggle with no text after it, the classic
// trailing control byte) are suppressed instead of rendering as stray
// delimiters. Colors (0x03 decimal pairs, 0x04 hex pairs) are consumed
// and dropped; reverse (0x16) has no markdown counterpart and is
// dropped; reset (0x0f) closes everything open.
func mircToMarkdown(s string) string {
	var b []byte
	type span struct {
		code byte
		pos  int // len(b) right before this span's opening delimiter
	}
	var stack []span
	open := func(code byte) {
		stack = append(stack, span{code: code, pos: len(b)})
		b = append(b, mdDelims[code]...)
	}
	// closeAt closes span idx (and only it); spans above are closed
	// and reopened around it so nesting stays balanced. An empty span
	// (nothing written since it opened - and thus nothing in the spans
	// above either) is cut out wholesale.
	closeAt := func(idx int) {
		if len(b) == stack[idx].pos+len(mdDelims[stack[idx].code]) {
			pos := stack[idx].pos
			stack = stack[:idx]
			b = b[:pos]
			return
		}
		for i := len(stack) - 1; i > idx; i-- {
			b = append(b, mdDelims[stack[i].code]...)
		}
		b = append(b, mdDelims[stack[idx].code]...)
		above := append([]span{}, stack[idx+1:]...)
		stack = append(stack[:idx], above...)
		for i := range above {
			above[i].pos = len(b)
			stack[idx+i] = above[i]
			b = append(b, mdDelims[above[i].code]...)
		}
	}
	toggle := func(code byte) {
		for i := range stack {
			if stack[i].code == code {
				closeAt(i)
				return
			}
		}
		open(code)
	}
	reset := func() {
		for i := len(stack) - 1; i >= 0; i-- {
			if len(b) == stack[i].pos+len(mdDelims[stack[i].code]) {
				// empty tail span: cut it (and the empty spans above)
				b = b[:stack[i].pos]
				stack = stack[:i]
				continue
			}
			b = append(b, mdDelims[stack[i].code]...)
		}
		stack = stack[:0]
	}
	for i := 0; i < len(s); {
		ch := s[i]
		switch ch {
		case 0x02, 0x11, 0x1d, 0x1e, 0x1f:
			toggle(ch)
			i++
		case 0x0f:
			reset()
			i++
		case 0x16:
			i++
		case 0x03:
			i += 1 + mircColorLen(s[i+1:])
		case 0x04:
			i += 1 + mircHexLen(s[i+1:])
		case '\n':
			// One PRIVMSG per line: state does not cross lines.
			reset()
			b = append(b, '\n')
			i++
		default:
			b = append(b, ch)
			i++
		}
	}
	reset()
	return string(b)
}

// mircColorLen measures the 0x03 color parameters to skip: one or two
// decimal digits, optionally ",<one or two digits>".
func mircColorLen(rest string) int {
	n := 0
	digits := 0
	for n < len(rest) && digits < 2 && rest[n] >= '0' && rest[n] <= '9' {
		n++
		digits++
	}
	if digits == 0 {
		return 0
	}
	if n < len(rest) && rest[n] == ',' {
		m := n + 1
		bg := 0
		for m < len(rest) && bg < 2 && rest[m] >= '0' && rest[m] <= '9' {
			m++
			bg++
		}
		if bg > 0 {
			n = m
		}
	}
	return n
}

// mircHexLen measures the 0x04 hex color parameters to skip: six hex
// digits, optionally ",<six hex digits>".
func mircHexLen(rest string) int {
	isHex := func(c byte) bool {
		return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
	}
	n := 0
	for n < 6 && n < len(rest) && isHex(rest[n]) {
		n++
	}
	if n == 0 {
		return 0
	}
	if n < len(rest) && rest[n] == ',' {
		m := n + 1
		bg := 0
		for m < len(rest) && bg < 6 && isHex(rest[m]) {
			m++
			bg++
		}
		if bg > 0 {
			n = m
		}
	}
	return n
}

