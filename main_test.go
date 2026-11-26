package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDashboardAccessAndPartialFailure(t *testing.T) {
	const upstreamKey = "private-test-bearer-key"
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+upstreamKey {
			t.Errorf("unexpected upstream method or authorization: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		var data string
		switch r.URL.Path {
		case "/api/health":
			data = `{"version":"v2","groups":[{"id":1,"enabled":true,"counts":{"available":2,"model_cooldown":1}}]}`
		case "/api/home":
			data = `{"concurrency":{"current":3,"limit":16},"access_keys":[{"masked_key":"private-detail"}]}`
		case "/api/access-keys/options":
			data = `[]`
		case "/api/home/subscription-accounts":
			data = `{"items":[{"credential_key":"private-detail","credential":{"credential_id":5,"account":{"email":"private-detail"}}}]}`
		case "/api/usage":
			from, _ := strconv.ParseInt(r.URL.Query().Get("from_ms"), 10, 64)
			to, _ := strconv.ParseInt(r.URL.Query().Get("to_ms"), 10, 64)
			if to-from == (30 * 24 * time.Hour).Milliseconds() {
				http.Error(w, "private-detail", http.StatusServiceUnavailable)
				return
			}
			data = fmt.Sprintf(`{"bucket_width_ms":3600000,"summary":{"request_count":15,"estimated_cost_nano_usd":"434"},"series":[{"bucket_start_ms":%d,"request_count":15}]}`, from)
		default:
			t.Errorf("unexpected upstream URL: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"code":0,"data":%s}`, data)
	}))
	defer upstream.Close()

	handler := newHandler(config{
		baseURL: upstream.URL, upstreamKey: upstreamKey, password: "viewer",
		sessionKey: strings.Repeat("k", 32), secureCookie: true,
	}, func() time.Time { return time.Date(2026, 11, 23, 6, 54, 0, 0, time.UTC) })
	request := func(method, path, password string, cookie *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		values := url.Values{"password": {password}}
		r := httptest.NewRequest(method, path, strings.NewReader(values.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := request(http.MethodGet, "/api/dashboard?range=24h", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated dashboard: status %d", w.Code)
	}
	if w := request(http.MethodPost, "/login", "wrong", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("incorrect password: status %d", w.Code)
	}
	if calls := upstreamCalls.Load(); calls != 0 {
		t.Fatalf("upstream called before login: %d", calls)
	}
	w := request(http.MethodPost, "/login", "viewer", nil)
	cookies := w.Result().Cookies()
	if w.Code != http.StatusSeeOther || len(cookies) != 1 || !cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("login did not establish a secure session: status %d", w.Code)
	}
	cookie := cookies[0]
	if w := request(http.MethodGet, "/api/dashboard?range=day", "", cookie); w.Code != http.StatusBadRequest {
		t.Fatalf("invalid range: status %d", w.Code)
	}
	for _, tc := range []struct{ period, want string }{
		{"24h", `"request_count":15`},
		{"30d", "用量统计暂时无法更新"},
	} {
		w := request(http.MethodGet, "/api/dashboard?range="+tc.period, "", cookie)
		body := w.Body.String()
		if w.Code != http.StatusOK {
			t.Fatalf("range %s: status %d", tc.period, w.Code)
		}
		for _, want := range []string{tc.want, `"current":3`, `"credential_id":5`, `"available":2`, `"model_cooldown":1`, `"updated_at_ms":`} {
			if !strings.Contains(body, want) {
				t.Errorf("range %s response missing %q", tc.period, want)
			}
		}
		if strings.Contains(body, "private-detail") || strings.Contains(body, upstreamKey) {
			t.Errorf("range %s exposed private data", tc.period)
		}
		if tc.period == "24h" {
			var result dashboardResponse
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			data := result.Usage.Data
			if data == nil || len(data.Series) == 0 || data.Series[0].Requests != 15 || data.Series[0].Start != data.From {
				t.Error("the trend lost the requests from the clipped first bucket")
			}
		}
	}
}

func TestDashboardQuotaAndAccessKeyWindow(t *testing.T) {
	// Monday 14:54 in Shanghai, 5 hours 54 minutes into a weekly window.
	now := time.Date(2026, 11, 23, 6, 54, 0, 0, time.UTC)
	start := now.Add(-5*time.Hour - 54*time.Minute)
	reset := start.Add(7 * 24 * time.Hour)
	yesterday := time.Date(2026, 11, 21, 16, 0, 0, 0, time.UTC)
	today := yesterday.Add(24 * time.Hour)
	var firstCredentialCalls, secondCredentialCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var data string
		switch r.URL.Path {
		case "/api/health":
			data = `{"groups":[{"id":1},{"id":2}]}`
		case "/api/home":
			data = `{"concurrency":{"current":3}}`
		case "/api/access-keys/options":
			data = `[{"id":3,"name":"Idle","status":"disabled"},{"id":2,"name":"Active"}]`
		case "/api/home/subscription-accounts":
			data = fmt.Sprintf(`{"observed_at_ms":%d,"items":[{"group_count":2,"credential_key":"test-identity","credential":{"credential_id":5,"observation":{"observed_at_ms":%d,"snapshot":{"quota_windows":[{"unit":"percent","utilization":0.05,"window_seconds":604800,"reset_at_ms":%d}]}}}}]}`, now.UnixMilli(), now.UnixMilli(), reset.UnixMilli())
		case "/api/modern/groups/1/credentials":
			data = `{"items":[{"credential_id":5},{"credential_id":5}]}`
		case "/api/modern/groups/2/credentials":
			data = `{"items":[{"credential_id":6}]}`
		case "/api/usage":
			q := r.URL.Query()
			wantFrom, wantTo := yesterday, today
			switch q.Get("credential_id") {
			case "5":
				firstCredentialCalls.Add(1)
				data = `{"summary":{"request_count":1,"estimated_cost_nano_usd":"23000000000"}}`
			case "6":
				secondCredentialCalls.Add(1)
				data = `{"summary":{"request_count":2,"estimated_cost_nano_usd":"25010000000","pricing_partial_count":1}}`
			default:
				data = `{"summary":{"estimated_cost_nano_usd":"0"}}`
				switch q.Get("access_key_id") {
				case "2":
					data = fmt.Sprintf(`{
						"bucket_width_ms":3600000,
						"summary":{"request_count":4,"total_tokens":2000,"uncached_input_tokens":50,"cache_read_tokens":60,
							"cache_write_5m_tokens":5,"cache_write_1h_tokens":3,"cache_write_unknown_tokens":2,
							"estimated_cost_nano_usd":"2400000000","unpriced_request_count":1},
						"series":[
							{"bucket_start_ms":%d,"uncached_input_tokens":25,"cache_read_tokens":60,
								"cache_write_5m_tokens":5,"cache_write_1h_tokens":3,"cache_write_unknown_tokens":2},
							{"bucket_start_ms":%d,"uncached_input_tokens":25}
						]
					}`, yesterday.UnixMilli(), yesterday.Add(2*time.Hour).UnixMilli())
				case "4":
					data = fmt.Sprintf(`{"bucket_width_ms":3600000,"summary":{"request_count":1,"cache_read_tokens":100,"estimated_cost_nano_usd":"1000000000"},"series":[{"bucket_start_ms":%d,"cache_read_tokens":100}]}`, yesterday.Add(time.Hour).UnixMilli())
				case "":
					data = `{"summary":{"estimated_cost_nano_usd":"0"},"distributions":{"access_key":{"requests":{"items":[{"access_key_id":4}]}}}}`
				}
			}
			if q.Has("credential_id") {
				wantFrom, wantTo = start, now
			}
			if q.Get("from_ms") != strconv.FormatInt(wantFrom.UnixMilli(), 10) || q.Get("to_ms") != strconv.FormatInt(wantTo.UnixMilli(), 10) {
				t.Errorf("unexpected usage window: %s", r.URL)
			}
		default:
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"code":0,"data":%s}`, data)
	}))
	defer upstream.Close()

	handler := newHandler(config{baseURL: upstream.URL, upstreamKey: "test", password: "viewer", sessionKey: strings.Repeat("k", 32)}, func() time.Time { return now })
	login := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("password=viewer"))
	login.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, login)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("login failed: %d", w.Code)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/dashboard?range=yesterday", nil)
	r.AddCookie(w.Result().Cookies()[0])
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("dashboard yesterday: status %d", w.Code)
	}
	for _, want := range []string{`"used_amount_usd":48.01`, `"estimated_limit_usd":960.2`, `"safe_level_percent":11.8`, `"safe_amount_usd":65.29`, `"range":"yesterday"`} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("dashboard missing %s", want)
		}
	}
	if calls := firstCredentialCalls.Load(); calls != 1 || secondCredentialCalls.Load() != 1 {
		t.Errorf("expected each credential once, got %d and %d", calls, secondCredentialCalls.Load())
	}
	var response struct {
		Quota      section[quotaData] `json:"quota"`
		AccessKeys struct {
			Data struct {
				Items []map[string]any `json:"items"`
			} `json:"data"`
		} `json:"access_keys"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Quota.Data == nil || !response.Quota.Data.Items[0].Credential.Observation.Snapshot.Windows[0].Incomplete {
		t.Error("partially priced quota should be marked incomplete")
	}
	items := response.AccessKeys.Data.Items
	if len(items) != 3 || items[0]["name"] != "Active" || items[1]["id"] != float64(4) || items[1]["deleted"] != true || items[2]["name"] != "Idle" {
		t.Fatalf("unexpected AccessKey rows: %v", items)
	}
	for field, want := range map[string]any{
		"request_count": float64(4), "total_tokens": float64(2000), "estimated_cost_nano_usd": "2400000000",
		"cache_hit_rate": 0.5, "cost_per_million_tokens_usd": float64(1200),
		"cost_per_thousand_requests_usd": float64(600), "tokens_per_request": float64(500), "pricing_incomplete": true,
	} {
		if items[0][field] != want {
			t.Errorf("AccessKey %s: got %v, want %v", field, items[0][field], want)
		}
		if strings.Contains(field, "_per_") || field == "cache_hit_rate" {
			if items[2][field] != nil {
				t.Errorf("idle AccessKey %s should be unavailable", field)
			}
		}
	}

	for _, tc := range []struct {
		index int
		rates []any
	}{
		{0, []any{60.0 / 95.0, nil, float64(0)}},
		{1, []any{nil, float64(1), nil}},
	} {
		points, ok := items[tc.index]["cache_hit_series"].([]any)
		if !ok || len(points) != 24 {
			t.Fatalf("AccessKey %v has no aligned cache hit series", items[tc.index]["id"])
		}
		for hour, want := range tc.rates {
			point := points[hour].(map[string]any)
			start := yesterday.Add(time.Duration(hour) * time.Hour)
			rate, exists := point["cache_hit_rate"]
			if !exists || rate != want || point["bucket_start_ms"] != float64(start.UnixMilli()) ||
				point["bucket_end_ms"] != float64(start.Add(time.Hour).UnixMilli()) {
				t.Errorf("AccessKey %v hour %d: got %v, want rate %v", items[tc.index]["id"], hour, point, want)
			}
		}
	}
	if _, exists := items[2]["cache_hit_series"]; exists {
		t.Error("idle AccessKey should have no cache series")
	}
}
