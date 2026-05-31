package ai

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// emojiRanges enumerates the Unicode ranges treated as emoji (plus the
// joiners/modifiers that form emoji sequences) for trimming purposes. The set
// is intentionally broad: it only governs what gets stripped from the very
// start and end of a reply, where over-trimming a stray symbol is harmless.
var emojiRanges = &unicode.RangeTable{
	R16: []unicode.Range16{
		{0x200d, 0x200d, 1}, // zero width joiner
		{0x20e3, 0x20e3, 1}, // combining enclosing keycap
		{0x2122, 0x2122, 1}, // trade mark
		{0x2139, 0x2139, 1}, // information
		{0x2194, 0x21aa, 1}, // arrows
		{0x231a, 0x231b, 1}, // watch, hourglass
		{0x2328, 0x2328, 1}, // keyboard
		{0x23cf, 0x23cf, 1}, // eject
		{0x23e9, 0x23f3, 1}, // media controls, clocks
		{0x23f8, 0x23fa, 1}, // pause/stop/record
		{0x24c2, 0x24c2, 1}, // circled M
		{0x25aa, 0x25ab, 1}, // small squares
		{0x25b6, 0x25b6, 1}, // play
		{0x25c0, 0x25c0, 1}, // reverse
		{0x25fb, 0x25fe, 1}, // squares
		{0x2600, 0x27bf, 1}, // misc symbols + dingbats
		{0x2934, 0x2935, 1}, // curved arrows
		{0x2b00, 0x2bff, 1}, // stars, shapes, arrows
		{0x3030, 0x3030, 1}, // wavy dash
		{0x303d, 0x303d, 1}, // part alternation mark
		{0x3297, 0x3297, 1}, // circled congratulation
		{0x3299, 0x3299, 1}, // circled secret
		{0xfe0f, 0xfe0f, 1}, // variation selector-16
	},
	R32: []unicode.Range32{
		{0x1f000, 0x1f02f, 1}, // mahjong tiles
		{0x1f0a0, 0x1f0ff, 1}, // playing cards
		{0x1f100, 0x1f1ff, 1}, // enclosed alphanumerics + regional indicators
		{0x1f200, 0x1f2ff, 1}, // enclosed ideographic supplement
		{0x1f300, 0x1f5ff, 1}, // misc symbols & pictographs
		{0x1f600, 0x1f64f, 1}, // emoticons
		{0x1f650, 0x1f67f, 1}, // ornamental dingbats
		{0x1f680, 0x1f6ff, 1}, // transport & map
		{0x1f700, 0x1f77f, 1}, // alchemical symbols
		{0x1f780, 0x1f7ff, 1}, // geometric shapes extended
		{0x1f800, 0x1f8ff, 1}, // supplemental arrows-c
		{0x1f900, 0x1f9ff, 1}, // supplemental symbols & pictographs
		{0x1fa00, 0x1faff, 1}, // symbols & pictographs extended-a
	},
}

// isEmojiRune reports whether r is an emoji or an emoji sequence component.
func isEmojiRune(r rune) bool {
	return unicode.Is(emojiRanges, r)
}

// trimEmoji removes any emoji (and the whitespace around them) from the start
// and end of s, leaving emoji in the interior untouched.
func trimEmoji(s string) string {
	for {
		s = strings.TrimSpace(s)
		r, size := utf8.DecodeRuneInString(s)
		if size == 0 || !isEmojiRune(r) {
			break
		}
		s = s[size:]
	}

	for {
		s = strings.TrimSpace(s)
		r, size := utf8.DecodeLastRuneInString(s)
		if size == 0 || !isEmojiRune(r) {
			break
		}
		s = s[:len(s)-size]
	}

	return strings.TrimSpace(s)
}
