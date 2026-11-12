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

func TestTurnCLIControlsAndReadsCurrentWork(t *testing.T) {
	t.Setenv("PUA_CHANNEL", "")
	t.Setenv(puaWorkspaceRootEnvironment, "")
	t.Setenv(puaWorkspaceInstanceEnvironment, "")
	t.Setenv(puaResourceIDEnvironment, "")
	withTempCwd(t, func(root string) {
		run(t, "init")
		var requests int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests++
			if !strings.HasSuffix(r.URL.Path, "/resources/workspace/turn-work") {
				t.Errorf("unexpected route %s", r.URL.Path)
			}
			if r.Method == http.MethodPost {
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["action"] != "yield" || body["workId"] != "work-a" ||
					body["messageId"] != "input-b" || body["checkpoint"] != "done\nnext" {
					t.Errorf("unexpected command: %+v", body)
				}
			} else if requests == 3 && (r.URL.Query().Get("work") != "work-a" || r.URL.Query().Get("chat") != "true") {
				t.Error("missing work Chat scope")
			}
			_, _ = io.WriteString(w, `{"status":"accepted"}`)
		}))
		defer server.Close()
		data, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "address": server.URL, "workspacePath": root})
		if err := os.WriteFile(filepath.Join(root, ".pua", "serve.lock"), data, 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(puaResourceIDEnvironment, "workspace")
		run(t, "turn", "yield", "--work=work-a", "--message=input-b", "--checkpoint=done\nnext")
		run(t, "turn", "status")
		run(t, "chat", "show", "--work=work-a")
		for _, args := range [][]string{
			{"turn", "yield"}, {"turn", "finish", "--work=a", "--work=b"},
			{"turn", "finish", "--work=a", "--unknown=b"},
		} {
			if _, err := runErr(t, args...); err == nil {
				t.Fatalf("accepted invalid command: %v", args)
			}
		}
		if requests != 3 {
			t.Fatalf("unexpected request count %d", requests)
		}
	})
}
