package serve

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Telegram credentials and delivery state live beside the server configuration,
// never in a Workspace or its repositories. Lock order: runtime.mu, then chatMu.
type telegramState struct {
	Version   int                        `json:"version"`
	Revision  uint64                     `json:"revision"`
	Enabled   bool                       `json:"enabled"`
	Token     string                     `json:"token"`
	BotID     int64                      `json:"botId"`
	BotName   string                     `json:"botName"`
	Offset    int64                      `json:"offset"`
	Inbox     []telegramUpdate           `json:"inbox,omitempty"`
	Pairings  map[string]telegramPairing `json:"pairings"`
	Accounts  map[int64]string           `json:"accounts"`
	Routes    []telegramRoute            `json:"routes"`
	LastError string                     `json:"lastError,omitempty"`
	LastPoll  string                     `json:"lastPoll,omitempty"`
}
type telegramPairing struct {
	User    string    `json:"user"`
	Expires time.Time `json:"expires"`
}
type telegramRoute struct {
	User     string            `json:"user"`
	ChatID   int64             `json:"chatId"`
	ThreadID int64             `json:"threadId"`
	Resource string            `json:"resource"`
	Title    string            `json:"title"`
	Epoch    uint64            `json:"epoch"`
	Cursor   int               `json:"cursor"`
	Paused   bool              `json:"paused"`
	Pending  *telegramDelivery `json:"pending,omitempty"`
}
type telegramDelivery struct {
	Text       string    `json:"text"`
	Chunks     []string  `json:"chunks"`
	Part       int       `json:"part"`
	NextCursor int       `json:"nextCursor"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	RetryAt    time.Time `json:"retryAt,omitempty"`
}
type telegramRuntime struct {
	mu        sync.Mutex
	server    *server
	workspace serveWorkspace
	// Injectable only from Go tests; no arbitrary API endpoint in public settings.
	apiURL    string
	lastFiles map[string]time.Time
	nextSend  map[int64]time.Time
}

func (s *server) telegramFor(w serveWorkspace) *telegramRuntime {
	s.telegramMu.Lock()
	defer s.telegramMu.Unlock()
	if s.telegramRuntimes == nil {
		s.telegramRuntimes = map[string]*telegramRuntime{}
	}
	if rt := s.telegramRuntimes[w.ID]; rt != nil {
		return rt
	}
	rt := &telegramRuntime{server: s, workspace: w, apiURL: "https://api.telegram.org", lastFiles: map[string]time.Time{}, nextSend: map[int64]time.Time{}}
	s.telegramRuntimes[w.ID] = rt
	return rt
}
func (rt *telegramRuntime) path() string {
	return filepath.Join(filepath.Dir(rt.server.config), "telegram", chatID(rt.workspace.InstanceID, rt.workspace.Path)+".json")
}
func (rt *telegramRuntime) load() (telegramState, error) {
	st := telegramState{Version: 1, Pairings: map[string]telegramPairing{}, Accounts: map[int64]string{}, Routes: []telegramRoute{}}
	data, err := os.ReadFile(rt.path())
	if os.IsNotExist(err) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err = json.Unmarshal(data, &st); err != nil {
		return st, err
	}
	if st.Version != 1 {
		return st, errors.New("unsupported Telegram state version")
	}
	if st.Pairings == nil {
		st.Pairings = map[string]telegramPairing{}
	}
	if st.Accounts == nil {
		st.Accounts = map[int64]string{}
	}
	return st, nil
}
func (rt *telegramRuntime) save(st telegramState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	dir := filepath.Dir(rt.path())
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err = os.Chmod(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".telegram-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), rt.path()); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func telegramPairKey(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}
func telegramPairCode() (string, error) {
	b := make([]byte, 24)
	_, err := rand.Read(b)
	return hex.EncodeToString(b), err
}

func (r telegramRoute) key() string {
	return chatID(r.User, fmt.Sprintf("%d/%d/%s", r.ChatID, r.ThreadID, r.Resource))
}
func (r telegramRoute) deliveryKey() string {
	if r.Pending == nil {
		return ""
	}
	return chatID(r.key(), fmt.Sprintf("%d/%d/%s", r.Epoch, r.Pending.NextCursor, r.Pending.Text))
}
