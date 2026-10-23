package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

type telegramUser struct {
	ID       int64  `json:"id"`
	IsBot    bool   `json:"is_bot"`
	Username string `json:"username"`
	Topics   bool   `json:"has_topics_enabled"`
}
type telegramChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}
type telegramMessage struct {
	ID       int64        `json:"message_id"`
	ThreadID int64        `json:"message_thread_id"`
	From     telegramUser `json:"from"`
	Chat     telegramChat `json:"chat"`
	Text     string       `json:"text"`
}
type telegramCallback struct {
	ID      string           `json:"id"`
	From    telegramUser     `json:"from"`
	Data    string           `json:"data"`
	Message *telegramMessage `json:"message"`
}
type telegramUpdate struct {
	ID       int64             `json:"update_id"`
	Message  *telegramMessage  `json:"message,omitempty"`
	Callback *telegramCallback `json:"callback_query,omitempty"`
}
type telegramButton struct {
	Text string `json:"text"`
	Data string `json:"callback_data"`
}
type telegramAPIError struct {
	Code       int
	RetryAfter int
}

func (e *telegramAPIError) Error() string { return fmt.Sprintf("Telegram API error %d", e.Code) }

// Do not return request URLs or Telegram descriptions: both may contain secrets
// or reflected message contents. A transport error means an unknown send outcome.
func (rt *telegramRuntime) call(ctx context.Context, token, method string, body any, result any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return errors.New("cannot encode Telegram request")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rt.apiURL+"/bot"+token+"/"+method, bytes.NewReader(data))
	if err != nil {
		return errors.New("invalid Telegram request")
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("Telegram connection failed; delivery outcome may be unknown")
	}
	defer resp.Body.Close()
	var envelope struct {
		OK         bool            `json:"ok"`
		Result     json.RawMessage `json:"result"`
		Code       int             `json:"error_code"`
		Parameters struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&envelope); err != nil {
		return errors.New("invalid Telegram response; delivery outcome may be unknown")
	}
	if !envelope.OK {
		return &telegramAPIError{Code: envelope.Code, RetryAfter: envelope.Parameters.RetryAfter}
	}
	if result != nil {
		if err = json.Unmarshal(envelope.Result, result); err != nil {
			return errors.New("invalid Telegram result; delivery outcome may be unknown")
		}
	}
	return nil
}
func (rt *telegramRuntime) send(ctx context.Context, st telegramState, chat, thread int64, text string, buttons [][]telegramButton) error {
	body := map[string]any{"chat_id": chat, "text": text, "link_preview_options": map[string]bool{"is_disabled": true}}
	if thread != 0 {
		body["message_thread_id"] = thread
	}
	if len(buttons) > 0 {
		body["reply_markup"] = map[string]any{"inline_keyboard": buttons}
	}
	return rt.call(ctx, st.Token, "sendMessage", body, nil)
}

// 3500 UTF-16 units leaves room under Telegram's 4096 character limit even for
// astral characters. Plain text avoids broken Markdown entities across chunks.
func telegramChunks(text string) []string {
	var parts []string
	start, units := 0, 0
	for i, r := range text {
		n := 1
		if r > 0xffff {
			n = 2
		}
		if units+n > 3500 {
			parts = append(parts, text[start:i])
			start = i
			units = 0
		}
		units += n
	}
	if start < len(text) {
		parts = append(parts, text[start:])
	}
	return parts
}
