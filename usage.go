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
	case "6h":
		from = now.Add(-6 * time.Hour)
	case "12h":
		from = now.Add(-12 * time.Hour)
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
	cacheTokens
	Success        int64 `json:"success_count"`
	Failure        int64 `json:"failure_count"`
	Unpriced       int64 `json:"unpriced_request_count"`
	PartialPricing int64 `json:"pricing_partial_count"`
	MissingUsage   int64 `json:"usage_missing_count"`
}

type cacheTokens struct {
	UncachedInput  int64 `json:"uncached_input_tokens"`
	CacheRead      int64 `json:"cache_read_tokens"`
	CacheWrite5m   int64 `json:"cache_write_5m_tokens"`
	CacheWrite1h   int64 `json:"cache_write_1h_tokens"`
	CacheWriteElse int64 `json:"cache_write_unknown_tokens"`
}

func (s cacheTokens) promptTokens() int64 {
	return s.UncachedInput + s.CacheRead + s.CacheWrite5m + s.CacheWrite1h + s.CacheWriteElse
}

func (s cacheTokens) cacheHitRate() *float64 {
	return divided(float64(s.CacheRead), s.promptTokens())
}

func (s usageSummary) incomplete() bool {
	return s.Unpriced > 0 || s.PartialPricing > 0 || s.MissingUsage > 0
}

type usageReportBucket struct {
	usageBucket
	cacheTokens
	Tokens         *int64 `json:"total_tokens"`
	Cost           string `json:"estimated_cost_nano_usd"`
	Unpriced       int64  `json:"unpriced_request_count"`
	PartialPricing int64  `json:"pricing_partial_count"`
	MissingUsage   int64  `json:"usage_missing_count"`
	PartialUsage   int64  `json:"partial_count"`
}

type usageReport struct {
	Granularity  string              `json:"granularity"`
	BucketWidth  int64               `json:"bucket_width_ms"`
	ObservedAt   int64               `json:"observed_at_ms"`
	Summary      usageSummary        `json:"summary"`
	Series       []usageReportBucket `json:"series"`
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
		ObservedAt: r.ObservedAt,
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
	for _, bucket := range r.fillBuckets(span) {
		result.Series = append(result.Series, bucket.usageBucket)
	}
	return result
}

func (r usageReport) fillBuckets(span usageSpan) []usageReportBucket {
	width := r.BucketWidth
	if width <= 0 || span.to <= span.from {
		return r.Series
	}
	start := span.from / width * width
	if (span.to-start)/width > 1000 {
		return r.Series
	}
	byStart := make(map[int64]usageReportBucket, len(r.Series))
	for _, bucket := range r.Series {
		byStart[bucket.Start/width*width] = bucket
	}
	var result []usageReportBucket
	for ; start < span.to; start += width {
		bucket := byStart[start]
		bucket.Start = max(start, span.from)
		bucket.End = min(start+width, span.to)
		result = append(result, bucket)
	}
	return result
}

type accessKeyUsagePoint struct {
	Start             int64    `json:"bucket_start_ms"`
	End               int64    `json:"bucket_end_ms"`
	Requests          int64    `json:"request_count"`
	Tokens            *int64   `json:"total_tokens"`
	CacheHitRate      *float64 `json:"cache_hit_rate"`
	Cost              string   `json:"estimated_cost_nano_usd,omitempty"`
	PricingIncomplete bool     `json:"pricing_incomplete,omitempty"`
}

func (r usageReport) accessKeySeries(span usageSpan) []accessKeyUsagePoint {
	if len(r.Series) == 0 {
		return nil
	}
	buckets := r.fillBuckets(span)
	points := make([]accessKeyUsagePoint, 0, len(buckets))
	for _, bucket := range buckets {
		point := accessKeyUsagePoint{
			Start: bucket.Start, End: bucket.End, Requests: bucket.Requests,
			Tokens: bucket.Tokens, CacheHitRate: bucket.cacheHitRate(), Cost: bucket.Cost,
			PricingIncomplete: bucket.Unpriced > 0 || bucket.PartialPricing > 0,
		}
		tokenIncomplete := bucket.MissingUsage > 0 || bucket.PartialUsage > 0
		if tokenIncomplete && point.Tokens != nil && *point.Tokens == 0 {
			point.Tokens = nil
		} else if !tokenIncomplete && point.Tokens == nil && point.Requests == 0 {
			point.Tokens = new(int64)
		}
		if point.PricingIncomplete && point.Cost == "0" {
			point.Cost = ""
		} else if !point.PricingIncomplete && point.Cost == "" && point.Requests == 0 {
			point.Cost = "0"
		}
		points = append(points, point)
	}
	return points
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
	CacheHitRate      *float64              `json:"cache_hit_rate,omitempty"`
	Series            []accessKeyUsagePoint `json:"series,omitempty"`
	CostPerMillion    *float64              `json:"cost_per_million_tokens_usd,omitempty"`
	CostPerThousand   *float64              `json:"cost_per_thousand_requests_usd,omitempty"`
	TokensPerRequest  *float64              `json:"tokens_per_request,omitempty"`
	PricingIncomplete bool                  `json:"pricing_incomplete,omitempty"`
	Error             string                `json:"error,omitempty"`
	costNano          int64
}

type accessKeyData struct {
	From  int64          `json:"from_ms"`
	To    int64          `json:"to_ms"`
	Items []accessKeyRow `json:"items"`
}

func (u upstream) accessKeyUsage(ctx context.Context, span usageSpan, keys []accessKey) section[accessKeyData] {
	data := accessKeyData{From: span.from, To: span.to, Items: make([]accessKeyRow, 0, len(keys))}
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
			row.Series = report.accessKeySeries(span)
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
	r.CacheHitRate = summary.cacheHitRate()
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
