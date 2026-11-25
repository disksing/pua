package serve

import (
	"errors"
	"net/http"
)

type cardQuotaWindow struct {
	Label            string   `json:"label"`
	RemainingPercent *float64 `json:"remainingPercent"`
	Display          string   `json:"display,omitempty"`
	Used             *float64 `json:"used,omitempty"`
	Limit            *float64 `json:"limit,omitempty"`
	Unit             string   `json:"unit,omitempty"`
	Unlimited        bool     `json:"unlimited,omitempty"`
	ResetsAt         string   `json:"resetsAt,omitempty"`
	Stale            bool     `json:"stale,omitempty"`
}

type cardQuotaProvider struct {
	Label        string            `json:"label"`
	AccountLabel string            `json:"accountLabel,omitempty"`
	PlanLabel    string            `json:"planLabel,omitempty"`
	Stale        bool              `json:"stale,omitempty"`
	Status       string            `json:"status"`
	Quotas       []cardQuotaWindow `json:"quotas"`
}

type cardQuotaSnapshot struct {
	Configured *bool               `json:"configured"`
	Connected  *bool               `json:"connected"`
	Stale      bool                `json:"stale,omitempty"`
	Providers  []cardQuotaProvider `json:"providers"`
}

func (s *server) serveMagpieQuota(w http.ResponseWriter, r *http.Request) {
	var response struct {
		Quota cardQuotaSnapshot `json:"quota"`
	}
	_, client, err := s.agents.agentHubRuntimeConfig()
	if err == nil {
		err = client.doJSON(r.Context(), http.MethodGet, "/v1/quota", nil, &response)
	}
	quota := response.Quota
	if err != nil || quota.Configured == nil || quota.Connected == nil || quota.Providers == nil {
		writeError(w, errors.New("AgentHub quota unavailable"), http.StatusBadGateway)
		return
	}
	writeJSON(w, quota)
}
