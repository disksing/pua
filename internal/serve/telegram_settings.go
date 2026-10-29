package serve

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/disksing/pua/internal/app"
)

var telegramTokenPattern = regexp.MustCompile(`^[0-9]+:[A-Za-z0-9_-]{20,}$`)

func (s *server) handleTelegram(w http.ResponseWriter, r *http.Request, id string, parts []string) {
	if origin := r.Header.Get("Origin"); origin != "" {
		parsed, e := url.Parse(origin)
		if e != nil || parsed.Host != r.Host || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			writeError(w, errors.New("cross-origin Telegram settings request rejected"), 403)
			return
		}
	}
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		writeError(w, errors.New("cross-site Telegram settings request rejected"), 403)
		return
	}
	workspace, err := s.workspace(id)
	if err != nil {
		writeError(w, err, 404)
		return
	}
	if err = s.requireWorkspaceOwnership(workspace.Path); err != nil {
		writeError(w, err, 409)
		return
	}
	if r.Header.Get("X-PUA-Chat-Token") != "" {
		writeError(w, errors.New("Telegram settings require the local Settings interface"), 403)
		return
	}
	s.telegramSettingsMu.Lock()
	defer s.telegramSettingsMu.Unlock()
	rt := s.telegramFor(workspace)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	st, err := rt.load()
	if err != nil {
		writeError(w, errors.New("cannot read Telegram settings"), 500)
		return
	}
	op := ""
	if len(parts) > 0 {
		op = parts[0]
	}
	if len(parts) > 1 {
		http.NotFound(w, r)
		return
	}
	ws, err := app.OpenWorkspace(workspace.Path)
	if err != nil {
		writeError(w, err, 500)
		return
	}
	if r.Method == http.MethodGet && op == "" {
		rt.writeSettings(w, st)
		return
	}
	var body struct {
		Token    string `json:"token"`
		Enabled  bool   `json:"enabled"`
		User     string `json:"user"`
		Route    string `json:"route"`
		Delivery string `json:"delivery"`
		Action   string `json:"action"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&body); err != nil {
		writeError(w, errors.New("invalid Telegram settings request"), 400)
		return
	}
	switch {
	case r.Method == http.MethodPut && op == "":
		token := strings.TrimSpace(body.Token)
		if token == "" {
			token = st.Token
		}
		if body.Enabled || body.Token != "" {
			if !telegramTokenPattern.MatchString(token) {
				writeError(w, errors.New("enter a valid BotFather token"), 400)
				return
			}
			var bot telegramUser
			if err = rt.call(r.Context(), token, "getMe", map[string]any{}, &bot); err != nil {
				writeError(w, err, 400)
				return
			}
			if !bot.IsBot || !bot.Topics {
				writeError(w, errors.New("enable Topics / Threaded Mode for this bot in BotFather first"), 400)
				return
			}
			var hook struct {
				URL string `json:"url"`
			}
			if err = rt.call(r.Context(), token, "getWebhookInfo", map[string]any{}, &hook); err != nil {
				writeError(w, err, 400)
				return
			}
			if hook.URL != "" {
				writeError(w, errors.New("this bot has an active webhook; use a dedicated bot or remove its webhook first"), 409)
				return
			}
			// One bot must have exactly one update consumer across managed Workspaces.
			cfg, e := s.loadConfig()
			if e != nil {
				writeError(w, e, 500)
				return
			}
			for _, other := range cfg.Workspaces {
				if other.ID == workspace.ID {
					continue
				}
				otherRT := s.telegramFor(other)
				otherState, e := otherRT.load()
				if e != nil {
					writeError(w, errors.New("cannot check other Workspace bot settings"), 500)
					return
				}
				if otherState.BotID == bot.ID {
					writeError(w, errors.New("this bot is already configured for another Workspace"), 409)
					return
				}
			}
			if st.BotID != 0 && st.BotID != bot.ID {
				writeError(w, errors.New("disconnect the current bot before replacing it"), 409)
				return
			}
			st.Token, st.BotID, st.BotName = token, bot.ID, bot.Username
		}
		st.Enabled = body.Enabled
		st.Revision++
		st.LastError = ""
	case r.Method == http.MethodPost && op == "pair":
		if !st.Enabled {
			writeError(w, errors.New("enable the Telegram connection first"), 409)
			return
		}
		if _, err = ws.User(body.User); err != nil {
			writeError(w, errors.New("select an existing Workspace user"), 400)
			return
		}
		code, e := telegramPairCode()
		if e != nil {
			writeError(w, e, 500)
			return
		}
		for key, p := range st.Pairings {
			if p.User == body.User || time.Now().After(p.Expires) {
				delete(st.Pairings, key)
			}
		}
		expires := time.Now().Add(10 * time.Minute)
		st.Pairings[telegramPairKey(code)] = telegramPairing{User: body.User, Expires: expires}
		if err = rt.save(st); err != nil {
			writeError(w, errors.New("cannot save Telegram pairing"), 500)
			return
		}
		writeJSON(w, map[string]any{"url": "https://t.me/" + st.BotName + "?start=" + code, "expiresAt": expires})
		return
	case r.Method == http.MethodPost && op == "unbind":
		revoked := map[int64]bool{}
		for account, user := range st.Accounts {
			if user == body.User {
				revoked[account] = true
				delete(st.Accounts, account)
			}
		}
		for key, p := range st.Pairings {
			if p.User == body.User {
				delete(st.Pairings, key)
			}
		}
		routes := st.Routes[:0]
		for _, route := range st.Routes {
			if route.User != body.User {
				routes = append(routes, route)
			}
		}
		st.Routes = routes
		inbox := st.Inbox[:0]
		for _, update := range st.Inbox {
			id := int64(0)
			if update.Message != nil {
				id = update.Message.From.ID
			}
			if update.Callback != nil {
				id = update.Callback.From.ID
			}
			if !revoked[id] {
				inbox = append(inbox, update)
			}
		}
		st.Inbox = inbox
		st.Revision++
	case r.Method == http.MethodPost && op == "disconnect":
		st = telegramState{Version: telegramStateVersion, Revision: st.Revision + 1, Pairings: map[string]telegramPairing{}, Accounts: map[int64]string{}, Routes: []telegramRoute{}}
	case r.Method == http.MethodPost && op == "delivery":

		var route *telegramRoute
		for i := range st.Routes {
			if st.Routes[i].key() == body.Route {
				route = &st.Routes[i]
				break
			}
		}
		if route == nil {
			writeError(w, errors.New("delivery changed; refresh the status first"), 409)
			return
		}

		if route.Pending != nil && route.deliveryKey() == body.Delivery {
			switch body.Action {
			case "retry":
				route.Pending.Status = "queued"
				route.Pending.RetryAt = time.Time{}
				route.Pending.Error = ""
			case "skip":
				route.Cursor = route.Pending.NextCursor
				route.Pending = nil
			default:
				writeError(w, errors.New("choose retry or skip"), 400)
				return
			}
		} else {
			index := -1
			for i, held := range route.Held {
				if held.ID == body.Delivery {
					index = i
					break
				}
			}
			if index < 0 {
				writeError(w, errors.New("delivery changed; refresh the status first"), 409)
				return
			}
			if body.Action != "acknowledge" {
				writeError(w, errors.New("earlier deliveries cannot be retried across chat periods"), 409)
				return
			}
			route.Held = append(route.Held[:index], route.Held[index+1:]...)
		}
	default:
		w.WriteHeader(405)
		return
	}
	if err = rt.save(st); err != nil {
		writeError(w, errors.New("cannot save Telegram settings"), 500)
		return
	}
	rt.writeSettings(w, st)
}
func (rt *telegramRuntime) writeSettings(w http.ResponseWriter, st telegramState) {
	type account struct {
		User       string `json:"user"`
		TelegramID int64  `json:"telegramId"`
	}
	accounts := []account{}
	for id, user := range st.Accounts {
		accounts = append(accounts, account{user, id})
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].User < accounts[j].User })
	type problem struct {
		Route         string `json:"route"`
		Delivery      string `json:"delivery"`
		Resource      string `json:"resource"`
		User          string `json:"user"`
		Status        string `json:"status"`
		Error         string `json:"error"`
		Held          bool   `json:"held"`
		Count         int    `json:"count,omitempty"`
		PendingStatus string `json:"pendingStatus,omitempty"`
		Since         string `json:"since,omitempty"`
	}
	type topicProblem struct {
		Resource string `json:"resource"`
		User     string `json:"user"`
		Reason   string `json:"reason"`
		Hint     string `json:"hint"`
		Since    string `json:"since,omitempty"`
	}
	problems := []problem{}
	topics := []topicProblem{}
	for _, r := range st.Routes {
		if r.Paused {
			topics = append(topics, topicProblem{r.Resource, r.User, r.PauseReason, telegramPauseHint(r.PauseReason), r.PausedAt})
		}
		if r.Pending != nil && r.Pending.Status != "queued" {
			problems = append(problems, problem{Route: r.key(), Delivery: r.deliveryKey(), Resource: r.Resource, User: r.User, Status: r.Pending.Status, Error: r.Pending.Error})
		}
		for _, held := range r.Held {
			pendingStatus := ""
			if held.Pending != nil {
				pendingStatus = held.Pending.Status
			}
			problems = append(problems, problem{
				Route: r.key(), Delivery: held.ID, Resource: r.Resource, User: r.User,
				Status: held.Reason, Held: true, Count: held.Count, PendingStatus: pendingStatus, Since: held.HeldAt,
			})
		}
	}
	writeJSON(w, map[string]any{"enabled": st.Enabled, "configured": st.Token != "", "botName": st.BotName, "accounts": accounts, "topicCount": len(st.Routes), "lastError": st.LastError, "lastPoll": st.LastPoll, "deliveryProblems": problems, "topicProblems": topics})
}
