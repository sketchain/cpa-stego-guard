package stego

import (
	"unicode/utf16"
	"unicode/utf8"
)

// decodeLiteral decodes a raw JSON string literal, quotes included, into runes.
// offsets[i] is the byte offset in raw where rune i starts and offsets[len]
// is the offset of the closing quote, so rune i is encoded by
// raw[offsets[i]:offsets[i+1]] whether it was written as UTF-8 or as an escape.
func decodeLiteral(raw []byte) (runes []rune, offsets []int, ok bool) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return nil, nil, false
	}
	end := len(raw) - 1
	runes = make([]rune, 0, end)
	offsets = make([]int, 0, end+1)
	for i := 1; i < end; {
		start := i
		c := raw[i]
		switch {
		case c == '\\':
			if i+1 >= end {
				return nil, nil, false
			}
			switch raw[i+1] {
			case '"', '\\', '/':
				runes = append(runes, rune(raw[i+1]))
				i += 2
			case 'b':
				runes = append(runes, '\b')
				i += 2
			case 'f':
				runes = append(runes, '\f')
				i += 2
			case 'n':
				runes = append(runes, '\n')
				i += 2
			case 'r':
				runes = append(runes, '\r')
				i += 2
			case 't':
				runes = append(runes, '\t')
				i += 2
			case 'u':
				r, ok := hex4(raw, i+2, end)
				if !ok {
					return nil, nil, false
				}
				i += 6
				if utf16.IsSurrogate(r) {
					if i+6 <= end && raw[i] == '\\' && raw[i+1] == 'u' {
						if low, ok := hex4(raw, i+2, end); ok {
							if pair := utf16.DecodeRune(r, low); pair != utf8.RuneError {
								r = pair
								i += 6
							}
						}
					}
				}
				runes = append(runes, r)
			default:
				return nil, nil, false
			}
		case c < utf8.RuneSelf:
			runes = append(runes, rune(c))
			i++
		default:
			r, size := utf8.DecodeRune(raw[i:end])
			runes = append(runes, r)
			i += size
		}
		offsets = append(offsets, start)
	}
	offsets = append(offsets, end)
	return runes, offsets, true
}

func hex4(raw []byte, at, end int) (rune, bool) {
	if at+4 > end {
		return 0, false
	}
	var r rune
	for _, c := range raw[at : at+4] {
		r <<= 4
		switch {
		case c >= '0' && c <= '9':
			r |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			r |= rune(c-'a') + 10
		case c >= 'A' && c <= 'F':
			r |= rune(c-'A') + 10
		default:
			return 0, false
		}
	}
	return r, true
}
