package main

import (
	"context"
	"math"
	"net/url"
	"strconv"
	"sync"
	"time"
)

const weekSeconds = 7 * 24 * 60 * 60

func (u upstream) enrichQuota(ctx context.Context, quota *quotaData, health *healthData, now time.Time) {
	var accounts sync.WaitGroup
	for i := range quota.Items {
		accounts.Go(func() {
			account := &quota.Items[i]
			observation := account.Credential.Observation
			if observation == nil || observation.Snapshot == nil || len(observation.Snapshot.Windows) == 0 {
				return
			}
			ids := u.subscriptionCredentials(ctx, account, health)
			for i := range observation.Snapshot.Windows {
				window := &observation.Snapshot.Windows[i]
				if window.ResetAt == nil || *window.ResetAt <= 0 || window.Seconds <= 0 || window.Seconds > 31*24*60*60 {
					continue
				}
				reset := time.UnixMilli(*window.ResetAt)
				start := reset.Add(-time.Duration(window.Seconds) * time.Second)
				safeLevel := quotaSafeLevel(start, reset, now, window.Seconds == weekSeconds)
				window.SafeLevel = &safeLevel
				span := usageSpan{start.UnixMilli(), min(*window.ResetAt, observation.ObservedAt)}
				if window.Unit == "percent" && (window.Scope == "" || window.Scope == "account") && span.to > span.from {
					u.loadQuotaAmounts(ctx, window, ids, span)
				}
			}
		})
	}
	accounts.Wait()
}

func (u upstream) subscriptionCredentials(ctx context.Context, account *subscription, health *healthData) []int64 {
	primary := account.Credential.ID
	if primary <= 0 {
		return nil
	}
	if account.GroupCount <= 1 {
		return []int64{primary}
	}
	if health == nil || len(health.Groups) < account.GroupCount || account.credentialKey == "" {
		return nil
	}

	matches := make([][]int64, len(health.Groups))
	var groups sync.WaitGroup
	for i, group := range health.Groups {
		if group.ID <= 0 {
			continue
		}
		groups.Go(func() {
			query := url.Values{"credential_key": {account.credentialKey}}
			path := "/api/modern/groups/" + strconv.FormatInt(group.ID, 10) + "/credentials?" + query.Encode()
			data, err := fetchUpstream[struct {
				Items []struct {
					ID int64 `json:"credential_id"`
				} `json:"items"`
			}](ctx, u, path)
			if err != nil {
				return
			}
			for _, item := range data.Items {
				if item.ID > 0 {
					matches[i] = append(matches[i], item.ID)
				}
			}
		})
	}
	groups.Wait()
	seen := make(map[int64]bool)
	var ids []int64
	matchedGroups := 0
	for _, group := range matches {
		if len(group) > 0 {
			matchedGroups++
		}
		for _, id := range group {
			if !seen[id] {
				ids = append(ids, id)
				seen[id] = true
			}
		}
	}
	if matchedGroups != account.GroupCount || !seen[primary] {
		return nil
	}
	return ids
}

func (u upstream) loadQuotaAmounts(ctx context.Context, window *quotaWindow, ids []int64, span usageSpan) {
	if len(ids) == 0 {
		window.Incomplete = true
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
			continue
		}
		window.Incomplete = window.Incomplete || result.incomplete()
		hasUsage = hasUsage || result.Requests > 0
		cost, err := strconv.ParseInt(result.Cost, 10, 64)
		if err != nil || cost < 0 {
			window.Incomplete = true
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
