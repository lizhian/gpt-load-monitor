package main

import (
	"cmp"
	"context"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	weekSeconds        = 7 * 24 * 60 * 60
	credentialPageSize = 100
	maxCredentialPages = 1000
)

type subscriptionSnapshot struct {
	Data        quotaData
	Memberships []subscriptionCredentialLink
}

type subscriptionCredentialLink struct {
	GroupID       int64
	CredentialID  int64
	CurrentWeight *int
	Observation   *observation
}

type accountKey struct {
	channel      string
	email        string
	credentialID int64
}

type upstreamModernGroups struct {
	ObservedAt int64 `json:"observed_at_ms"`
	Items      []struct {
		ID             int64  `json:"id"`
		Name           string `json:"name"`
		ChannelID      string `json:"channel_id"`
		ChannelName    string `json:"channel_name"`
		ConnectionType string `json:"connection_type"`
		Enabled        bool   `json:"enabled"`
	} `json:"items"`
}

type upstreamGroupCredentials struct {
	ObservedAt int64                `json:"observed_at_ms"`
	Items      []upstreamCredential `json:"items"`
	Pagination struct {
		Page       int `json:"page"`
		PageSize   int `json:"page_size"`
		TotalItems int `json:"total_items"`
		TotalPages int `json:"total_pages"`
	} `json:"pagination"`
}

type upstreamCredential struct {
	ID               int64  `json:"credential_id"`
	Name             string `json:"name"`
	ConfiguredStatus string `json:"configured_status"`
	EffectiveStatus  string `json:"effective_status"`
	Weight           *int   `json:"weight"`
	WeightManual     *int   `json:"weight_manual"`
	Account          struct {
		Email     string `json:"email"`
		EmailMask string `json:"email_mask"`
	} `json:"account"`
	Observation *upstreamObservation `json:"observation"`
}

type upstreamObservation struct {
	State      string `json:"state"`
	ObservedAt int64  `json:"observed_at_ms"`
	Snapshot   *struct {
		Plan *struct {
			Name string `json:"name"`
		} `json:"plan_summary"`
		Account *struct {
			Email string `json:"email"`
		} `json:"account_summary"`
		Windows []quotaWindow `json:"quota_windows"`
	} `json:"snapshot"`
}

type activeCredentialMember struct {
	groupID       int64
	cred          upstreamCredential
	currentWeight *int
}

type aggregatedSubscriptionAccount struct {
	key           accountKey
	channelName   string
	allIDs        []int64
	seenAllIDs    map[int64]bool
	activeMembers []activeCredentialMember
}

type preparedSubscriptionAccount struct {
	item   subscription
	allIDs []int64
	links  []subscriptionCredentialLink
}

func (u upstream) loadSubscriptionSnapshot(ctx context.Context, now time.Time) (subscriptionSnapshot, error) {
	groupsResp, err := fetchUpstream[upstreamModernGroups](ctx, u, "/api/modern/groups")
	if err != nil {
		return subscriptionSnapshot{}, err
	}

	type groupResult struct {
		enabled     bool
		channelKey  string
		channelName string
		groupID     int64
		observedAt  int64
		items       []upstreamCredential
		err         error
	}

	var subGroups []groupResult
	for _, g := range groupsResp.Items {
		if g.ID <= 0 || !strings.EqualFold(strings.TrimSpace(g.ConnectionType), "subscription") {
			continue
		}
		channelKey := strings.ToLower(strings.TrimSpace(g.ChannelID))
		if channelKey == "" {
			channelKey = strings.ToLower(strings.TrimSpace(g.ChannelName))
		}
		channelName := strings.TrimSpace(g.ChannelName)
		if channelName == "" {
			channelName = strings.TrimSpace(g.ChannelID)
		}
		subGroups = append(subGroups, groupResult{
			enabled:     g.Enabled,
			channelKey:  channelKey,
			channelName: channelName,
			groupID:     g.ID,
		})
	}

	var wg sync.WaitGroup
	for i := range subGroups {
		wg.Go(func() {
			items, obsAt, fetchErr := u.fetchGroupCredentialsAllPages(ctx, subGroups[i].groupID)
			subGroups[i].items = items
			subGroups[i].observedAt = obsAt
			subGroups[i].err = fetchErr
		})
	}
	wg.Wait()

	observedAtMS := groupsResp.ObservedAt
	accountsByKey := make(map[accountKey]*aggregatedSubscriptionAccount)
	for _, g := range subGroups {
		if g.err != nil {
			return subscriptionSnapshot{}, g.err
		}
		observedAtMS = max(observedAtMS, g.observedAt)
		for _, cred := range g.items {
			if cred.ID <= 0 {
				continue
			}
			key := subscriptionAccountKey(g.channelKey, cred)
			acc := accountsByKey[key]
			if acc == nil {
				acc = &aggregatedSubscriptionAccount{
					key:         key,
					channelName: g.channelName,
					seenAllIDs:  make(map[int64]bool),
				}
				accountsByKey[key] = acc
			} else if acc.channelName == "" && g.channelName != "" {
				acc.channelName = g.channelName
			}
			if !acc.seenAllIDs[cred.ID] {
				acc.seenAllIDs[cred.ID] = true
				acc.allIDs = append(acc.allIDs, cred.ID)
			}
			if g.enabled && isCredentialActive(cred.ConfiguredStatus, cred.EffectiveStatus) {
				acc.activeMembers = append(acc.activeMembers, activeCredentialMember{
					groupID:       g.groupID,
					cred:          cred,
					currentWeight: resolveCredentialWeight(cred.Weight, cred.WeightManual),
				})
			}
		}
	}

	prepared := make([]preparedSubscriptionAccount, 0, len(accountsByKey))
	for _, acc := range accountsByKey {
		if len(acc.activeMembers) == 0 {
			continue
		}
		prepared = append(prepared, buildPreparedSubscriptionAccount(acc))
	}
	slices.SortFunc(prepared, func(a, b preparedSubscriptionAccount) int {
		if c := cmp.Compare(a.item.ChannelName, b.item.ChannelName); c != 0 {
			return c
		}
		if c := cmp.Compare(a.item.Credential.DisplayName, b.item.Credential.DisplayName); c != 0 {
			return c
		}
		return cmp.Compare(a.item.Credential.ID, b.item.Credential.ID)
	})

	snapshot := subscriptionSnapshot{
		Data: quotaData{
			ObservedAt: observedAtMS,
			Items:      make([]subscription, len(prepared)),
		},
	}
	for i := range prepared {
		snapshot.Data.Items[i] = prepared[i].item
		for _, link := range prepared[i].links {
			link.Observation = snapshot.Data.Items[i].Credential.Observation
			snapshot.Memberships = append(snapshot.Memberships, link)
		}
	}
	slices.SortFunc(snapshot.Memberships, func(a, b subscriptionCredentialLink) int {
		if c := cmp.Compare(a.GroupID, b.GroupID); c != 0 {
			return c
		}
		return cmp.Compare(a.CredentialID, b.CredentialID)
	})

	var enrichGroup sync.WaitGroup
	for i := range snapshot.Data.Items {
		enrichGroup.Go(func() {
			u.enrichObservationWindows(ctx, snapshot.Data.Items[i].Credential.Observation, prepared[i].allIDs, now)
		})
	}
	enrichGroup.Wait()

	return snapshot, nil
}

func (u upstream) fetchGroupCredentialsAllPages(ctx context.Context, groupID int64) ([]upstreamCredential, int64, error) {
	var all []upstreamCredential
	var observedAtMS int64
	for page := 1; page <= maxCredentialPages; page++ {
		path := fmt.Sprintf("/api/modern/groups/%d/credentials?page=%d&page_size=%d", groupID, page, credentialPageSize)
		resp, err := fetchUpstream[upstreamGroupCredentials](ctx, u, path)
		if err != nil {
			return nil, 0, err
		}
		observedAtMS = max(observedAtMS, resp.ObservedAt)
		all = append(all, resp.Items...)
		if len(resp.Items) == 0 || page >= resp.Pagination.TotalPages {
			break
		}
	}
	return all, observedAtMS, nil
}

func subscriptionAccountKey(channelKey string, cred upstreamCredential) accountKey {
	email := strings.ToLower(strings.TrimSpace(cred.Account.Email))
	if email == "" && cred.Observation != nil && cred.Observation.Snapshot != nil && cred.Observation.Snapshot.Account != nil {
		email = strings.ToLower(strings.TrimSpace(cred.Observation.Snapshot.Account.Email))
	}
	if email != "" {
		return accountKey{channel: channelKey, email: email}
	}
	return accountKey{channel: channelKey, credentialID: cred.ID}
}

func isCredentialActive(configuredStatus, effectiveStatus string) bool {
	configured := strings.ToLower(strings.TrimSpace(configuredStatus))
	if configured == "disabled" {
		return false
	}
	if configured == "" && strings.ToLower(strings.TrimSpace(effectiveStatus)) == "disabled" {
		return false
	}
	return true
}

func resolveCredentialWeight(weight, weightManual *int) *int {
	if weightManual != nil {
		value := *weightManual
		return &value
	}
	if weight != nil {
		value := *weight
		return &value
	}
	return nil
}

func isCredentialAvailable(effectiveStatus string) bool {
	return strings.EqualFold(strings.TrimSpace(effectiveStatus), "available")
}

func observationObservedAt(obs *upstreamObservation) int64 {
	if obs == nil {
		return 0
	}
	return obs.ObservedAt
}

func buildPreparedSubscriptionAccount(acc *aggregatedSubscriptionAccount) preparedSubscriptionAccount {
	rep := acc.activeMembers[0]
	groups := make(map[int64]bool, len(acc.activeMembers))
	availableGroups := make(map[int64]bool, len(acc.activeMembers))
	links := make([]subscriptionCredentialLink, 0, len(acc.activeMembers))

	var minW, maxW int
	hasWeight := false
	for _, m := range acc.activeMembers {
		groups[m.groupID] = true
		if isCredentialAvailable(m.cred.EffectiveStatus) {
			availableGroups[m.groupID] = true
		}
		if m.currentWeight != nil {
			w := *m.currentWeight
			if !hasWeight {
				minW, maxW, hasWeight = w, w, true
			} else {
				minW = min(minW, w)
				maxW = max(maxW, w)
			}
		}
		links = append(links, subscriptionCredentialLink{
			GroupID:       m.groupID,
			CredentialID:  m.cred.ID,
			CurrentWeight: m.currentWeight,
		})
		if betterRepresentative(m, rep) {
			rep = m
		}
	}

	displayName := maskSubscriptionName(rep.cred.Name)
	if displayName == "" {
		displayName = strings.TrimSpace(rep.cred.Account.EmailMask)
	}
	var weightPtr, weightMinPtr, weightMaxPtr *int
	if hasWeight {
		repW := minW
		if rep.currentWeight != nil {
			repW = *rep.currentWeight
		}
		weightPtr = &repW
		if minW != maxW {
			weightMinPtr = &minW
			weightMaxPtr = &maxW
		}
	}

	return preparedSubscriptionAccount{
		item: subscription{
			ChannelName:         acc.channelName,
			GroupCount:          len(groups),
			AvailableGroupCount: len(availableGroups),
			Credential: subscriptionCredential{
				ID:              rep.cred.ID,
				DisplayName:     displayName,
				EffectiveStatus: rep.cred.EffectiveStatus,
				Weight:          weightPtr,
				WeightMin:       weightMinPtr,
				WeightMax:       weightMaxPtr,
				Observation:     sanitizeObservation(rep.cred.Observation),
			},
		},
		allIDs: acc.allIDs,
		links:  links,
	}
}

func betterRepresentative(candidate, current activeCredentialMember) bool {
	cObs := observationObservedAt(candidate.cred.Observation)
	curObs := observationObservedAt(current.cred.Observation)
	if cObs != curObs {
		return cObs > curObs
	}
	cAvail := isCredentialAvailable(candidate.cred.EffectiveStatus)
	curAvail := isCredentialAvailable(current.cred.EffectiveStatus)
	if cAvail != curAvail {
		return cAvail
	}
	return candidate.cred.ID < current.cred.ID
}

func sanitizeObservation(src *upstreamObservation) *observation {
	if src == nil {
		return nil
	}
	dst := &observation{
		State:      src.State,
		ObservedAt: src.ObservedAt,
	}
	if src.Snapshot != nil {
		snap := &struct {
			Plan *struct {
				Name string `json:"name"`
			} `json:"plan_summary,omitempty"`
			Windows []quotaWindow `json:"quota_windows"`
		}{}
		if src.Snapshot.Plan != nil {
			snap.Plan = &struct {
				Name string `json:"name"`
			}{Name: src.Snapshot.Plan.Name}
		}
		if len(src.Snapshot.Windows) > 0 {
			snap.Windows = make([]quotaWindow, len(src.Snapshot.Windows))
			copy(snap.Windows, src.Snapshot.Windows)
		}
		dst.Snapshot = snap
	}
	return dst
}

func (u upstream) enrichObservationWindows(ctx context.Context, obs *observation, ids []int64, now time.Time) {
	if obs == nil || obs.Snapshot == nil || len(obs.Snapshot.Windows) == 0 {
		return
	}
	for i := range obs.Snapshot.Windows {
		window := &obs.Snapshot.Windows[i]
		if window.ResetAt == nil || *window.ResetAt <= 0 || window.Seconds <= 0 || window.Seconds > 31*24*60*60 {
			continue
		}
		reset := time.UnixMilli(*window.ResetAt)
		start := reset.Add(-time.Duration(window.Seconds) * time.Second)
		safeLevel := quotaSafeLevel(start, reset, now, window.Seconds == weekSeconds)
		window.SafeLevel = &safeLevel
		span := usageSpan{start.UnixMilli(), min(*window.ResetAt, obs.ObservedAt)}
		if window.isAccountPercent() && span.to > span.from {
			u.loadQuotaAmounts(ctx, window, ids, span)
		}
	}
}

func (u upstream) loadQuotaAmounts(ctx context.Context, window *quotaWindow, ids []int64, span usageSpan) {
	if len(ids) == 0 {
		window.Incomplete = true
		window.usageFetchFailed = true
		return
	}
	results := make([]*usageSummary, len(ids))
	var group sync.WaitGroup
	for i, id := range ids {
		group.Go(func() {
			report, err := u.readUsage(ctx, span, url.Values{"credential_id": {strconv.FormatInt(id, 10)}})
			if err == nil {
				results[i] = &report.Summary
			}
		})
	}
	group.Wait()
	var totalUSD float64
	var hasPrice, hasUsage bool
	for _, result := range results {
		if result == nil {
			window.Incomplete = true
			window.usageFetchFailed = true
			continue
		}
		window.Incomplete = window.Incomplete || result.incomplete()
		hasUsage = hasUsage || result.Requests > 0
		cost, err := strconv.ParseInt(result.Cost, 10, 64)
		if err != nil || cost < 0 {
			window.Incomplete = true
			window.usageFetchFailed = true
			continue
		}
		hasPrice = hasPrice || cost > 0 || (result.Requests > 0 && !result.incomplete())
		totalUSD += float64(cost) / 1e9
	}
	utilization := window.Utilization
	if utilization == nil && window.Used != nil && window.Limit != nil && *window.Limit > 0 {
		value := *window.Used / *window.Limit
		utilization = &value
	}
	if hasPrice || (utilization != nil && *utilization == 0 && !hasUsage && !window.Incomplete) {
		window.UsedUSD = roundedUSD(totalUSD)
	}
	if totalUSD == 0 || utilization == nil || *utilization <= 0 || *utilization > 1 {
		return
	}
	estimated := totalUSD / *utilization
	if math.IsInf(estimated, 0) || estimated > math.MaxFloat64/100 {
		return
	}
	window.EstimateUSD = roundedUSD(estimated)
	difference := (*window.SafeLevel/100 - *utilization) * estimated
	if difference >= 0 {
		window.SafeUSD = roundedUSD(difference)
	} else {
		window.AheadUSD = roundedUSD(-difference)
	}
}

func quotaSafeLevel(start, reset, now time.Time, weekly bool) float64 {
	if !now.After(start) {
		return 0
	}
	if !now.Before(reset) || (weekly && !now.Before(reset.Add(-12*time.Hour))) {
		return 100
	}
	if !weekly {
		return math.Round(1000*float64(now.Sub(start))/float64(reset.Sub(start))) / 10
	}
	var level float64
	for day := localMidnight(start); !day.After(now); day = day.AddDate(0, 0, 1) {
		begin := day.Add(9 * time.Hour)
		end := day.Add(18 * time.Hour)
		if begin.Before(start) {
			begin = start
		}
		if end.After(now) {
			end = now
		}
		if spent := end.Sub(begin); spent > 0 {
			weight := 18.0
			if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
				weight = 5
			}
			level += weight * spent.Hours() / 9
		}
	}
	return min(100, math.Round(10*level)/10)
}

func roundedUSD(value float64) *float64 {
	amount := math.Round(value*100) / 100
	return &amount
}
