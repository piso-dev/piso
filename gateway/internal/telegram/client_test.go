package telegram

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const goodToken = "123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGetMeParsesBotAndUsesThePathForm(t *testing.T) {
	var path, body string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		body = string(raw)
		io.WriteString(w, `{"ok":true,"result":{"id":42,"is_bot":true,"first_name":"Piso","username":"piso_bot"}}`)
	}))
	defer ts.Close()
	c := NewClient(ts.Client())
	c.Base = ts.URL

	bot, err := c.GetMe(context.Background(), goodToken)
	if err != nil {
		t.Fatal(err)
	}
	if bot.ID != 42 || !bot.IsBot || bot.Username != "piso_bot" {
		t.Fatalf("bot %+v", bot)
	}
	// The token belongs in the path; getMe has no form fields.
	if path != "/bot"+goodToken+"/getMe" {
		t.Fatalf("path %q", path)
	}
	if body != "" {
		t.Fatalf("getMe should send no form fields, got %q", body)
	}
}

func TestMalformedTokenNeverReachesTheNetwork(t *testing.T) {
	c := NewClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("a malformed token must not produce a request")
		return nil, nil
	})})
	_, err := c.GetMe(context.Background(), "not-a-bot-token")
	var api *APIError
	if !errors.As(err, &api) || api.Code != 401 {
		t.Fatalf("want a 401 APIError, got %v", err)
	}
}

func TestTelegramRejectionKeepsItsDescription(t *testing.T) {
	c := NewClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Status:     "401 Unauthorized",
			Body:       io.NopCloser(strings.NewReader(`{"ok":false,"error_code":401,"description":"Unauthorized"}`)),
			Header:     http.Header{},
		}, nil
	})})
	_, err := c.GetMe(context.Background(), goodToken)
	var api *APIError
	if !errors.As(err, &api) || api.Code != 401 || api.Description != "Unauthorized" {
		t.Fatalf("want Telegram's own description, got %v", err)
	}
	if strings.Contains(err.Error(), goodToken) {
		t.Fatalf("token leaked: %v", err)
	}
}

// The token is in the request URL, and net/http embeds the URL in its errors.
// This is the test that keeps it out of the gateway log.
func TestTransportFailureNeverLeaksTheToken(t *testing.T) {
	c := NewClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New(`Post "https://api.telegram.org/bot` + goodToken + `/getMe": dial tcp: connection refused`)
	})})
	_, err := c.GetMe(context.Background(), goodToken)
	if !errors.Is(err, ErrNoResponse) {
		t.Fatalf("want ErrNoResponse, got %v", err)
	}
	if strings.Contains(err.Error(), goodToken) {
		t.Fatalf("token leaked: %v", err)
	}
}

func TestNonEnvelopeBodyIsErrNoResponse(t *testing.T) {
	c := NewClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusBadGateway,
			Status:     "502 Bad Gateway",
			Body:       io.NopCloser(strings.NewReader("<html>proxy error</html>")),
			Header:     http.Header{},
		}, nil
	})})
	_, err := c.GetMe(context.Background(), goodToken)
	if !errors.Is(err, ErrNoResponse) {
		t.Fatalf("want ErrNoResponse, got %v", err)
	}
}
