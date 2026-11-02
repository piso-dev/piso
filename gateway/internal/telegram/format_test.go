package telegram

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTMLTransportIsOptInForSendsAndEdits(t *testing.T) {
	modes := []string{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		modes = append(modes, r.Form.Get("parse_mode"))
		if r.Form.Get("parse_mode") == "HTML" && r.Form.Get("link_preview_options") != `{"is_disabled":true}` {
			t.Error("HTML link previews must be disabled")
		}
		io.WriteString(w, `{"ok":true,"result":{"message_id":42}}`)
	}))
	defer ts.Close()
	c := NewClient(ts.Client())
	c.Base = ts.URL
	ctx := context.Background()
	if err := c.SendMessage(ctx, goodToken, 9, "a < b & c"); err != nil {
		t.Fatal(err)
	}
	if err := c.SendMessage(ctx, goodToken, 9, "<b>Answer</b><pre>a &lt; b</pre>", "HTML"); err != nil {
		t.Fatal(err)
	}
	if err := c.EditMessageText(ctx, goodToken, 9, 42, "<code>go test</code>", "HTML"); err != nil {
		t.Fatal(err)
	}
	if len(modes) != 3 || modes[0] != "" || modes[1] != "HTML" || modes[2] != "HTML" {
		t.Fatalf("modes: %v", modes)
	}
}
