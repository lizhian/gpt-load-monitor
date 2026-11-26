package main

import (
	"cmp"
	"context"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"
)

var shanghai = time.FixedZone("Asia/Shanghai", 8*60*60)

type usageSpan struct{ from, to int64 }

func localMidnight(t time.Time) time.Time {
	local := t.In(shanghai)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, shanghai)
}

func selectedUsageSpan(period string, now time.Time) usageSpan {
	to, from := now, now.Add(-24*time.Hour)
	switch period {
	case "today":
		from = localMidnight(now)
	case "yesterday":
		to = localMidnight(now)
		from = to.AddDate(0, 0, -1)
	case "7d":
		from = now.Add(-7 * 24 * time.Hour)
	case "30d":
		from = now.Add(-30 * 24 * time.Hour)
	}
	return usageSpan{from.UnixMilli(), to.UnixMilli()}
}

type usageSummary struct {
	rankingTotals
	Success        int64 `json:"success_count"`
	Failure        int64 `json:"failure_count"`
	UncachedInput  int64 `json:"uncached_input_tokens"`
	CacheRead      int64 `json:"cache_read_tokens"`
	CacheWrite5m   int64 `json:"cache_write_5m_tokens"`
	CacheWrite1h   int64 `json:"cache_write_1h_tokens"`
	CacheWriteElse int64 `json:"cache_write_unknown_tokens"`
	Unpriced       int64 `json:"unpriced_request_count"`
	PartialPricing int64 `json:"pricing_partial_count"`
	MissingUsage   int64 `json:"usage_missing_count"`
}

func (s usageSummary) promptTokens() int64 {
	return s.UncachedInput + s.CacheRead + s.CacheWrite5m + s.CacheWrite1h + s.CacheWriteElse
}

func (s usageSummary) incomplete() bool {
	return s.Unpriced > 0 || s.PartialPricing > 0 || s.MissingUsage > 0
}

type usageReport struct {
	Granularity  string        `json:"granularity"`
	BucketWidth  int64         `json:"bucket_width_ms"`
	ObservedAt   int64         `json:"observed_at_ms"`
	Summary      usageSummary  `json:"summary"`
	Series       []usageBucket `json:"series"`
	Distribution struct {
		Model struct {
			Requests struct{ Items []modelRanking } `json:"requests"`
		} `json:"model"`
		Group struct {
			Requests struct {
				Items []struct {
					ID int64 `json:"group_id"`
					rankingTotals
				}
			} `json:"requests"`
		} `json:"group"`
		AccessKey map[string]struct {
			Items []struct {
				ID int64 `json:"access_key_id"`
			} `json:"items"`
		} `json:"access_key"`
	} `json:"distributions"`
}

type modelRanking struct {
	Model string `json:"model"`
	rankingTotals
}

type groupRanking struct {
	Group struct {
		Name string `json:"name"`
	} `json:"group"`
	rankingTotals
}

func (u upstream) readUsage(ctx context.Context, span usageSpan, filters url.Values) (usageReport, error) {
	if span.to <= span.from {
		return usageReport{Granularity: "hour", Summary: usageSummary{rankingTotals: rankingTotals{Cost: "0"}}}, nil
	}
	if filters == nil {
		filters = make(url.Values)
	}
	filters.Set("from_ms", strconv.FormatInt(span.from, 10))
	filters.Set("to_ms", strconv.FormatInt(span.to, 10))
	return fetchUpstream[usageReport](ctx, u, "/api/usage?"+filters.Encode())
}

func (r usageReport) dashboardUsage(period string, span usageSpan, health *healthData) usageData {
	result := usageData{
		Range: period, Granularity: r.Granularity, From: span.from, To: span.to,
		ObservedAt: r.ObservedAt, Series: r.Series,
	}
	result.Summary.usageSummary = r.Summary
	result.Summary.InputTokens = r.Summary.promptTokens()
	result.Rankings.Models = r.Distribution.Model.Requests.Items
	names := make(map[int64]string)
	if health != nil {
		for _, group := range health.Groups {
			names[group.ID] = group.Name
		}
	}
	for _, group := range r.Distribution.Group.Requests.Items {
		row := groupRanking{rankingTotals: group.rankingTotals}
		row.Group.Name = names[group.ID]
		if row.Group.Name == "" {
			row.Group.Name = "分组 #" + strconv.FormatInt(group.ID, 10)
		}
		result.Rankings.Groups = append(result.Rankings.Groups, row)
	}
	if r.BucketWidth > 0 && span.to > span.from {
		result.Series = r.fillBuckets(span)
	}
	return result
}

func (r usageReport) fillBuckets(span usageSpan) []usageBucket {
	width := r.BucketWidth
	byStart := make(map[int64]usageBucket, len(r.Series))
	for _, bucket := range r.Series {
		byStart[bucket.Start/width*width] = bucket
	}
	start := span.from / width * width
	if (span.to-start)/width > 1000 {
		return r.Series
	}
	var result []usageBucket
	for ; start < span.to; start += width {
		bucket := byStart[start]
		bucket.Start = max(start, span.from)
		bucket.End = min(start+width, span.to)
		result = append(result, bucket)
	}
	return result
}

type accessKey struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Deleted bool   `json:"deleted,omitempty"`
}

func (r usageReport) includeHistoricalKeys(keys []accessKey) []accessKey {
	seen := make(map[int64]bool, len(keys))
	for _, key := range keys {
		seen[key.ID] = true
	}
	for _, distribution := range r.Distribution.AccessKey {
		for _, key := range distribution.Items {
			if key.ID > 0 && !seen[key.ID] {
				keys = append(keys, accessKey{ID: key.ID, Deleted: true})
				seen[key.ID] = true
			}
		}
	}
	return keys
}

type accessKeyRow struct {
	accessKey
	*rankingTotals
	CacheHitRate      *float64 `json:"cache_hit_rate,omitempty"`
	CostPerMillion    *float64 `json:"cost_per_million_tokens_usd,omitempty"`
	CostPerThousand   *float64 `json:"cost_per_thousand_requests_usd,omitempty"`
	TokensPerRequest  *float64 `json:"tokens_per_request,omitempty"`
	PricingIncomplete bool     `json:"pricing_incomplete,omitempty"`
	Error             string   `json:"error,omitempty"`
	costNano          int64
}

type accessKeyData struct {
	From  int64          `json:"from_ms"`
	Items []accessKeyRow `json:"items"`
}

func (u upstream) accessKeyUsage(ctx context.Context, span usageSpan, keys []accessKey) section[accessKeyData] {
	data := accessKeyData{From: span.from, Items: make([]accessKeyRow, 0, len(keys))}
	for _, key := range keys {
		if key.ID > 0 {
			data.Items = append(data.Items, accessKeyRow{accessKey: key})
		}
	}
	var group sync.WaitGroup
	for i := range data.Items {
		group.Go(func() {
			row := &data.Items[i]
			report, err := u.readUsage(ctx, span, url.Values{"access_key_id": {strconv.FormatInt(row.ID, 10)}})
			if err != nil {
				row.Error = "用量暂时无法更新"
				return
			}
			row.setMetrics(report.Summary)
		})
	}
	group.Wait()
	slices.SortFunc(data.Items, func(a, b accessKeyRow) int {
		if a.Error != "" && b.Error == "" {
			return 1
		}
		if a.Error == "" && b.Error != "" {
			return -1
		}
		if order := cmp.Compare(b.costNano, a.costNano); order != 0 {
			return order
		}
		return cmp.Compare(a.ID, b.ID)
	})
	if len(data.Items) > 0 && data.Items[0].Error != "" {
		return section[accessKeyData]{Error: "AccessKey 用量暂时无法更新。"}
	}
	return section[accessKeyData]{Data: &data, UpdatedAt: time.Now().UnixMilli()}
}

func (r *accessKeyRow) setMetrics(summary usageSummary) {
	r.rankingTotals = &summary.rankingTotals
	r.PricingIncomplete = summary.incomplete()
	r.CacheHitRate = divided(float64(summary.CacheRead), summary.promptTokens())
	r.TokensPerRequest = divided(float64(summary.Tokens), summary.Requests)
	if cost, err := strconv.ParseInt(summary.Cost, 10, 64); err == nil && cost >= 0 {
		r.costNano = cost
		r.CostPerMillion = divided(float64(cost)/1e3, summary.Tokens)
		r.CostPerThousand = divided(float64(cost)/1e6, summary.Requests)
	}
}

func divided(numerator float64, denominator int64) *float64 {
	if denominator <= 0 {
		return nil
	}
	value := numerator / float64(denominator)
	return &value
}
