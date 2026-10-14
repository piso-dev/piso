// Package telegram is the gateway's Telegram Bot API client.
//
// The gateway is the only holder of a bot token. A bot token is a gateway-local
// credential like the GitHub App key: it is never exported to a worker and never
// substituted into a worker request, so there is no placeholder for it to leak
// through.
//
// The bot token is part of the request URL (/bot<token>/<method>). Every failure
// path in this package is therefore mapped to an error that carries no detail,
// because net/http embeds the request URL in its errors and forwarding one would
// write the token into the gateway log.
package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// APIBase is the Bot API root. Tests point a Client at a local server instead.
const APIBase = "https://api.telegram.org"

// TokenPattern matches a BotFather token: "<bot id>:<secret>". It is checked
// before any request, so a malformed token never reaches the URL path.
var TokenPattern = regexp.MustCompile(`^[0-9]{5,}:[A-Za-z0-9_-]{30,}$`)

// ErrNoResponse means no usable Bot API response arrived: a transport failure,
// a timeout, or a body that is not a Bot API envelope. It deliberately carries
// no detail, for the reason in the package comment.
var ErrNoResponse = errors.New("telegram did not return a Bot API response")

// APIError is a rejection by the Bot API itself (ok:false). Description is
// Telegram's own text ("Unauthorized", "Not Found"); it never contains the
// token.
type APIError struct {
	Code        int
	Description string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram: %s (error %d)", e.Description, e.Code)
}

// Bot is the getMe result.
type Bot struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

// Client talks to the Bot API. It holds no credential: the token is passed per
// call, so one client serves validation and (later) polling alike.
type Client struct {
	HTTP *http.Client
	Base string
}

// NewClient returns a client using h, falling back to http.DefaultClient.
// h is nil in production: the gateway reaches Telegram directly, it is the
// proxy. Tests inject a client pointed at a local server.
func NewClient(h *http.Client) *Client {
	if h == nil {
		h = http.DefaultClient
	}
	return &Client{HTTP: h, Base: APIBase}
}

// envelope is the Bot API response wrapper: every method returns ok plus either
// result or a description.
type envelope[T any] struct {
	OK          bool   `json:"ok"`
	Result      T      `json:"result"`
	ErrorCode   int    `json:"error_code"`
	Description string `json:"description"`
}

// ClientMessage renders err for a human. ErrNoResponse is already detail-free,
// and an *APIError carries only Telegram's own text, so neither can echo the
// token that rides in the request URL.
func ClientMessage(err error) string {
	var api *APIError
	if errors.As(err, &api) {
		return api.Error()
	}
	return "Telegram is unreachable right now."
}

// GetMe validates token and returns the bot it belongs to. A token Telegram
// rejects is an *APIError; anything else is ErrNoResponse.
func (c *Client) GetMe(ctx context.Context, token string) (Bot, error) {
	return call[Bot](ctx, c, token, "getMe", nil)
}

// Update is one entry of the getUpdates stream. Only message updates are asked
// for, so no other kind is modelled.
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

// Message is the subset of a Telegram message this gateway reads.
//
// From is the account that sent it and Chat is the conversation it landed in.
// In a private chat they carry the same id, and private is the only chat type
// this gateway serves: binding a group would let any member act as the owner.
// From is never read for a group, and it is the id the allow-list is keyed by.
type Message struct {
	MessageID int64  `json:"message_id"`
	Date      int64  `json:"date"`
	Text      string `json:"text"`
	Chat      Chat   `json:"chat"`
	From      *User  `json:"from"`
}

// Chat is the conversation a message landed in.
type Chat struct {
	ID        int64  `json:"id"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}

// User is a Telegram account.
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

// GetUpdates long-polls for updates after offset. Telegram holds the request
// open for up to wait and then returns an empty list, so a caller that keeps
// calling never spins. Passing the previous update_id+1 acknowledges
// everything before it: an update that is not acknowledged is redelivered,
// which is what turns a dropped message into a busy loop.
func (c *Client) GetUpdates(ctx context.Context, token string, offset int64, wait time.Duration) ([]Update, error) {
	form := url.Values{}
	if offset > 0 {
		form.Set("offset", strconv.FormatInt(offset, 10))
	}
	form.Set("timeout", strconv.Itoa(int(wait.Seconds())))
	// Only message updates are consumed. Telegram returns a JSON array here.
	form.Set("allowed_updates", `["message"]`)
	return call[[]Update](ctx, c, token, "getUpdates", form)
}

// SendMessage posts text to a chat.
func (c *Client) SendMessage(ctx context.Context, token string, chatID int64, text string) error {
	_, err := call[Message](ctx, c, token, "sendMessage", url.Values{
		"chat_id": {strconv.FormatInt(chatID, 10)},
		"text":    {text},
	})
	return err
}

// DeleteWebhook removes a webhook from the bot. A bot with a webhook set
// answers getUpdates with 409 Conflict, so taking a bot over for polling means
// clearing one first.
func (c *Client) DeleteWebhook(ctx context.Context, token string) error {
	_, err := call[bool](ctx, c, token, "deleteWebhook", nil)
	return err
}

// call performs one Bot API method and decodes its result. It is a package
// function rather than a method because Go methods cannot take type
// parameters.
func call[T any](ctx context.Context, c *Client, token, method string, form url.Values) (T, error) {
	var zero T
	if !TokenPattern.MatchString(token) {
		return zero, &APIError{Code: 401, Description: "malformed bot token"}
	}
	base := c.Base
	if base == "" {
		base = APIBase
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/bot"+token+"/"+method, strings.NewReader(form.Encode()))
	if err != nil {
		return zero, ErrNoResponse
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return zero, ErrNoResponse
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return zero, ErrNoResponse
	}
	var env envelope[T]
	if err := json.Unmarshal(raw, &env); err != nil {
		return zero, ErrNoResponse
	}
	if !env.OK {
		desc := env.Description
		if desc == "" {
			desc = resp.Status
		}
		return zero, &APIError{Code: env.ErrorCode, Description: desc}
	}
	return env.Result, nil
}
