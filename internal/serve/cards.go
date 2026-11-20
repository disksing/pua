package serve

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

type cardItem struct {
	Label    string  `json:"label"`
	Resource string  `json:"resource,omitempty"`
	URL      string  `json:"url,omitempty"`
	Value    float64 `json:"value,omitempty"`
}

type cardPoint struct {
	Time  time.Time `json:"time"`
	Value float64   `json:"value"`
}

type cardContent struct {
	Value  *float64    `json:"value,omitempty"`
	Max    float64     `json:"max,omitempty"`
	Items  []cardItem  `json:"items,omitempty"`
	Points []cardPoint `json:"points,omitempty"`
	HTML   string      `json:"html,omitempty"`
}

type workspaceCard struct {
	ID        string       `json:"id"`
	Kind      string       `json:"kind"`
	Source    string       `json:"source"`
	Title     string       `json:"title"`
	Version   int          `json:"version"`
	Collapsed bool         `json:"collapsed"`
	Height    int          `json:"height,omitempty"`
	Content   *cardContent `json:"content,omitempty"`
	Updated   string       `json:"updated,omitempty"`
}

type cardChange struct {
	Action    string       `json:"action"`
	ID        string       `json:"id,omitempty"`
	Key       string       `json:"key,omitempty"`
	Version   int          `json:"version,omitempty"`
	Kind      string       `json:"kind,omitempty"`
	Source    string       `json:"source,omitempty"`
	Title     string       `json:"title,omitempty"`
	Content   *cardContent `json:"content,omitempty"`
	Collapsed *bool        `json:"collapsed,omitempty"`
	Height    *int         `json:"height,omitempty"`
	Before    *string      `json:"before,omitempty"`
}

var cardSources = map[string][]string{
	"stat":  {"running", "running-tasks", "tasks"},
	"gauge": {"task-ratio"},
	"pie":   {"task-states"},
	"trend": {"running", "running-tasks", "tasks"},
	"list":  {"running", "running-tasks", "tasks"},
	"html":  {},
}

var cardColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func staticCardHTML(fragment string) (string, error) {
	decoder := xml.NewDecoder(strings.NewReader("<section>" + fragment + "</section>"))
	decoder.AutoClose, decoder.Entity = xml.HTMLAutoClose, xml.HTMLEntity
	var safe strings.Builder
	allowed := strings.Fields("section div span p h1 h2 h3 h4 strong em small b i br hr ul ol li table thead tbody tr th td code pre blockquote dl dt dd")
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return safe.String(), nil
		}
		if err != nil {
			return "", fmt.Errorf("invalid static HTML: %w", err)
		}
		switch element := token.(type) {
		case xml.StartElement:
			if element.Name.Space != "" || !slices.Contains(allowed, element.Name.Local) {
				return "", errors.New("use static formatting elements; use List for links")
			}
			safe.WriteString("<" + element.Name.Local)
			for _, attr := range element.Attr {
				if attr.Name.Space != "" || attr.Name.Local != "style" {
					return "", errors.New("only static color styles are supported")
				}
				var declarations []string
				for declaration := range strings.SplitSeq(attr.Value, ";") {
					if strings.TrimSpace(declaration) == "" {
						continue
					}
					property, value, _ := strings.Cut(declaration, ":")
					property, value = strings.TrimSpace(property), strings.TrimSpace(value)
					if (property != "color" && property != "background-color") || !cardColor.MatchString(value) {
						return "", errors.New("use color or background-color with a six-digit hex value")
					}
					declarations = append(declarations, property+":"+value)
				}
				safe.WriteString(` style="` + strings.Join(declarations, ";") + `"`)
			}
			safe.WriteByte('>')
		case xml.EndElement:
			safe.WriteString("</" + element.Name.Local + ">")
		case xml.CharData:
			safe.WriteString(html.EscapeString(string(element)))
		case xml.Directive, xml.ProcInst:
			return "", errors.New("HTML directives are unsupported")
		}
	}
}

func (content *cardContent) validate(kind string) error {
	if content == nil {
		return errors.New("agent content required")
	}
	if len(content.Items) > 200 || len(content.Points) > 500 || len(content.HTML) > 32768 {
		return errors.New("card content is too large")
	}
	for _, item := range content.Items {
		if strings.TrimSpace(item.Label) == "" || len(item.Label) > 500 {
			return errors.New("each item needs a short label")
		}
		if item.URL != "" {
			link, err := url.Parse(item.URL)
			if err != nil || link.Host == "" || (link.Scheme != "https" && link.Scheme != "http") {
				return errors.New("links require an http or https URL")
			}
		}
		if kind == "pie" && item.Value < 0 {
			return errors.New("pie values must be nonnegative")
		}
	}
	for index, point := range content.Points {
		if point.Time.IsZero() || index > 0 && !point.Time.After(content.Points[index-1].Time) {
			return errors.New("points need increasing timestamps")
		}
	}
	if (kind == "gauge" || kind == "stat") && content.Value == nil {
		return errors.New("numeric value required")
	}
	if kind == "gauge" && (content.Max <= 0 || *content.Value < 0 || *content.Value > content.Max) {
		return errors.New("gauge needs a value between zero and max")
	}
	if kind == "html" {
		safe, err := staticCardHTML(content.HTML)
		if err == nil {
			content.HTML = safe
		}
		return err
	}
	if content.HTML != "" {
		return errors.New("HTML requires the html style")
	}
	return nil
}

func cardKeyID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func (s *server) handleCards(w http.ResponseWriter, r *http.Request, workspaceID string) {
	workspace, err := s.workspace(workspaceID)
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}
	user, err := s.workspaceUserName(r, workspace.Path)
	if err != nil {
		writeError(w, err, http.StatusForbidden)
		return
	}
	var change cardChange
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&change); err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
		if err := ensureJSONEOF(decoder); err != nil {
			writeError(w, err, http.StatusBadRequest)
			return
		}
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.uiStateMu.Lock()
	defer s.uiStateMu.Unlock()
	path := filepath.Join(filepath.Dir(userUIStatePath(workspace.Path, user)), "cards.json")
	cards := []workspaceCard{}
	data, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(data, &cards)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, err, 500)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet {
		writeJSON(w, cards)
		return
	}
	cards, id, status, err := applyCardChange(cards, change)
	if err != nil {
		writeError(w, err, status)
		return
	}
	if err := saveJSONStateFile(path, ".cards-*.tmp", cards); err != nil {
		writeError(w, err, 500)
		return
	}
	writeJSON(w, map[string]any{"id": id, "cards": cards})
}

func applyCardChange(cards []workspaceCard, change cardChange) ([]workspaceCard, string, int, error) {
	index := slices.IndexFunc(cards, func(card workspaceCard) bool { return card.ID == change.ID })
	if change.Action == "create" {
		sources, ok := cardSources[change.Kind]
		title := strings.TrimSpace(change.Title)
		if !ok || title == "" || len(title) > 160 || len(cards) >= 64 {
			return nil, "", 400, errors.New("invalid style, title or card limit reached")
		}
		card := workspaceCard{ID: rand.Text(), Title: title, Kind: change.Kind, Source: change.Source, Version: 1}
		if change.Source == "agent" {
			if len(change.Key) < 26 || len(change.Key) > 128 {
				return nil, "", 400, errors.New("a private publisher key is required")
			}
			if err := change.Content.validate(change.Kind); err != nil {
				return nil, "", 400, err
			}
			card.ID, card.Content, card.Updated = cardKeyID(change.Key), change.Content, time.Now().UTC().Format(time.RFC3339)
			if slices.ContainsFunc(cards, func(existing workspaceCard) bool { return existing.ID == card.ID }) {
				return nil, "", 409, errors.New("publisher key already in use")
			}
		} else if !slices.Contains(sources, change.Source) || change.Content != nil {
			return nil, "", 400, errors.New("incompatible system source")
		}
		return append(cards, card), card.ID, 200, nil
	}
	if index < 0 {
		return nil, "", 404, errors.New("card removed or unknown")
	}
	card := &cards[index]
	if card.Version != change.Version {
		return nil, "", 409, errors.New("card changed; read its latest version")
	}
	switch change.Action {
	case "update":
		if card.Source != "agent" || cardKeyID(change.Key) != card.ID {
			return nil, "", 403, errors.New("publisher key required")
		}
		if err := change.Content.validate(card.Kind); err != nil {
			return nil, "", 400, err
		}
		card.Content, card.Updated = change.Content, time.Now().UTC().Format(time.RFC3339)
	case "remove":
		return slices.Delete(cards, index, index+1), change.ID, 200, nil
	case "layout":
		if change.Collapsed != nil {
			card.Collapsed = *change.Collapsed
		}
		if change.Height != nil {
			if !slices.Contains([]int{0, 180, 320}, *change.Height) {
				return nil, "", 400, errors.New("invalid height")
			}
			card.Height = *change.Height
		}
		if change.Before != nil && *change.Before != card.ID {
			moved := *card
			moved.Version++
			cards = slices.Delete(cards, index, index+1)
			to := len(cards)
			if *change.Before != "" {
				to = slices.IndexFunc(cards, func(other workspaceCard) bool { return other.ID == *change.Before })
				if to < 0 {
					return nil, "", 409, errors.New("destination changed")
				}
			}
			return slices.Insert(cards, to, moved), change.ID, 200, nil
		}
	default:
		return nil, "", 400, errors.New("unknown card action")
	}
	card.Version++
	return cards, change.ID, 200, nil
}
