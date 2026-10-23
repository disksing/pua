package serve

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/disksing/pua/internal/app"
)

func (rt *telegramRuntime) process(ctx context.Context, st *telegramState, u telegramUpdate) error {
	msg := u.Message
	command := ""
	account := int64(0)
	if u.Callback != nil {
		cb := u.Callback
		msg = cb.Message
		account = cb.From.ID
		if msg == nil || msg.Chat.Type != "private" || msg.Chat.ID != account {
			return nil
		}
		_ = rt.call(ctx, st.Token, "answerCallbackQuery", map[string]any{"callback_query_id": cb.ID}, nil)
		command = cb.Data
	} else if msg != nil {
		account = msg.From.ID
		command = msg.Text
	}
	if msg == nil || msg.Chat.Type != "private" || account != msg.Chat.ID || account <= 0 {
		return nil
	}
	if u.Message != nil && msg.From.IsBot {
		return nil
	}
	say := func(text string, buttons [][]telegramButton) {
		if err := rt.send(ctx, *st, msg.Chat.ID, msg.ThreadID, text, buttons); err != nil {
			st.LastError = err.Error()
		}
	}
	args := strings.Fields(command)
	verb := ""
	if len(args) > 0 {
		verb = strings.Split(args[0], "@")[0]
	}
	if verb == "/start" && len(args) == 2 {
		key := telegramPairKey(args[1])
		pair, ok := st.Pairings[key]
		if !ok || time.Now().After(pair.Expires) {
			say("Pairing link is invalid or expired. Generate a new link in Workspace Settings.", nil)
			return nil
		}
		ws, err := app.OpenWorkspace(rt.workspace.Path)
		if err != nil {
			return err
		}
		if _, err = ws.User(pair.User); err != nil {
			delete(st.Pairings, key)
			return nil
		}
		if old := st.Accounts[account]; old != "" && old != pair.User {
			say("Unlink this Telegram account in Workspace Settings before pairing another user.", nil)
			return nil
		}
		for other, user := range st.Accounts {
			if user == pair.User && other != account {
				say("This Workspace user is already paired. Unlink the old account in Workspace Settings first.", nil)
				return nil
			}
		}
		st.Accounts[account] = pair.User
		delete(st.Pairings, key)
		if err = rt.save(*st); err != nil {
			return err
		}
		say("Paired with "+rt.workspace.Name+" as "+pair.User+". Open a resource to start chatting.", telegramHomeButtons())
		return nil
	}
	user := st.Accounts[account]
	ws, err := app.OpenWorkspace(rt.workspace.Path)
	if err != nil {
		return err
	}
	if user == "" {
		say("Pair your account using Telegram in PUA Workspace Settings.", nil)
		return nil
	}
	if _, err = ws.User(user); err != nil {
		return nil
	}
	switch verb {
	case "/start", "/menu", "/help":
		say("Choose a resource. Use /search words to find tasks, /open resource-id to open one, /status for this topic, /stop to pause pushes, and /resume to resume new replies.", telegramHomeButtons())
		return nil
	case "/projects", "/tasks", "/search", "/recent":
		text, buttons, e := rt.resourceMenu(ws, *st, user, args)
		if e != nil {
			return e
		}
		say(text, buttons)
		return nil
	case "/open", "/bind":
		if len(args) != 2 {
			say("Use /open workspace or /open project1.task1. Use /bind resource-id inside an existing topic to repair its binding.", nil)
			return nil
		}
		if verb == "/bind" && msg.ThreadID == 0 {
			say("Send /bind inside the resource's topic.", nil)
			return nil
		}
		thread := int64(0)
		if verb == "/bind" {
			thread = msg.ThreadID
		}
		return rt.openTopic(ctx, st, user, msg.Chat.ID, thread, args[1], say)
	case "/stop", "/resume", "/status":
		for i := range st.Routes {
			route := &st.Routes[i]
			if route.ChatID != account || route.ThreadID != msg.ThreadID {
				continue
			}
			if verb == "/status" {
				detail, e := ws.Resource(route.Resource)
				status := ""
				if e == nil {
					status = fmt.Sprintf("\nState: %v", detail.State)
				}
				say(route.Title+"\nResource: "+route.Resource+status+fmt.Sprintf("\nPushes paused: %t", route.Paused), nil)
				return nil
			}
			if verb == "/stop" {
				route.Paused = true
				route.Pending = nil
				if e := rt.save(*st); e != nil {
					return e
				}
				say("Pushes paused for this topic. Agent execution and the PUA chat continue. Use /resume for new replies.", nil)
				return nil
			}
			return rt.openTopic(ctx, st, user, msg.Chat.ID, route.ThreadID, route.Resource, say)
		}
		say("Open a resource first.", telegramHomeButtons())
		return nil
	}
	if strings.HasPrefix(command, "/") {
		say("Unknown command. Use /help for the menu.", nil)
		return nil
	}
	if msg.Text == "" {
		if u.Message != nil {
			say("This version accepts text messages. Send attachments in PUA.", nil)
		}
		return nil
	}
	for i := range st.Routes {
		route := &st.Routes[i]
		if route.ChatID != account || route.ThreadID != msg.ThreadID || route.User != user {
			continue
		}
		if route.Paused {
			say("This topic is paused. Use /resume to reopen it.", nil)
			return nil
		}
		exists, archived, _, e := resourceExistsAndArchived(rt.workspace.Path, route.Resource)
		if e != nil || !exists || archived {
			route.Paused = true
			say("This resource is unavailable or archived. Check PUA and use /resume when it is available.", nil)
			return nil
		}
		chatMu.Lock()
		chat, e := ensureChatLocked(rt.workspace.Path, route.Resource, user)
		if e == nil && (!chat.Open || chat.Epoch != route.Epoch) {
			chatMu.Unlock()
			say("This PUA chat was closed or reopened. Use /resume to reconnect this topic.", nil)
			return nil
		}
		if e == nil {
			e = chat.appendUserInput(fmt.Sprintf("telegram-%d-%d", st.BotID, u.ID), msg.Text)
		}
		if e == nil {
			e = saveChatLocked(rt.workspace.Path, chat)
		}
		chatMu.Unlock()
		if e != nil {
			return e
		}
		rt.server.agents.requestReconcile(reconcileMailboxes | reconcileAgentHub)
		return nil
	}
	say("This topic is not linked to a resource. Choose one from the menu, or use /bind resource-id here.", telegramHomeButtons())
	return nil
}
func telegramHomeButtons() [][]telegramButton {
	return [][]telegramButton{{{Text: "Workspace", Data: "/open workspace"}, {Text: "Recent chats", Data: "/recent"}}, {{Text: "Browse projects", Data: "/projects"}, {Text: "Search tasks", Data: "/search"}}}
}
func (rt *telegramRuntime) openTopic(ctx context.Context, st *telegramState, user string, chat, thread int64, resource string, say func(string, [][]telegramButton)) error {
	resource = normalizedResourceID(resource)
	exists, archived, _, err := resourceExistsAndArchived(rt.workspace.Path, resource)
	// A typo or a stale menu must not poison the durable input queue.
	if err != nil || !exists || archived || strings.HasPrefix(resource, dialoguePrefix) {
		say("Resource is unavailable or archived.", nil)
		return nil
	}
	title := rt.workspace.Name + " · Workspace"
	if resource != "workspace" {
		ws, e := app.OpenWorkspace(rt.workspace.Path)
		if e != nil {
			return e
		}
		detail, e := ws.Resource(resource)
		if e != nil {
			return e
		}
		title = resource + " · " + detail.Title
	}
	titleRunes := []rune(title)
	if len(titleRunes) > 100 {
		title = string(titleRunes[:100])
	}
	index := -1
	for i, r := range st.Routes {
		if r.ChatID == chat && thread != 0 && r.ThreadID == thread && r.Resource != resource {
			say("This topic belongs to another resource. Open a separate topic instead.", nil)
			return nil
		}
		if r.ChatID == chat && r.User == user && r.Resource == resource {
			index = i
		}
	}

	if index >= 0 && thread != 0 && st.Routes[index].ThreadID != thread {
		// /bind explicitly repairs a deleted or inaccessible topic. The old
		// topic loses its mapping; pending deliveries never move to a new topic.
		st.Routes[index].ThreadID = thread
		st.Routes[index].Paused = true
		st.Routes[index].Pending = nil
	}

	if index < 0 {
		st.Routes = append(st.Routes, telegramRoute{User: user, ChatID: chat, Resource: resource, Title: title, Paused: true})
		index = len(st.Routes) - 1
		// Reserve before createForumTopic. A crash after creation must not create a
		// second topic automatically on inbox replay.
		if err = rt.save(*st); err != nil {
			return err
		}
		if thread == 0 {
			var topic struct {
				ID int64 `json:"message_thread_id"`
			}
			if err = rt.call(ctx, st.Token, "createForumTopic", map[string]any{"chat_id": chat, "name": title}, &topic); err != nil {
				var apiErr *telegramAPIError
				if errors.As(err, &apiErr) {
					st.Routes = st.Routes[:len(st.Routes)-1]
					if e := rt.save(*st); e != nil {
						return e
					}
				}
				say("Could not confirm topic creation. If a topic appeared, send /bind "+resource+" inside it; otherwise try /open again after checking Settings.", nil)
				return nil
			}
			thread = topic.ID
		}
	}
	route := &st.Routes[index]
	if thread == 0 {
		thread = route.ThreadID
	}
	if thread == 0 {
		say("Topic creation outcome is unknown. Open or create a Telegram topic and send /bind "+resource+" inside it.", nil)
		return nil
	}

	chatMu.Lock()
	c, err := ensureChatLocked(rt.workspace.Path, resource, user)
	if err == nil {
		if !c.Open {
			c.Open = true
			c.Epoch++
			c.Token = newGenerationRecordID()
		}
		err = saveChatLocked(rt.workspace.Path, c)
	}
	chatMu.Unlock()
	if err != nil {
		return err
	}
	if route.ThreadID != 0 && !route.Paused && route.Epoch == c.Epoch {
		if e := rt.send(ctx, *st, chat, thread, "Continue chatting with "+route.Title+" here.", nil); e != nil {
			st.LastError = e.Error()
		}
		return nil
	}
	route.ThreadID = thread
	route.Title = title
	route.Epoch = c.Epoch
	route.Cursor = len(c.Messages)
	route.Paused = false
	route.Pending = nil
	if err = rt.save(*st); err != nil {
		return err
	}
	if err = rt.send(ctx, *st, chat, thread, "Connected to "+title+". New public replies from PUA will appear here. Send text to continue; /menu opens navigation.", nil); err != nil {
		st.LastError = err.Error()
	}
	return nil
}

type telegramResourceItem struct{ ID, Title, Updated string }

func (rt *telegramRuntime) resourceMenu(ws *app.Workspace, st telegramState, user string, args []string) (string, [][]telegramButton, error) {
	verb := strings.Split(args[0], "@")[0]
	items := []telegramResourceItem{}
	page := 0
	prefix := verb
	if verb == "/recent" {

		titles := map[string]string{}
		for _, route := range st.Routes {
			if route.User == user {
				titles[route.Resource] = route.Title
			}
		}
		chatMu.Lock()
		paths, e := telegramChatPaths(rt.workspace.Path)
		if e != nil {
			chatMu.Unlock()
			return "", nil, e
		}
		for _, id := range paths {
			c, e := loadChatLocked(rt.workspace.Path, id)
			if e != nil || c.User != user || !c.Open {
				continue
			}
			title := titles[c.ResourceID]
			if title == "" {
				title = c.ResourceID
			}
			updated := ""
			if len(c.Messages) > 0 {
				updated = c.Messages[len(c.Messages)-1].CreatedAt
			}
			items = append(items, telegramResourceItem{ID: c.ResourceID, Title: title, Updated: updated})
		}
		chatMu.Unlock()

	} else {
		projects, err := ws.Projects(false)
		if err != nil {
			return "", nil, err
		}
		query := ""
		if verb == "/search" {
			if len(args) < 2 {
				return "Send /search followed by a task name or resource ID.", nil, nil
			}
			query = strings.ToLower(strings.Join(args[1:], " "))
		}
		for _, p := range projects {
			if verb == "/projects" {
				items = append(items, telegramResourceItem{ID: p.Project.ID, Title: p.Project.Title})
				continue
			}
			if verb == "/tasks" && (len(args) < 2 || p.Project.ID != args[1]) {
				continue
			}
			if verb == "/search" && strings.Contains(strings.ToLower(p.Project.ID+" "+p.Project.Title), query) {
				items = append(items, telegramResourceItem{ID: p.Project.ID, Title: p.Project.Title})
			}
			tasks, e := ws.Tasks(app.TaskListOptions{ProjectID: p.Project.ID})
			if e != nil {
				return "", nil, e
			}
			for _, t := range tasks.Tasks {
				if verb != "/search" || strings.Contains(strings.ToLower(t.Task.ID+" "+t.Task.Title), query) {
					items = append(items, telegramResourceItem{ID: t.Task.ID, Title: t.Task.Title})
				}
			}
		}
	}
	if verb == "/projects" || verb == "/recent" {
		if len(args) > 1 {
			page, _ = strconv.Atoi(args[1])
		}
	} else if verb == "/tasks" {
		if len(args) > 1 {
			prefix += " " + args[1]
		}
		if len(args) > 2 {
			page, _ = strconv.Atoi(args[2])
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if verb == "/recent" && items[i].Updated != items[j].Updated {
			return items[i].Updated > items[j].Updated
		}
		return items[i].ID < items[j].ID
	})
	// Clamp untrusted page numbers before multiplication to avoid overflow.
	if page < 0 || page > len(items)/8 {
		page = 0
	}
	start := page * 8
	if start >= len(items) {
		start = 0
		page = 0
	}
	end := min(start+8, len(items))
	buttons := [][]telegramButton{}
	for _, item := range items[start:end] {
		label := item.ID + " · " + item.Title
		if len([]rune(label)) > 65 {
			label = string([]rune(label)[:65])
		}
		row := []telegramButton{{Text: label, Data: "/open " + item.ID}}
		if verb == "/projects" {
			row = append(row, telegramButton{Text: "Tasks", Data: "/tasks " + item.ID})
		}
		buttons = append(buttons, row)
	}
	if verb != "/search" {
		nav := []telegramButton{}
		if start > 0 {
			nav = append(nav, telegramButton{Text: "Previous", Data: fmt.Sprintf("%s %d", prefix, page-1)})
		}
		if end < len(items) {
			nav = append(nav, telegramButton{Text: "Next", Data: fmt.Sprintf("%s %d", prefix, page+1)})
		}
		if len(nav) > 0 {
			buttons = append(buttons, nav)
		}
	}
	text := fmt.Sprintf("Resources %d–%d of %d", min(start+1, len(items)), end, len(items))
	if verb == "/search" && end < len(items) {
		text += ". Refine your search to see other matches."
	}
	if len(items) == 0 {
		text = "No matching resources."
	}
	return text, buttons, nil
}

func telegramChatPaths(root string) ([]string, error) {
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(chatPath(root, "placeholder")), "*.json"))
	ids := make([]string, 0, len(paths))
	for _, p := range paths {
		ids = append(ids, strings.TrimSuffix(filepath.Base(p), ".json"))
	}
	return ids, err
}
