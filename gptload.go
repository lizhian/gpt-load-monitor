package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type upstream struct {
	baseURL string
	key     string
	client  *http.Client
	slots   chan struct{}
}

type section[T any] struct {
	Data      *T     `json:"data,omitempty"`
	UpdatedAt int64  `json:"updated_at_ms,omitempty"`
	Error     string `json:"error,omitempty"`
}

type dashboardResponse struct {
	Health       section[healthData]      `json:"health"`
	Concurrency  section[concurrencyData] `json:"concurrency"`
	Quota        section[quotaData]       `json:"quota"`
	Usage        section[usageData]       `json:"usage"`
	AccessKeys   section[accessKeyData]   `json:"access_keys"`
	WeightAdjust *weightAdjustStatus      `json:"weight_adjust,omitempty"`
}

func (a *app) dashboard(w http.ResponseWriter, r *http.Request) {
	if !a.authenticated(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "请重新登录。"})
		return
	}
	rangeValue := r.URL.Query().Get("range")
	switch rangeValue {
	case "":
		rangeValue = "24h"
	case "6h", "12h", "24h", "today", "yesterday", "7d", "30d":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请选择有效的统计时间范围。"})
		return
	}

	var response dashboardResponse
	var group sync.WaitGroup
	ctx, cancel := context.WithTimeout(r.Context(), 13*time.Second)
	defer cancel()
	now := a.now()
	span := selectedUsageSpan(rangeValue, now)
	healthReady := make(chan struct{})
	usageReady := make(chan struct{})
	var report *usageReport
	group.Go(func() {
		defer close(healthReady)
		response.Health = loadSection(ctx, a.upstream, "/api/health", "服务健康暂时无法更新。", identity[healthData])
	})
	group.Go(func() {
		response.Concurrency = loadSection(ctx, a.upstream, "/api/home", "并发数据暂时无法更新。", func(data homeData) concurrencyData {
			return data.Concurrency
		})
	})
	group.Go(func() {
		snap, err := a.upstream.loadSubscriptionSnapshot(ctx, now)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("GPT-Load subscription snapshot: %v", err)
			}
			response.Quota = section[quotaData]{Error: "订阅额度暂时无法更新。"}
			return
		}
		response.Quota = section[quotaData]{Data: &snap.Data, UpdatedAt: time.Now().UnixMilli()}
	})
	group.Go(func() {
		data, err := a.upstream.readUsage(ctx, span, nil)
		if err == nil {
			report = &data
		}
		close(usageReady)
		if err != nil {
			log.Printf("GPT-Load usage: %v", err)
			response.Usage.Error = "用量统计暂时无法更新。"
			return
		}
		<-healthReady
		usage := data.dashboardUsage(rangeValue, span, response.Health.Data)
		response.Usage = section[usageData]{Data: &usage, UpdatedAt: time.Now().UnixMilli()}
	})
	group.Go(func() {
		keys, err := fetchUpstream[[]accessKey](ctx, a.upstream, "/api/access-keys/options")
		if err != nil {
			log.Printf("GPT-Load access key options: %v", err)
			response.AccessKeys.Error = "AccessKey 列表暂时无法更新。"
			return
		}
		<-usageReady
		if report != nil {
			keys = report.includeHistoricalKeys(keys)
		}
		response.AccessKeys = a.upstream.accessKeyUsage(ctx, span, keys)
	})
	group.Wait()
	if a.adjuster != nil {
		status := a.adjuster.Status()
		response.WeightAdjust = &status
	}
	writeJSON(w, http.StatusOK, response)
}

func identity[T any](data T) T { return data }

func loadSection[In, Out any](ctx context.Context, upstream upstream, path, message string, project func(In) Out) section[Out] {
	data, err := fetchUpstream[In](ctx, upstream, path)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("GPT-Load %s: %v", path, err)
		}
		return section[Out]{Error: message}
	}
	projected := project(data)
	return section[Out]{Data: &projected, UpdatedAt: time.Now().UnixMilli()}
}

func fetchUpstream[T any](ctx context.Context, upstream upstream, path string) (T, error) {
	return doUpstream[T](ctx, upstream, http.MethodGet, path, nil)
}

func (u upstream) updateCredentialWeight(ctx context.Context, groupID, credentialID int64, weight int) error {
	path := fmt.Sprintf("/api/groups/%d/credentials/%d", groupID, credentialID)
	_, err := doUpstream[json.RawMessage](ctx, u, http.MethodPut, path, struct {
		WeightManual int `json:"weight_manual"`
	}{WeightManual: weight})
	return err
}

func doUpstream[T any](ctx context.Context, upstream upstream, method, path string, payload any) (T, error) {
	var zero T
	select {
	case upstream.slots <- struct{}{}:
		defer func() { <-upstream.slots }()
	case <-ctx.Done():
		return zero, ctx.Err()
	}
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return zero, err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, upstream.baseURL+path, body)
	if err != nil {
		return zero, err
	}
	request.Header.Set("Authorization", "Bearer "+upstream.key)
	request.Header.Set("Accept", "application/json")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := upstream.client.Do(request)
	if err != nil {
		return zero, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return zero, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	var envelope struct {
		Code *int `json:"code"`
		Data *T   `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&envelope); err != nil {
		return zero, errors.New("invalid management API response")
	}
	if envelope.Code == nil || *envelope.Code != 0 {
		return zero, errors.New("management API did not return success")
	}
	if method == http.MethodGet {
		if envelope.Data == nil {
			return zero, errors.New("management API did not return data")
		}
		return *envelope.Data, nil
	}
	if envelope.Data != nil {
		return *envelope.Data, nil
	}
	return zero, nil
}

// These response types select the dashboard's fields from upstream JSON.
type healthData struct {
	Version    string           `json:"version"`
	Uptime     int64            `json:"uptime_seconds"`
	ObservedAt int64            `json:"observed_at_ms"`
	Counts     credentialCounts `json:"counts"`
	Groups     []groupHealth    `json:"groups"`
}

type credentialCounts struct {
	Credentials int `json:"credentials"`
	Available   int `json:"available"`
	Cooldown    int `json:"cooldown"`
	Blacklisted int `json:"blacklisted"`
}

type groupHealth struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Counts  struct {
		credentialCounts
		ModelCooldown int `json:"model_cooldown"`
	} `json:"counts"`
}

type homeData struct {
	Concurrency concurrencyData `json:"concurrency"`
}

type concurrencyData struct {
	Current int `json:"current"`
	Limit   int `json:"limit"`
}

type quotaData struct {
	ObservedAt int64          `json:"observed_at_ms"`
	Items      []subscription `json:"items"`
}

type subscription struct {
	ChannelName         string                 `json:"channel_name"`
	GroupCount          int                    `json:"group_count"`
	AvailableGroupCount int                    `json:"available_group_count"`
	Credential          subscriptionCredential `json:"credential"`
}

type subscriptionCredential struct {
	ID              int64        `json:"credential_id"`
	DisplayName     string       `json:"display_name,omitempty"`
	EffectiveStatus string       `json:"effective_status"`
	Weight          *int         `json:"weight,omitempty"`
	WeightMin       *int         `json:"weight_min,omitempty"`
	WeightMax       *int         `json:"weight_max,omitempty"`
	Observation     *observation `json:"observation,omitempty"`
}

func maskSubscriptionName(name string) string {
	chars := []rune(strings.TrimSpace(name))
	switch len(chars) {
	case 0:
		return ""
	case 1:
		return "***"
	case 2:
		return string(chars[0]) + "***"
	default:
		return string(chars[0]) + "***" + string(chars[len(chars)-1])
	}
}

type observation struct {
	State      string `json:"state"`
	ObservedAt int64  `json:"observed_at_ms"`
	Snapshot   *struct {
		Plan *struct {
			Name string `json:"name"`
		} `json:"plan_summary,omitempty"`
		Windows []quotaWindow `json:"quota_windows"`
	} `json:"snapshot,omitempty"`
}

type quotaWindow struct {
	ID               string   `json:"id"`
	Label            string   `json:"label"`
	Scope            string   `json:"scope"`
	Unit             string   `json:"unit"`
	Used             *float64 `json:"used,omitempty"`
	Limit            *float64 `json:"limit,omitempty"`
	Remaining        *float64 `json:"remaining,omitempty"`
	Utilization      *float64 `json:"utilization,omitempty"`
	ResetAt          *int64   `json:"reset_at_ms,omitempty"`
	Seconds          int64    `json:"window_seconds"`
	State            string   `json:"state"`
	SafeLevel        *float64 `json:"safe_level_percent,omitempty"`
	UsedUSD          *float64 `json:"used_amount_usd,omitempty"`
	EstimateUSD      *float64 `json:"estimated_limit_usd,omitempty"`
	SafeUSD          *float64 `json:"safe_amount_usd,omitempty"`
	AheadUSD         *float64 `json:"ahead_amount_usd,omitempty"`
	Incomplete       bool     `json:"pricing_incomplete,omitempty"`
	usageFetchFailed bool
}

func (w quotaWindow) isAccountPercent() bool {
	return w.Unit == "percent" && (w.Scope == "" || w.Scope == "account")
}

func (w quotaWindow) isWeeklyAccountPercent() bool {
	return w.isAccountPercent() && w.Seconds == weekSeconds
}

func (w quotaWindow) remainingSafeQuota() (float64, bool) {
	if w.usageFetchFailed {
		return 0, false
	}
	if w.AheadUSD != nil {
		return 0, true
	}
	if w.SafeUSD != nil {
		if *w.SafeUSD <= 0 {
			return 0, true
		}
		return *w.SafeUSD, true
	}
	return 0, false
}

func (o *observation) remainingSafeQuota() (float64, bool) {
	if o == nil || o.Snapshot == nil || len(o.Snapshot.Windows) == 0 {
		return 0, false
	}
	hasWeekly := false
	for _, w := range o.Snapshot.Windows {
		if w.isWeeklyAccountPercent() {
			hasWeekly = true
			break
		}
	}
	var minSafe float64
	found := false
	for _, w := range o.Snapshot.Windows {
		if hasWeekly && !w.isWeeklyAccountPercent() {
			continue
		}
		safe, ok := w.remainingSafeQuota()
		if !ok {
			continue
		}
		if !found || safe < minSafe {
			minSafe = safe
			found = true
		}
	}
	return minSafe, found
}

type usageData struct {
	Range       string `json:"range"`
	Granularity string `json:"granularity"`
	From        int64  `json:"from_ms"`
	To          int64  `json:"to_ms"`
	ObservedAt  int64  `json:"observed_at_ms"`
	Summary     struct {
		usageSummary
		InputTokens int64 `json:"input_tokens"`
	} `json:"summary"`
	Series   []usageBucket `json:"series"`
	Rankings struct {
		Models []modelRanking `json:"models"`
		Groups []groupRanking `json:"groups"`
	} `json:"rankings"`
}

type usageBucket struct {
	Start    int64 `json:"bucket_start_ms"`
	End      int64 `json:"bucket_end_ms"`
	Requests int64 `json:"request_count"`
	Failures int64 `json:"failure_count"`
}

type rankingTotals struct {
	Requests int64  `json:"request_count"`
	Tokens   int64  `json:"total_tokens"`
	Cost     string `json:"estimated_cost_nano_usd"`
}
