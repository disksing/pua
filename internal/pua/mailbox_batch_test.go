package pua

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestMailboxAckCLIRecoversCommittedRequestAfterLostResponses(t *testing.T) {
	for _, retry := range []string{"same-command", "resume"} {
		t.Run(retry, func(t *testing.T) {
			t.Setenv("PUA_CHANNEL", "")
			t.Setenv(puaWorkspaceRootEnvironment, "")
			t.Setenv(puaWorkspaceInstanceEnvironment, "")
			t.Setenv(puaResourceIDEnvironment, "workspace")
			withTempCwd(t, func(root string) {
				run(t, "init")
				var mutex sync.Mutex
				var frozen map[string]any
				var operationID string
				posts, workQueries, commits := 0, 0, 0
				loseQuery := true
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mutex.Lock()
					defer mutex.Unlock()
					switch {
					case strings.HasSuffix(r.URL.Path, "/turn-work"):
						workQueries++
						if workQueries != 1 {
							t.Error("retry inferred a new work instead of restoring the frozen request")
						}
						_, _ = fmt.Fprint(w, `{"works":[{"id":"work-original","state":"running"}]}`)
					case strings.HasSuffix(r.URL.Path, "/messages/ack"):
						posts++
						var body map[string]any
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
							return
						}
						operationID, _ = body["operationId"].(string)
						pending, found, err := readMailboxPendingAck(root, operationID)
						if err != nil || !found || pending.WorkID != "work-original" {
							t.Errorf("request was not frozen before POST: %#v found=%v err=%v", pending, found, err)
						}
						if posts == 1 {
							frozen = body
							commits++
							_, _ = fmt.Fprint(w, `{`)
							return
						}
						if !reflect.DeepEqual(body, frozen) {
							t.Errorf("request changed after restart: got=%#v want=%#v", body, frozen)
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"operationId": operationID, "handled": 2})
					case strings.Contains(r.URL.Path, "/message-operations/"):
						if loseQuery {
							http.Error(w, "query response unavailable", http.StatusServiceUnavailable)
							return
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"operationId": operationID, "handled": 2})
					default:
						t.Errorf("unexpected request: %s", r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				args := []string{"message", "ack", "--ids=one,two", "--summary=both reports checked", "--server=" + server.URL}
				_, err := runErr(t, args...)
				if err == nil || !strings.Contains(err.Error(), operationID) || !strings.Contains(err.Error(), "--resume=") {
					t.Fatalf("unknown result did not identify frozen operation: operation=%s err=%v", operationID, err)
				}
				pending, found, err := readMailboxPendingAck(root, operationID)
				if err != nil || !found || pending.LastError == "" || pending.InstanceID == "" {
					t.Fatalf("pending request lost: %#v found=%v err=%v", pending, found, err)
				}
				info, err := os.Stat(mailboxPendingAckPath(root, operationID))
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("pending request permissions: info=%v err=%v", info, err)
				}
				output, err := runErr(t, "message", "operation", "show", "--id="+operationID, "--server="+server.URL)
				if err == nil || !strings.Contains(output, `"status": "unconfirmed"`) || !strings.Contains(output, operationID) {
					t.Fatalf("operation inspection lost local request: output=%s err=%v", output, err)
				}
				if _, err := runErr(t, "message", "ack", "--operation="+operationID, "--ids=other", "--summary=different", "--server="+server.URL); err == nil {
					t.Fatal("different request overwrote frozen operation")
				}
				mutex.Lock()
				loseQuery = false
				mutex.Unlock()
				if retry == "resume" {
					args = []string{"message", "ack", "--resume=" + operationID, "--server=" + server.URL}
				}
				output, err = runErr(t, args...)
				if err != nil || !strings.Contains(output, operationID) {
					t.Fatalf("frozen replay failed: output=%s err=%v", output, err)
				}
				if _, found, err := readMailboxPendingAck(root, operationID); err != nil || found {
					t.Fatalf("confirmed request remains pending: found=%v err=%v", found, err)
				}
				mutex.Lock()
				defer mutex.Unlock()
				if posts != 2 || commits != 1 || workQueries != 1 {
					t.Fatalf("recovery duplicated work: posts=%d commits=%d workQueries=%d", posts, commits, workQueries)
				}
			})
		})
	}
}

func TestMailboxReadCLISelectsWorkspaceMessagesAndHidesReceipts(t *testing.T) {
	t.Setenv("PUA_CHANNEL", "")
	t.Setenv(puaWorkspaceRootEnvironment, "")
	t.Setenv(puaWorkspaceInstanceEnvironment, "")
	t.Setenv(puaResourceIDEnvironment, "workspace")
	withTempCwd(t, func(root string) {
		run(t, "init")
		for _, test := range []struct {
			name  string
			flag  string
			query url.Values
		}{
			{name: "explicit IDs", flag: "--ids=one"},
			{name: "source", flag: "--from=project1", query: url.Values{"fromResource": {"project1"}, "limit": {"20"}}},
			{name: "cursor", flag: "--cursor=cursor-limit-one", query: url.Values{"cursor": {"cursor-limit-one"}}},
		} {
			t.Run(test.name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case strings.HasSuffix(r.URL.Path, "/resources/workspace/turn-work"):
						_, _ = fmt.Fprint(w, `{"works":[{"id":"work-original","state":"running"}]}`)
					case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/resources/workspace/messages"):
						query := r.URL.Query()
						query.Del("status")
						query.Del("role")
						if !reflect.DeepEqual(query, test.query) {
							t.Errorf("unexpected list selector: %s", r.URL.RawQuery)
						}
						_, _ = fmt.Fprint(w, `{"items":[{"messageId":"one","canAck":true},{"messageId":"ineligible","canAck":false}],"nextCursor":"page-three"}`)
					case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/resources/workspace/messages/read"):
						var body struct {
							IDs      []string `json:"ids"`
							WorkID   string   `json:"workId"`
							MaxBytes int      `json:"maxBytes"`
						}
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
						}
						if !reflect.DeepEqual(body.IDs, []string{"one"}) || body.WorkID != "work-original" || body.MaxBytes != 1024 {
							t.Errorf("unexpected Workspace read request: %#v", body)
						}
						_, _ = fmt.Fprint(w, `{"capability":"mailbox_batch_v1","items":[{"messageId":"one","text":"report","complete":true,"canAck":true,"readReceipt":"opaque-secret"}],"remainingIds":[],"queue":{"waitingTotal":2}}`)
					default:
						t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				output, err := runErr(t, "message", "read", test.flag, "--max-bytes=1024", "--server="+server.URL)
				if err != nil || strings.Contains(output, "opaque-secret") || !strings.Contains(output, `"text": "report"`) ||
					!strings.Contains(output, `"waitingTotal": 2`) || strings.Contains(output, `"nextCursor": "page-three"`) != (test.query != nil) {
					t.Fatalf("CLI read contract mismatch: output=%s err=%v", output, err)
				}
			})
		}
	})
}
