package reaction

import (
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Allowed is the set of free Telegram reactions bots may use.
var Allowed = []string{
	"❤",
	"👍",
	"👎",
	"🔥",
	"🥰",
	"😁",
	"🤔",
	"🤯",
	"😢",
	"🎉",
	"🤮",
	"💩",
	"🤡",
	"🥱",
	"🐳",
	"❤‍🔥",
	"💯",
	"🏆",
	"💔",
	"🍓",
	"💋",
	"🖕",
	"😴",
	"😭",
	"🤓",
	"😨",
	"🫡",
	"💅",
	"💊",
	"😎",
	"😡",
}

// allowedSet is a fast-lookup copy of Allowed.
var allowedSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(Allowed))
	for _, r := range Allowed {
		m[r] = struct{}{}
	}
	return m
}()

var reUnicode = regexp.MustCompile(`U\+([0-9a-fA-F]{4,6})`)

// DecodeUnicodeTokens converts tokens like "U+2764 U+FE0F" to the
// corresponding Unicode characters, trimming surrounding whitespace.
func DecodeUnicodeTokens(input string) string {
	result := reUnicode.ReplaceAllStringFunc(input, func(match string) string {
		sub := reUnicode.FindStringSubmatch(match)
		if len(sub) < 2 {
			return ""
		}

		cp, err := strconv.ParseInt(sub[1], 16, 32)
		if err != nil || !utf8.ValidRune(rune(cp)) {
			return ""
		}

		return string(rune(cp))
	})

	return strings.TrimSpace(result)
}

// StripVariationSelectors removes text/emoji presentation variation selectors
// (U+FE0E and U+FE0F) while keeping ZWJ sequences intact.
func StripVariationSelectors(input string) string {
	return strings.NewReplacer("\uFE0E", "", "\uFE0F", "").Replace(input)
}

// IsAllowed reports whether emoji is in the Allowed list.
func IsAllowed(emoji string) bool {
	_, ok := allowedSet[emoji]
	return ok
}

// Canonicalize maps any incoming string to a member of Allowed if possible,
// returning ("", false) when no match is found.
//
// Steps:
//  1. Decode any "U+XXXX" tokens.
//  2. Exact match against Allowed.
//  3. Strip variation selectors and retry against each allowed entry.
func Canonicalize(input string) (string, bool) {
	candidate := DecodeUnicodeTokens(input)

	if IsAllowed(candidate) {
		return candidate, true
	}

	base := StripVariationSelectors(candidate)

	for _, a := range Allowed {
		if StripVariationSelectors(a) == base {
			return a, true
		}
	}

	return "", false
}

// NormalizeBlacklist canonicalizes each entry in reactions, deduplicates, and
// returns only entries that map to a known allowed reaction.
func NormalizeBlacklist(reactions []string) []string {
	seen := make(map[string]struct{}, len(reactions))
	out := make([]string, 0, len(reactions))

	for _, r := range reactions {
		canon, ok := Canonicalize(r)
		if !ok {
			continue
		}

		if _, dup := seen[canon]; dup {
			continue
		}

		seen[canon] = struct{}{}
		out = append(out, canon)
	}

	return out
}

// ResolveEnabled returns all Allowed reactions that are not in blacklist.
// If blacklist is empty the full Allowed slice is returned.
func ResolveEnabled(blacklist []string) []string {
	bl := NormalizeBlacklist(blacklist)
	if len(bl) == 0 {
		result := make([]string, len(Allowed))
		copy(result, Allowed)
		return result
	}

	blSet := make(map[string]struct{}, len(bl))
	for _, r := range bl {
		blSet[r] = struct{}{}
	}

	out := make([]string, 0, len(Allowed))
	for _, r := range Allowed {
		if _, blocked := blSet[r]; !blocked {
			out = append(out, r)
		}
	}

	return out
}
