package pua

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDialogueCLIUsesFencedChatAndRejectsDirectTaskMutation(t *testing.T) {
	t.Setenv("PUA_CHANNEL", "")
	t.Setenv(puaWorkspaceRootEnvironment, "")
	t.Setenv(puaWorkspaceInstanceEnvironment, "")
	t.Setenv(puaResourceIDEnvironment, "")
	withTempCwd(t, func(root string) {
		run(t, "init")
		var body map[string]any
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasSuffix(r.URL.Path, "/resources/workspace/chat/publish") {
				t.Errorf("unexpected route %s", r.URL.Path)
				http.NotFound(w, r)
				return
			}
			if r.Header.Get("X-PUA-Chat") != "test-chat" || r.Header.Get("X-PUA-Chat-Token") != "test-token" || r.Header.Get("X-PUA-Chat-Generation") != "test-generation" {
				t.Error("missing dialogue context")
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			_, _ = io.WriteString(w, `{"id":"test-chat","messages":[]}`)
		}))
		defer server.Close()
		data, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "address": server.URL, "workspacePath": root})
		if err := os.WriteFile(filepath.Join(root, ".pua", "serve.lock"), data, 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PUA_CHANNEL", "dialogue")
		t.Setenv("PUA_CHAT_ID", "test-chat")
		t.Setenv("PUA_CHAT_TOKEN", "test-token")
		t.Setenv("PUA_CHAT_GENERATION", "test-generation")
		t.Setenv(puaResourceIDEnvironment, "workspace")
		run(t, "chat", "publish", "--id=reply-1", "Hello")
		if body["id"] != "reply-1" || body["text"] != "Hello" {
			t.Fatalf("unexpected publish: %#v", body)
		}
		for _, args := range [][]string{{"task", "state", "set", "completed"}, {"message", "send", "--to=project1", "change"}, {"project", "create", "unapproved execution"}} {
			if _, err := runErr(t, args...); err == nil || !strings.Contains(err.Error(), "dialogue Sessions") {
				t.Fatalf("unfenced mutation %v: %v", args, err)
			}
		}
	})
}
