package companion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/disksing/pua/agenthub/internal/config"
)

const (
	upstreamTimeout = 8 * time.Second
	maxResponseSize = 2 << 20
	staleAfter      = 15 * time.Minute
	retryAfter      = 5 * time.Second
)

type Quota struct {
	Key                   string   `json:"key"`
	Kind                  string   `json:"kind"`
	Label                 string   `json:"label"`
	RemainingPercent      *float64 `json:"remainingPercent"`
	UsedPercent           *float64 `json:"usedPercent"`
	ResetInSeconds        *int64   `json:"resetInSeconds,omitempty"`
	ResetsAt              string   `json:"resetsAt,omitempty"`
	WindowPositionPercent *float64 `json:"windowPositionPercent,omitempty"`
	Status                string   `json:"status"`
	Used                  *float64 `json:"used,omitempty"`
	Limit                 *float64 `json:"limit,omitempty"`
	Unit                  string   `json:"unit,omitempty"`
	Display               string   `json:"display,omitempty"`
	Unlimited             bool     `json:"unlimited,omitempty"`
	Expired               bool     `json:"expired,omitempty"`
	Stale                 bool     `json:"stale,omitempty"`
}

type ProviderQuota struct {
	ID           string  `json:"id"`
	Provider     string  `json:"provider"`
	Label        string  `json:"label"`
	AccountLabel string  `json:"accountLabel,omitempty"`
	PlanLabel    string  `json:"planLabel,omitempty"`
	CapturedAt   string  `json:"capturedAt,omitempty"`
	Status       string  `json:"status"`
	Stale        bool    `json:"stale,omitempty"`
	ReadDeferred bool    `json:"readDeferred,omitempty"`
	Error        string  `json:"error,omitempty"`
	Quotas       []Quota `json:"quotas"`
}

type QuotaSnapshot struct {
	Configured        bool            `json:"configured"`
	Connected         bool            `json:"connected"`
	Stale             bool            `json:"stale,omitempty"`
	Error             string          `json:"error,omitempty"`
	UpdatedAt         time.Time       `json:"updatedAt,omitzero"`
	StaleAfterSeconds int             `json:"staleAfterSeconds"`
	Providers         []ProviderQuota `json:"providers"`
}

type quotaCache struct {
	settings  config.Magpie
	expiresAt time.Time
	snapshot  QuotaSnapshot
	inFlight  chan struct{}
}

type Service struct {
	client *http.Client
	now    func() time.Time
	mu     sync.Mutex
	cache  *quotaCache
}

func NewService(client *http.Client) *Service {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	localClient := http.Client{Transport: transport, Timeout: upstreamTimeout}
	if client != nil {
		localClient = *client
	}
	localClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Service{client: &localClient, now: time.Now, cache: &quotaCache{}}
}

func (service *Service) Configure(settings config.Magpie) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.cache.settings != settings {
		service.cache = &quotaCache{settings: settings}
	}
}

func emptySnapshot(enabled bool) QuotaSnapshot {
	return QuotaSnapshot{
		Configured: enabled, StaleAfterSeconds: int(staleAfter.Seconds()),
		Providers: []ProviderQuota{},
	}
}

func (service *Service) Snapshot(ctx context.Context) QuotaSnapshot {
	for {
		service.mu.Lock()
		cache := service.cache
		now := service.now()
		if !cache.settings.Enabled || ctx.Err() != nil {
			service.mu.Unlock()
			return emptySnapshot(cache.settings.Enabled)
		}
		if now.Before(cache.expiresAt) {
			snapshot := cache.snapshot
			service.mu.Unlock()
			return snapshot.at(now)
		}
		if pending := cache.inFlight; pending != nil {
			service.mu.Unlock()
			select {
			case <-pending:
			case <-ctx.Done():
			}
			continue
		}
		pending := make(chan struct{})
		cache.inFlight = pending
		previous := cache.snapshot
		service.mu.Unlock()

		providers, err := service.fetch(context.WithoutCancel(ctx), cache.settings)
		finishedAt := service.now()
		snapshot := emptySnapshot(true)
		ttl := time.Duration(cache.settings.RefreshIntervalSeconds) * time.Second
		if err == nil {
			snapshot.Connected = true
			snapshot.UpdatedAt = finishedAt
			snapshot.Providers = mergeReadings(providers, previous.Providers)
		} else {
			snapshot.UpdatedAt = previous.UpdatedAt
			if !previous.UpdatedAt.IsZero() {
				snapshot.Providers = previous.Providers
				snapshot.Stale = true
			}
			snapshot.Error = err.Error()
			ttl = retryAfter
		}

		service.mu.Lock()
		cache.inFlight = nil
		close(pending)
		current := cache == service.cache
		if current {
			cache.snapshot, cache.expiresAt = snapshot, finishedAt.Add(ttl)
		}
		service.mu.Unlock()
		if current {
			return snapshot.at(finishedAt)
		}
	}
}

func (service *Service) TestConnection(ctx context.Context, settings config.Magpie) (int, error) {
	providers, err := service.fetch(ctx, settings)
	return len(providers), err
}

type magpieRow struct {
	Provider     string         `json:"provider"`
	Name         string         `json:"name"`
	Kind         string         `json:"kind"`
	Plan         string         `json:"plan"`
	User         string         `json:"user"`
	From         string         `json:"from"`
	Alias        string         `json:"alias"`
	Seat         string         `json:"seat"`
	Windows      []magpieWindow `json:"windows"`
	Balance      string         `json:"balance"`
	ReadAt       string         `json:"readAt"`
	AsOf         string         `json:"asOf"`
	ReadDeferred bool           `json:"readDeferred"`
	Error        string         `json:"error"`
}

type magpieWindow struct {
	Name      string   `json:"name"`
	Used      *float64 `json:"used"`
	Remaining *float64 `json:"remaining"`
	ResetsAt  string   `json:"resetsAt"`
	Display   string   `json:"display"`
	Amount    *float64 `json:"amount"`
	Limit     *float64 `json:"limit"`
	Unit      string   `json:"unit"`
	Balance   *float64 `json:"balance"`
	Text      bool     `json:"text"`
	Unlimited bool     `json:"unlimited"`
}

func (service *Service) fetch(ctx context.Context, settings config.Magpie) ([]ProviderQuota, error) {
	endpoint, err := settings.QuotaURL()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, upstreamTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, errors.New("Invalid Magpie quota endpoint")
	}
	request.Header.Set("Accept", "application/json")
	response, err := service.client.Do(request)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, errors.New("Magpie quota request timed out")
		}
		return nil, errors.New("Could not connect to Magpie")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("Magpie quota request returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseSize+1))
	if err != nil || len(body) > maxResponseSize {
		return nil, errors.New("Could not read the Magpie quota response")
	}
	var envelope struct {
		Data []magpieRow `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Data == nil {
		return nil, errors.New("Magpie returned an invalid quota response")
	}
	return mapRows(envelope.Data)
}

var providerIdentifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/-]{0,127}$`)

func mapRows(rows []magpieRow) ([]ProviderQuota, error) {
	providers := make([]ProviderQuota, 0, len(rows))
	indexByID := make(map[string]int, len(rows))
	for _, row := range rows {
		if strings.TrimSpace(row.Provider) == "" || strings.TrimSpace(row.Kind) == "" {
			return nil, errors.New("Magpie returned a quota row without an identity")
		}
		identity := firstNonEmpty(row.User, row.Alias, row.Seat)
		id := stableID("magpie", row.From, row.Provider, row.Kind, strings.ToLower(identity))
		provider := normalizeRow(id, row)
		if index, exists := indexByID[id]; exists {
			if identity == "" {
				provider.AccountLabel = "Unknown account"
				provider.Error = "Magpie did not distinguish these accounts"
				provider.Quotas = []Quota{}
				provider.Status = "unavailable"
				provider.CapturedAt = ""
				providers[index] = provider
			} else if preferReading(provider, providers[index]) {
				providers[index] = provider
			}
			continue
		}
		indexByID[id] = len(providers)
		providers = append(providers, provider)
	}
	return providers, nil
}

func normalizeRow(id string, row magpieRow) ProviderQuota {
	providerID := row.Provider
	if !providerIdentifier.MatchString(providerID) || cleanDisplay(providerID, row.User) == "" {
		providerID = stableID("provider", row.Provider)
	}
	provider := ProviderQuota{
		ID: id, Provider: providerID, Label: firstNonEmpty(cleanDisplay(row.Name, row.User), cleanDisplay(row.Provider, row.User), "Provider"),
		AccountLabel: id[:8], PlanLabel: cleanDisplay(row.Plan, row.User), Status: "healthy",
		ReadDeferred: row.ReadDeferred, Quotas: make([]Quota, 0, len(row.Windows)+1),
	}
	if captured := readingTime(row.ReadAt, row.AsOf); !captured.IsZero() {
		provider.CapturedAt = captured.Format(time.RFC3339Nano)
	}
	keys := make(map[string]int, len(row.Windows))
	for _, window := range row.Windows {
		quota := normalizeWindow(window, row.User)
		keys[quota.Key]++
		if count := keys[quota.Key]; count > 1 {
			quota.Key = stableID(quota.Key, fmt.Sprint(count))
		}
		provider.Quotas = append(provider.Quotas, quota)
		if statusRank(quota.Status) > statusRank(provider.Status) {
			provider.Status = quota.Status
		}
	}
	if balance := cleanDisplay(row.Balance, row.User); balance != "" {
		provider.Quotas = append(provider.Quotas, Quota{
			Key: stableID("balance"), Kind: "balance", Label: "Balance", Display: balance, Status: "healthy",
		})
	}
	if row.Error != "" {
		provider.Error = "Magpie could not refresh this account"
		provider.Status = "unavailable"
	}
	if len(provider.Quotas) == 0 && provider.Error == "" {
		provider.Error = "Magpie has no quota reading for this account"
		provider.Status = "unavailable"
	}
	return provider
}

func normalizeWindow(window magpieWindow, user string) Quota {
	name := firstNonEmpty(cleanDisplay(window.Name, user), "Quota")
	quota := Quota{
		Key: stableID("window", window.Name), Kind: quotaKind(name), Label: name,
		Unlimited: window.Unlimited, Status: "healthy",
		Display: cleanDisplay(window.Display, user),
	}
	if reset := readingTime(window.ResetsAt); !reset.IsZero() {
		quota.ResetsAt = reset.Format(time.RFC3339Nano)
	}
	unit := cleanDisplay(window.Unit, user)
	if window.Balance != nil {
		quota.Kind = "balance"
		if quota.Display == "" {
			quota.Display = strings.TrimSpace(fmt.Sprintf("%g %s", *window.Balance, unit))
		}
	}
	if window.Unlimited || window.Text || window.Balance != nil {
		return quota
	}
	quota.RemainingPercent, quota.UsedPercent = percentage(window.Remaining), percentage(window.Used)
	switch {
	case quota.RemainingPercent != nil && quota.UsedPercent == nil:
		quota.UsedPercent = percentageComplement(quota.RemainingPercent)
	case quota.RemainingPercent == nil && quota.UsedPercent != nil:
		quota.RemainingPercent = percentageComplement(quota.UsedPercent)
	}
	if quota.RemainingPercent != nil {
		quota.Status = derivedStatus(*quota.RemainingPercent)
	} else if quota.Display == "" {
		quota.Status = "unavailable"
	}
	if unit != "" && window.Limit != nil {
		quota.Used, quota.Limit, quota.Unit = window.Amount, window.Limit, unit
	}
	return quota
}

func mergeReadings(providers, previous []ProviderQuota) []ProviderQuota {
	byID := make(map[string]ProviderQuota, len(previous))
	for _, provider := range previous {
		byID[provider.ID] = provider
	}
	for index, provider := range providers {
		old, exists := byID[provider.ID]
		if !exists || len(old.Quotas) == 0 || provider.AccountLabel == "Unknown account" {
			continue
		}
		if (provider.Error != "" || provider.ReadDeferred) && (len(provider.Quotas) == 0 || preferReading(old, provider)) {
			old.Error, old.ReadDeferred = provider.Error, provider.ReadDeferred
			providers[index] = old
		}
	}
	return providers
}

func (snapshot QuotaSnapshot) at(now time.Time) QuotaSnapshot {
	markAllStale := snapshot.Stale
	snapshot.Providers = slices.Clone(snapshot.Providers)
	for index := range snapshot.Providers {
		provider := &snapshot.Providers[index]
		captured := readingTime(provider.CapturedAt)
		markCardStale := markAllStale || (!captured.IsZero() && now.Sub(captured) > staleAfter) || (len(provider.Quotas) > 0 && provider.Error != "")
		provider.Quotas = slices.Clone(provider.Quotas)
		for quotaIndex := range provider.Quotas {
			window := &provider.Quotas[quotaIndex]
			reset := readingTime(window.ResetsAt)
			if !reset.IsZero() {
				seconds := int64(math.Max(0, math.Ceil(reset.Sub(now).Seconds())))
				window.Expired = !reset.After(now) && (captured.IsZero() || !captured.After(reset))
				if !window.Expired && reset.After(now) {
					window.ResetInSeconds = &seconds
					window.WindowPositionPercent = windowPosition(window.Kind, seconds)
				}
			}
			window.Stale = markCardStale || window.Expired
			provider.Stale = provider.Stale || window.Stale
		}
		provider.Stale = provider.Stale || markCardStale
		snapshot.Stale = snapshot.Stale || provider.Stale
	}
	return snapshot
}

func preferReading(candidate, existing ProviderQuota) bool {
	newTime, oldTime := readingTime(candidate.CapturedAt), readingTime(existing.CapturedAt)
	return newTime.After(oldTime) || (newTime.Equal(oldTime) && candidate.Error == "" && existing.Error != "")
}

func stableID(parts ...string) string {
	data, _ := json.Marshal(parts)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}

func cleanDisplay(value, user string) string {
	value, user = strings.TrimSpace(value), strings.TrimSpace(user)
	if strings.Contains(value, "@") || (len(user) >= 4 && strings.Contains(strings.ToLower(value), strings.ToLower(user))) {
		return ""
	}
	value = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return -1
		}
		return character
	}, value)
	characters := []rune(value)
	if len(characters) > 128 {
		return string(characters[:128])
	}
	return value
}

func readingTime(values ...string) time.Time {
	for _, value := range values {
		if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func quotaKind(name string) string {
	value := strings.ToLower(name)
	switch {
	case strings.Contains(value, "five") || strings.Contains(value, "5h") || strings.Contains(value, "5-hour"):
		return "5h"
	case strings.Contains(value, "seven") || strings.Contains(value, "weekly") || strings.Contains(value, "7-day") || strings.Contains(value, "7d"):
		return "7d"
	case strings.Contains(value, "month"):
		return "monthly"
	case strings.Contains(value, "credit"):
		return "credits"
	default:
		return name
	}
}

func windowPosition(kind string, seconds int64) *float64 {
	durations := map[string]float64{"5h": 5 * 60 * 60, "7d": 7 * 24 * 60 * 60, "monthly": 30 * 24 * 60 * 60}
	if duration, ok := durations[kind]; ok {
		value := clampPercent(100 * float64(seconds) / duration)
		return &value
	}
	return nil
}

func percentage(value *float64) *float64 {
	if value == nil {
		return nil
	}
	result := clampPercent(*value)
	return &result
}

func percentageComplement(value *float64) *float64 {
	result := 100 - *value
	return &result
}

func clampPercent(value float64) float64 {
	return math.Min(100, math.Max(0, value))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func derivedStatus(remaining float64) string {
	switch {
	case remaining <= 10:
		return "critical"
	case remaining <= 25:
		return "danger"
	case remaining <= 40:
		return "warning"
	default:
		return "healthy"
	}
}

func statusRank(status string) int {
	return map[string]int{"healthy": 0, "warning": 1, "danger": 2, "critical": 3, "unavailable": 4}[status]
}
