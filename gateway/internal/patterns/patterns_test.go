package patterns

import "testing"

func TestTelegramBotTokenDetected(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// A real Telegram bot token is "<bot id>:<secret>"; the secret is only
	// base64url-ish, so without a dedicated pattern it matches nothing else.
	const real = "123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"
	if !hasPattern(c.Match([]byte("GET https://api.telegram.org/bot"+real+"/getMe")), "telegram-bot") {
		t.Fatalf("telegram token not detected")
	}
}

func TestTelegramPlaceholderIsNotARealToken(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if hasPattern(c.Match([]byte("GET https://api.telegram.org/botpiso_telegram_5ef1739b3cf1/getMe")), "telegram-bot") {
		t.Fatalf("placeholder matched the telegram pattern")
	}
}

func TestTelegramPatternDoesNotMatchTimes(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, text := range []string{
		"2024-01-01T12:34:56Z",
		"started at 12:34:56 and finished",
		"ratio 1:2:3",
	} {
		if hasPattern(c.Match([]byte(text)), "telegram-bot") {
			t.Fatalf("false positive on %q", text)
		}
	}
}

func hasPattern(matches []Match, id string) bool {
	for _, m := range matches {
		if m.PatternID == id {
			return true
		}
	}
	return false
}
