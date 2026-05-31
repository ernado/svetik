package ai

import "testing"

func TestTrimEmoji(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"no emoji", "hello world", "hello world"},
		{"leading", "😀 привет", "привет"},
		{"trailing", "привет 😀", "привет"},
		{"both", "🔥hello🔥", "hello"},
		{"multiple and spaces", "😀😀  hi there  🎉🎉", "hi there"},
		{"interior kept", "hi 😀 there", "hi 😀 there"},
		{"leading kept interior", "🔥 a 😀 b", "a 😀 b"},
		{"zwj sequence", "👨‍👩‍👧 family", "family"},
		{"emoji with vs16", "❤️ love", "love"},
		{"only emoji", "😀🎉🔥", ""},
		{"empty", "", ""},
		{"flags", "🇷🇺 Россия 🇷🇺", "Россия"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := trimEmoji(c.in); got != c.want {
				t.Errorf("trimEmoji(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
