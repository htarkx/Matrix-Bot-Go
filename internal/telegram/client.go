// Package telegram is the part of the Telegram Bot API the bots use:
// sendMessage, getUpdates and setMyCommands.
package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultAPI is the public Bot API endpoint.
const DefaultAPI = "https://api.telegram.org"

// Client calls the Bot API for one bot token.
type Client struct {
	base string // <api>/bot<token>/
	http *http.Client
}

// New returns a client. api is DefaultAPI outside tests.
func New(api, token string) *Client {
	return &Client{base: strings.TrimRight(api, "/") + "/bot" + token + "/", http: &http.Client{}}
}

// Error is an API answer with ok=false.
type Error struct {
	Code        int    `json:"error_code"`
	Description string `json:"description"`
}

func (e *Error) Error() string { return fmt.Sprintf("telegram %d: %s", e.Code, e.Description) }

// call posts form parameters and decodes "result" into out.
func (c *Client) call(ctx context.Context, method string, params url.Values, timeout time.Duration, out any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+method, strings.NewReader(params.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		// the URL holds the token: never let it reach a log line
		var ue *url.Error
		if errors.As(err, &ue) {
			return fmt.Errorf("telegram %s: %w", method, ue.Err)
		}
		return err
	}
	defer resp.Body.Close()
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&env); err != nil {
		return fmt.Errorf("telegram %s: HTTP %d: %w", method, resp.StatusCode, err)
	}
	if !env.OK {
		return &env.Error
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(env.Result, out)
}

// SendHTML sends a parse_mode=HTML message without link previews.
func (c *Client) SendHTML(ctx context.Context, chatID int64, html string) error {
	return c.call(ctx, "sendMessage", url.Values{
		"chat_id":                  {strconv.FormatInt(chatID, 10)},
		"text":                     {html},
		"parse_mode":               {"HTML"},
		"disable_web_page_preview": {"true"},
	}, 20*time.Second, nil)
}

// Update is a getUpdates entry; only messages are requested.
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

// Message is the part of a message the bots read.
type Message struct {
	Date int64  `json:"date"`
	Text string `json:"text"`
	Chat struct {
		ID int64 `json:"id"`
	} `json:"chat"`
}

// GetUpdates long-polls for messages after offset (0 = from the oldest kept).
func (c *Client) GetUpdates(ctx context.Context, offset int64, wait time.Duration) ([]Update, error) {
	p := url.Values{
		"timeout":         {strconv.Itoa(int(wait.Seconds()))},
		"allowed_updates": {`["message"]`},
	}
	if offset != 0 {
		p.Set("offset", strconv.FormatInt(offset, 10))
	}
	var out []Update
	err := c.call(ctx, "getUpdates", p, wait+20*time.Second, &out)
	return out, err
}

// Command is a /command shown in Telegram's menu.
type Command struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

// Scope selects who sees a command menu; nil is the default scope (every
// user who opens the bot).
type Scope map[string]any

// ChatScope shows a menu in one chat only.
func ChatScope(chatID int64) Scope { return Scope{"type": "chat", "chat_id": chatID} }

func scopeParams(scope Scope) (url.Values, error) {
	p := url.Values{}
	if scope != nil {
		b, err := json.Marshal(scope)
		if err != nil {
			return nil, err
		}
		p.Set("scope", string(b))
	}
	return p, nil
}

// SetMyCommands replaces the command menu for a scope.
func (c *Client) SetMyCommands(ctx context.Context, cmds []Command, scope Scope) error {
	p, err := scopeParams(scope)
	if err != nil {
		return err
	}
	b, err := json.Marshal(cmds)
	if err != nil {
		return err
	}
	p.Set("commands", string(b))
	return c.call(ctx, "setMyCommands", p, 20*time.Second, nil)
}

// DeleteMyCommands removes the command menu of a scope.
func (c *Client) DeleteMyCommands(ctx context.Context, scope Scope) error {
	p, err := scopeParams(scope)
	if err != nil {
		return err
	}
	return c.call(ctx, "deleteMyCommands", p, 20*time.Second, nil)
}
