package cli

import (
	"io"
	"unicode/utf8"
)

// plainWriter keeps terminal control out of everything the command line
// prints. Much of it is text other computers chose: a host's name and its
// accounts, a paired device's name, a Switcher advertised on the network, a
// provider's error. Printed as they came, escape codes in them could rewrite
// what is on screen, retitle the window or set the clipboard. Line breaks
// and tabs stay; other control characters, and the marks that reorder text,
// are dropped, as are bytes that are not text.
type plainWriter struct {
	w       io.Writer
	pending []byte // the start of a character whose rest has not arrived
}

func plain(w io.Writer) io.Writer {
	if _, ok := w.(*plainWriter); ok {
		return w
	}
	return &plainWriter{w: w}
}

func (p *plainWriter) Write(b []byte) (int, error) {
	data := b
	if len(p.pending) > 0 {
		data = append(append([]byte(nil), p.pending...), b...)
		p.pending = p.pending[:0]
	}
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); {
		if !utf8.FullRune(data[i:]) {
			p.pending = append(p.pending, data[i:]...)
			break
		}
		r, size := utf8.DecodeRune(data[i:])
		if printable(r, size) {
			out = append(out, data[i:i+size]...)
		}
		i += size
	}
	if len(out) > 0 {
		if _, err := p.w.Write(out); err != nil {
			return 0, err
		}
	}
	return len(b), nil
}

func printable(r rune, size int) bool {
	switch {
	case r == '\n' || r == '\t':
		return true
	case r == utf8.RuneError && size == 1: // not text
		return false
	case r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0: // C0, DEL, C1
		return false
	case r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 || r == 0x200e || r == 0x200f || r == 0x061c: // reordering marks
		return false
	}
	return true
}
