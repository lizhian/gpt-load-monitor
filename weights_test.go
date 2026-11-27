package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLoadConfigAutoWeight(t *testing.T) {
	setBaseEnv := func(t *testing.T) {
		t.Helper()
		t.Setenv("GPT_LOAD_BASE_URL", "https://gpt-load.example.com")
		t.Setenv("GPT_LOAD_AUTH_KEY", "admin-key")
		t.Setenv("VIEWER_PASSWORD", "viewer-pass")
		t.Setenv("SESSION_SECRET", strings.Repeat("s", 32))
		t.Setenv("COOKIE_SECURE", "")
	}

	t.Run("defaults", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("AUTO_WEIGHT_ENABLED", "")
		t.Setenv("AUTO_WEIGHT_INTERVAL", "")

		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig failed: %v", err)
		}
		if !cfg.autoWeightEnabled {
			t.Errorf("autoWeightEnabled = false, want true")
		}
		if cfg.autoWeightInterval != 30*time.Minute {
			t.Errorf("autoWeightInterval = %v, want 30m", cfg.autoWeightInterval)
		}
	})

	t.Run("explicit overrides", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("AUTO_WEIGHT_ENABLED", "false")
		t.Setenv("AUTO_WEIGHT_INTERVAL", "15m")

		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("loadConfig failed: %v", err)
		}
		if cfg.autoWeightEnabled {
			t.Errorf("autoWeightEnabled = true, want false")
		}
		if cfg.autoWeightInterval != 15*time.Minute {
			t.Errorf("autoWeightInterval = %v, want 15m", cfg.autoWeightInterval)
		}
	})

	for _, tc := range []struct {
		name     string
		enabled  string
		interval string
	}{
		{name: "invalid enabled", enabled: "maybe"},
		{name: "zero interval", interval: "0s"},
		{name: "negative interval", interval: "-5m"},
		{name: "invalid interval", interval: "bad"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setBaseEnv(t)
			t.Setenv("AUTO_WEIGHT_ENABLED", tc.enabled)
			t.Setenv("AUTO_WEIGHT_INTERVAL", tc.interval)

			if _, err := loadConfig(); err == nil {
				t.Fatalf("expected error for enabled=%q interval=%q", tc.enabled, tc.interval)
			}
		})
	}
}

func TestWeightAdjusterProportionalAndIdempotent(t *testing.T) {
	// Monday 14:54 in Shanghai, 5h54m into a 7d window -> safe_level_percent = 11.8%.
	now := time.Date(2026, 11, 23, 6, 54, 0, 0, time.UTC)
	start := now.Add(-5*time.Hour - 54*time.Minute)
	reset := start.Add(7 * 24 * time.Hour)

	var mu sync.Mutex
	weights := map[int64]int{
		101: 50,
		102: 50,
		201: 50,
		202: 30,
		203: 50,
	}
	type putCall struct {
		groupID      string
		credentialID string
		weightManual int
	}
	var puts []putCall

	// Group 1 (Codex):
	//   cred 101: util=0.20, cost=$20 -> est=$100, safe_level=11.8% -> ahead ~$8.20 -> target 5
	//   cred 102: util=0.05, cost=$10 -> est=$200, safe_level=11.8% -> safe $13.60 -> target 100
	// Group 2 (Claude):
	//   cred 201: util=0.05, cost=$100 -> est=$2000, safe = (0.118 - 0.05)*2000 = $136 -> target 100
	//   cred 202: util=0.05, cost=$50  -> est=$1000, safe = (0.118 - 0.05)*1000 = $68  -> target 50
	//   cred 203: util=0.05, cost=$2.5 -> est=$50,   safe = (0.118 - 0.05)*50   = $3.4 -> round(3.4/136*100)=3 -> min 5
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/modern/groups":
			fmt.Fprintf(w, `{"code":0,"data":{"observed_at_ms":%d,"items":[
				{"id":1,"name":"Codex","channel_id":"codex","channel_name":"Codex","connection_type":"subscription","enabled":true,"weight":50},
				{"id":2,"name":"Claude","channel_id":"claude","channel_name":"Claude","connection_type":"subscription","enabled":true,"weight":50}
			]}}`, now.UnixMilli())
		case r.Method == http.MethodGet && r.URL.Path == "/api/modern/groups/1/credentials":
			fmt.Fprintf(w, `{"code":0,"data":{"observed_at_ms":%d,"pagination":{"page":1,"page_size":100,"total_items":2,"total_pages":1},"items":[
				{"credential_id":101,"name":"a1","account":{"email":"a1@example.com","email_mask":"a***1@example.com"},"configured_status":"active","effective_status":"available","weight":%d,"weight_manual":%d,"observation":{"state":"fresh","observed_at_ms":%d,"snapshot":{"quota_windows":[{"id":"7d","unit":"percent","scope":"account","utilization":0.20,"window_seconds":604800,"reset_at_ms":%d}]}}},
				{"credential_id":102,"name":"a2","account":{"email":"a2@example.com","email_mask":"a***2@example.com"},"configured_status":"active","effective_status":"available","weight":%d,"weight_manual":%d,"observation":{"state":"fresh","observed_at_ms":%d,"snapshot":{"quota_windows":[{"id":"7d","unit":"percent","scope":"account","utilization":0.05,"window_seconds":604800,"reset_at_ms":%d}]}}}
			]}}`, now.UnixMilli(), weights[101], weights[101], now.UnixMilli(), reset.UnixMilli(), weights[102], weights[102], now.UnixMilli(), reset.UnixMilli())
		case r.Method == http.MethodGet && r.URL.Path == "/api/modern/groups/2/credentials":
			fmt.Fprintf(w, `{"code":0,"data":{"observed_at_ms":%d,"pagination":{"page":1,"page_size":100,"total_items":3,"total_pages":1},"items":[
				{"credential_id":201,"name":"b1","account":{"email":"b1@example.com","email_mask":"b***1@example.com"},"configured_status":"active","effective_status":"available","weight":%d,"weight_manual":%d,"observation":{"state":"fresh","observed_at_ms":%d,"snapshot":{"quota_windows":[{"id":"7d","unit":"percent","scope":"account","utilization":0.05,"window_seconds":604800,"reset_at_ms":%d}]}}},
				{"credential_id":202,"name":"b2","account":{"email":"b2@example.com","email_mask":"b***2@example.com"},"configured_status":"active","effective_status":"available","weight":%d,"weight_manual":%d,"observation":{"state":"fresh","observed_at_ms":%d,"snapshot":{"quota_windows":[{"id":"7d","unit":"percent","scope":"account","utilization":0.05,"window_seconds":604800,"reset_at_ms":%d}]}}},
				{"credential_id":203,"name":"b3","account":{"email":"b3@example.com","email_mask":"b***3@example.com"},"configured_status":"active","effective_status":"available","weight":%d,"weight_manual":%d,"observation":{"state":"fresh","observed_at_ms":%d,"snapshot":{"quota_windows":[{"id":"7d","unit":"percent","scope":"account","utilization":0.05,"window_seconds":604800,"reset_at_ms":%d}]}}}
			]}}`, now.UnixMilli(), weights[201], weights[201], now.UnixMilli(), reset.UnixMilli(), weights[202], weights[202], now.UnixMilli(), reset.UnixMilli(), weights[203], weights[203], now.UnixMilli(), reset.UnixMilli())
		case r.Method == http.MethodGet && r.URL.Path == "/api/usage":
			costByCred := map[string]string{
				"101": "20000000000",
				"102": "10000000000",
				"201": "100000000000",
				"202": "50000000000",
				"203": "2500000000",
			}
			cost := costByCred[r.URL.Query().Get("credential_id")]
			fmt.Fprintf(w, `{"code":0,"data":{"summary":{"request_count":1,"estimated_cost_nano_usd":%q}}}`, cost)
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/groups/"):
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/groups/"), "/")
			if len(parts) != 3 || parts[1] != "credentials" {
				http.NotFound(w, r)
				return
			}
			var body struct {
				WeightManual int `json:"weight_manual"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "bad json", http.StatusBadRequest)
				return
			}
			credID, _ := strconv.ParseInt(parts[2], 10, 64)
			weights[credID] = body.WeightManual
			puts = append(puts, putCall{groupID: parts[0], credentialID: parts[2], weightManual: body.WeightManual})
			fmt.Fprintf(w, `{"code":0,"data":{"credential_id":%d,"weight":%d}}`, credID, body.WeightManual)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	up := upstream{
		baseURL: srv.URL,
		key:     "admin-key",
		slots:   make(chan struct{}, 8),
		client:  srv.Client(),
	}
	adjuster := newWeightAdjuster(up, true, 30*time.Minute, func() time.Time { return now })

	if err := adjuster.RunOnce(context.Background()); err != nil {
		t.Fatalf("first RunOnce failed: %v", err)
	}

	mu.Lock()
	gotWeights := map[int64]int{
		101: weights[101],
		102: weights[102],
		201: weights[201],
		202: weights[202],
		203: weights[203],
	}
	firstPuts := len(puts)
	mu.Unlock()

	want := map[int64]int{
		101: 5,
		102: 100,
		201: 100,
		202: 50,
		203: 5,
	}
	for id, wantW := range want {
		if gotWeights[id] != wantW {
			t.Errorf("credential %d weight = %d, want %d", id, gotWeights[id], wantW)
		}
	}
	if firstPuts != len(want) {
		t.Errorf("first run PUT count = %d, want %d", firstPuts, len(want))
	}

	status := adjuster.Status()
	if !status.Enabled || status.LastRunAt != now.UnixMilli() || status.Error != "" {
		t.Errorf("unexpected adjuster status: %+v", status)
	}

	// Second run with unchanged upstream weights should perform zero PUTs.
	mu.Lock()
	puts = nil
	mu.Unlock()

	if err := adjuster.RunOnce(context.Background()); err != nil {
		t.Fatalf("second RunOnce failed: %v", err)
	}
	mu.Lock()
	secondPuts := len(puts)
	mu.Unlock()
	if secondPuts != 0 {
		t.Errorf("second run PUT count = %d, want 0", secondPuts)
	}
}

func TestWeightAdjusterGroupIsolationAndPagination(t *testing.T) {
	now := time.Date(2026, 11, 23, 6, 54, 0, 0, time.UTC)
	start := now.Add(-5*time.Hour - 54*time.Minute)
	reset := start.Add(7 * 24 * time.Hour)

	var mu sync.Mutex
	weights := map[int64]int{11: 50, 12: 50, 21: 50, 22: 50}
	var seenPages []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/modern/groups":
			fmt.Fprintf(w, `{"code":0,"data":{"observed_at_ms":%d,"items":[
				{"id":1,"name":"Codex A","channel_id":"codex","channel_name":"Codex","connection_type":"subscription","enabled":true},
				{"id":2,"name":"Codex B","channel_id":"codex","channel_name":"Codex","connection_type":"subscription","enabled":true},
				{"id":99,"name":"OpenAI Key","channel_id":"openai","channel_name":"OpenAI","connection_type":"api_key","enabled":true}
			]}}`, now.UnixMilli())
		case r.Method == http.MethodGet && r.URL.Path == "/api/modern/groups/1/credentials":
			page := r.URL.Query().Get("page")
			seenPages = append(seenPages, "1:"+page)
			if page == "1" {
				fmt.Fprintf(w, `{"code":0,"data":{"observed_at_ms":%d,"pagination":{"page":1,"page_size":100,"total_items":2,"total_pages":2},"items":[
					{"credential_id":11,"account":{"email":"p1@example.com"},"configured_status":"active","effective_status":"available","weight":%d,"observation":{"state":"fresh","observed_at_ms":%d,"snapshot":{"quota_windows":[{"id":"7d","unit":"percent","scope":"account","utilization":0.05,"window_seconds":604800,"reset_at_ms":%d}]}}}
				]}}`, now.UnixMilli(), weights[11], now.UnixMilli(), reset.UnixMilli())
				return
			}
			fmt.Fprintf(w, `{"code":0,"data":{"observed_at_ms":%d,"pagination":{"page":2,"page_size":100,"total_items":2,"total_pages":2},"items":[
				{"credential_id":12,"account":{"email":"p2@example.com"},"configured_status":"active","effective_status":"available","weight":%d,"observation":{"state":"fresh","observed_at_ms":%d,"snapshot":{"quota_windows":[{"id":"7d","unit":"percent","scope":"account","utilization":0.05,"window_seconds":604800,"reset_at_ms":%d}]}}}
			]}}`, now.UnixMilli(), weights[12], now.UnixMilli(), reset.UnixMilli())
		case r.Method == http.MethodGet && r.URL.Path == "/api/modern/groups/2/credentials":
			seenPages = append(seenPages, "2:"+r.URL.Query().Get("page"))
			fmt.Fprintf(w, `{"code":0,"data":{"observed_at_ms":%d,"pagination":{"page":1,"page_size":100,"total_items":2,"total_pages":1},"items":[
				{"credential_id":21,"account":{"email":"q1@example.com"},"configured_status":"active","effective_status":"available","weight":%d,"observation":{"state":"fresh","observed_at_ms":%d,"snapshot":{"quota_windows":[{"id":"7d","unit":"percent","scope":"account","utilization":0.05,"window_seconds":604800,"reset_at_ms":%d}]}}},
				{"credential_id":22,"account":{"email":"q2@example.com"},"configured_status":"active","effective_status":"available","weight":%d,"observation":{"state":"fresh","observed_at_ms":%d,"snapshot":{"quota_windows":[{"id":"7d","unit":"percent","scope":"account","utilization":0.05,"window_seconds":604800,"reset_at_ms":%d}]}}}
			]}}`, now.UnixMilli(), weights[21], now.UnixMilli(), reset.UnixMilli(), weights[22], now.UnixMilli(), reset.UnixMilli())
		case r.Method == http.MethodGet && r.URL.Path == "/api/usage":
			// Group 1: cred 11 cost $20 (safe $27.20), cred 12 cost $10 (safe $13.60) -> 100, 50
			// Group 2: cred 21 cost $100 (safe $136), cred 22 cost $20 (safe $27.20) -> 100, 20
			cost := map[string]string{
				"11": "20000000000",
				"12": "10000000000",
				"21": "100000000000",
				"22": "20000000000",
			}[r.URL.Query().Get("credential_id")]
			fmt.Fprintf(w, `{"code":0,"data":{"summary":{"request_count":1,"estimated_cost_nano_usd":%q}}}`, cost)
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/groups/"):
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/groups/"), "/")
			var body struct {
				WeightManual int `json:"weight_manual"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			id, _ := strconv.ParseInt(parts[2], 10, 64)
			weights[id] = body.WeightManual
			fmt.Fprintf(w, `{"code":0,"data":{"credential_id":%d}}`, id)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	adjuster := newWeightAdjuster(upstream{baseURL: srv.URL, key: "k", slots: make(chan struct{}, 8), client: srv.Client()}, true, 30*time.Minute, func() time.Time { return now })
	if err := adjuster.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if weights[11] != 100 || weights[12] != 50 {
		t.Errorf("group 1 weights = (%d, %d), want (100, 50)", weights[11], weights[12])
	}
	if weights[21] != 100 || weights[22] != 20 {
		t.Errorf("group 2 weights = (%d, %d), want (100, 20)", weights[21], weights[22])
	}
	if !strings.Contains(strings.Join(seenPages, ","), "1:2") {
		t.Errorf("expected page 2 to be fetched for group 1, got %v", seenPages)
	}
}

func TestWeightAdjusterCrossGroupAccountAggregation(t *testing.T) {
	now := time.Date(2026, 11, 23, 6, 54, 0, 0, time.UTC)
	start := now.Add(-5*time.Hour - 54*time.Minute)
	reset := start.Add(7 * 24 * time.Hour)

	var mu sync.Mutex
	weights := map[int64]int{11: 50, 12: 50, 21: 50, 22: 50, 31: 50}
	var updatedIDs []int64

	// Shared account across enabled groups 1 & 2 and disabled group 3:
	//   cred 11 (account.email="Shared@Example.COM"), cred 21 (account_summary.email="shared@example.com"), cred 31 (disabled group)
	//   Spend: 11=$5, 21=$5, 31=$10 -> total=$20, util=0.05 -> est=$400 -> safe=$27.20
	//   Group 1 peer cred 12 (no email, same mask as 13): cost=$40, util=0.05 -> safe=$54.40 -> 11=50, 12=100
	//   Group 2 peer cred 22 (no email, same mask as 12): cost=$10, util=0.05 -> safe=$13.60 -> 21=100, 22=50
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/modern/groups":
			fmt.Fprintf(w, `{"code":0,"data":{"observed_at_ms":%d,"items":[
				{"id":1,"name":"Codex 1","channel_id":"codex","connection_type":"subscription","enabled":true},
				{"id":2,"name":"Codex 2","channel_id":"codex","connection_type":"subscription","enabled":true},
				{"id":3,"name":"Codex Disabled","channel_id":"codex","connection_type":"subscription","enabled":false}
			]}}`, now.UnixMilli())
		case r.Method == http.MethodGet && r.URL.Path == "/api/modern/groups/1/credentials":
			fmt.Fprintf(w, `{"code":0,"data":{"observed_at_ms":%d,"pagination":{"page":1,"page_size":100,"total_items":2,"total_pages":1},"items":[
				{"credential_id":11,"account":{"email":" Shared@Example.COM ","email_mask":"s***d@example.com"},"configured_status":"active","effective_status":"available","weight":%d,"observation":{"state":"fresh","observed_at_ms":%d,"snapshot":{"quota_windows":[{"id":"7d","unit":"percent","scope":"account","utilization":0.05,"window_seconds":604800,"reset_at_ms":%d}]}}},
				{"credential_id":12,"account":{"email":"","email_mask":"same***@example.com"},"configured_status":"active","effective_status":"available","weight":%d,"observation":{"state":"fresh","observed_at_ms":%d,"snapshot":{"quota_windows":[{"id":"7d","unit":"percent","scope":"account","utilization":0.05,"window_seconds":604800,"reset_at_ms":%d}]}}}
			]}}`, now.UnixMilli(), weights[11], now.UnixMilli(), reset.UnixMilli(), weights[12], now.UnixMilli(), reset.UnixMilli())
		case r.Method == http.MethodGet && r.URL.Path == "/api/modern/groups/2/credentials":
			fmt.Fprintf(w, `{"code":0,"data":{"observed_at_ms":%d,"pagination":{"page":1,"page_size":100,"total_items":2,"total_pages":1},"items":[
				{"credential_id":21,"account":{"email_mask":"s***d@example.com"},"configured_status":"active","effective_status":"available","weight":%d,"observation":{"state":"fresh","observed_at_ms":%d,"snapshot":{"account_summary":{"email":"shared@example.com"},"quota_windows":[{"id":"7d","unit":"percent","scope":"account","utilization":0.05,"window_seconds":604800,"reset_at_ms":%d}]}}},
				{"credential_id":22,"account":{"email":"","email_mask":"same***@example.com"},"configured_status":"active","effective_status":"available","weight":%d,"observation":{"state":"fresh","observed_at_ms":%d,"snapshot":{"quota_windows":[{"id":"7d","unit":"percent","scope":"account","utilization":0.05,"window_seconds":604800,"reset_at_ms":%d}]}}}
			]}}`, now.UnixMilli(), weights[21], now.UnixMilli()+1000, reset.UnixMilli(), weights[22], now.UnixMilli(), reset.UnixMilli())
		case r.Method == http.MethodGet && r.URL.Path == "/api/modern/groups/3/credentials":
			fmt.Fprintf(w, `{"code":0,"data":{"observed_at_ms":%d,"pagination":{"page":1,"page_size":100,"total_items":1,"total_pages":1},"items":[
				{"credential_id":31,"account":{"email":"shared@example.com"},"configured_status":"active","effective_status":"disabled","weight":%d,"observation":{"state":"fresh","observed_at_ms":%d,"snapshot":{"quota_windows":[{"id":"7d","unit":"percent","scope":"account","utilization":0.05,"window_seconds":604800,"reset_at_ms":%d}]}}}
			]}}`, now.UnixMilli(), weights[31], now.UnixMilli(), reset.UnixMilli())
		case r.Method == http.MethodGet && r.URL.Path == "/api/usage":
			cost := map[string]string{
				"11": "5000000000",
				"21": "5000000000",
				"31": "10000000000",
				"12": "40000000000",
				"22": "10000000000",
			}[r.URL.Query().Get("credential_id")]
			fmt.Fprintf(w, `{"code":0,"data":{"summary":{"request_count":1,"estimated_cost_nano_usd":%q}}}`, cost)
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/groups/"):
			parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/groups/"), "/")
			var body struct {
				WeightManual int `json:"weight_manual"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			id, _ := strconv.ParseInt(parts[2], 10, 64)
			weights[id] = body.WeightManual
			updatedIDs = append(updatedIDs, id)
			fmt.Fprintf(w, `{"code":0,"data":{"credential_id":%d}}`, id)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	adjuster := newWeightAdjuster(upstream{baseURL: srv.URL, key: "k", slots: make(chan struct{}, 8), client: srv.Client()}, true, 30*time.Minute, func() time.Time { return now })
	if err := adjuster.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if weights[11] != 50 || weights[12] != 100 {
		t.Errorf("group 1 weights = (%d, %d), want (50, 100)", weights[11], weights[12])
	}
	if weights[21] != 100 || weights[22] != 50 {
		t.Errorf("group 2 weights = (%d, %d), want (100, 50)", weights[21], weights[22])
	}
	if weights[31] != 50 {
		t.Errorf("disabled group cred 31 should not be updated, got %d", weights[31])
	}
	for _, id := range updatedIDs {
		if id == 31 {
			t.Fatal("disabled group credential 31 received PUT")
		}
	}
}

func TestWeightAdjusterBoundaryCases(t *testing.T) {
	now := time.Date(2026, 11, 23, 6, 54, 0, 0, time.UTC)
	start7d := now.Add(-5*time.Hour - 54*time.Minute)
	reset7d := start7d.Add(7 * 24 * time.Hour)
	start5h := now.Add(-1 * time.Hour)
	reset5h := start5h.Add(5 * time.Hour)

	type credSpec struct {
		id        int64
		email     string
		cfgStatus string
		effStatus string
		weight    int
		costNano  string
		partial   int
		windows   string
	}

	runScenario := func(t *testing.T, creds []credSpec) map[int64]int {
		t.Helper()
		var mu sync.Mutex
		weights := make(map[int64]int, len(creds))
		for _, c := range creds {
			weights[c.id] = c.weight
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/api/modern/groups":
				fmt.Fprintf(w, `{"code":0,"data":{"observed_at_ms":%d,"items":[{"id":1,"name":"Codex","channel_id":"codex","connection_type":"subscription","enabled":true}]}}`, now.UnixMilli())
			case r.Method == http.MethodGet && r.URL.Path == "/api/modern/groups/1/credentials":
				items := make([]string, 0, len(creds))
				for _, c := range creds {
					items = append(items, fmt.Sprintf(
						`{"credential_id":%d,"account":{"email":%q},"configured_status":%q,"effective_status":%q,"weight":%d,"observation":{"state":"fresh","observed_at_ms":%d,"snapshot":{"quota_windows":%s}}}`,
						c.id, c.email, c.cfgStatus, c.effStatus, weights[c.id], now.UnixMilli(), c.windows,
					))
				}
				fmt.Fprintf(w, `{"code":0,"data":{"observed_at_ms":%d,"pagination":{"page":1,"page_size":100,"total_items":%d,"total_pages":1},"items":[%s]}}`, now.UnixMilli(), len(items), strings.Join(items, ","))
			case r.Method == http.MethodGet && r.URL.Path == "/api/usage":
				id, _ := strconv.ParseInt(r.URL.Query().Get("credential_id"), 10, 64)
				for _, c := range creds {
					if c.id == id {
						if c.costNano == "0" {
							fmt.Fprintf(w, `{"code":0,"data":{"summary":{"request_count":0,"estimated_cost_nano_usd":"0"}}}`)
						} else {
							fmt.Fprintf(w, `{"code":0,"data":{"summary":{"request_count":1,"pricing_partial_count":%d,"estimated_cost_nano_usd":%q}}}`, c.partial, c.costNano)
						}
						return
					}
				}
				http.NotFound(w, r)
			case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/groups/1/credentials/"):
				idStr := strings.TrimPrefix(r.URL.Path, "/api/groups/1/credentials/")
				id, _ := strconv.ParseInt(idStr, 10, 64)
				var body struct {
					WeightManual int `json:"weight_manual"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				weights[id] = body.WeightManual
				fmt.Fprintf(w, `{"code":0,"data":{"credential_id":%d}}`, id)
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		adjuster := newWeightAdjuster(upstream{baseURL: srv.URL, key: "k", slots: make(chan struct{}, 8), client: srv.Client()}, true, 30*time.Minute, func() time.Time { return now })
		if err := adjuster.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce failed: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		return weights
	}

	win7d := func(util float64) string {
		return fmt.Sprintf(`[{"id":"7d","unit":"percent","scope":"account","utilization":%v,"window_seconds":604800,"reset_at_ms":%d}]`, util, reset7d.UnixMilli())
	}
	win5h := func(util float64) string {
		return fmt.Sprintf(`[{"id":"5h","unit":"percent","scope":"account","utilization":%v,"window_seconds":18000,"reset_at_ms":%d}]`, util, reset5h.UnixMilli())
	}

	t.Run("all_over_safe_line", func(t *testing.T) {
		got := runScenario(t, []credSpec{
			{id: 1, email: "a@e.com", cfgStatus: "active", effStatus: "available", weight: 50, costNano: "20000000000", windows: win7d(0.20)},
			{id: 2, email: "b@e.com", cfgStatus: "active", effStatus: "available", weight: 80, costNano: "30000000000", windows: win7d(0.30)},
		})
		if got[1] != 5 || got[2] != 5 {
			t.Fatalf("all over safe line weights = %v, want both 5", got)
		}
	})

	t.Run("single_account_positive_and_over", func(t *testing.T) {
		pos := runScenario(t, []credSpec{
			{id: 1, email: "a@e.com", cfgStatus: "active", effStatus: "available", weight: 50, costNano: "10000000000", windows: win7d(0.05)},
		})
		if pos[1] != 100 {
			t.Fatalf("single positive weight = %d, want 100", pos[1])
		}
		over := runScenario(t, []credSpec{
			{id: 1, email: "a@e.com", cfgStatus: "active", effStatus: "available", weight: 50, costNano: "20000000000", windows: win7d(0.20)},
		})
		if over[1] != 5 {
			t.Fatalf("single over safe line weight = %d, want 5", over[1])
		}
	})

	t.Run("multi_window_and_uncalculable_skip", func(t *testing.T) {
		// cred 1: 7d safe $13.60 (util 0.05, cost $10) + 5h safe $1.00 (util 0.10, cost $10) -> prefers 7d ($13.60) => 100
		// cred 2: only 5h safe $6.80 (util 0.10, cost $68 -> est $680, safe_level 20% -> safe $68? wait: 1h/5h = 20%, util=0.10, cost=$6.80 -> est=$68 -> safe $6.80) => 50
		// cred 3: 7d at 0% (cost 0 -> safe nil) + 5h calculable -> 7d exists so must NOT fall back to 5h; skipped, keeps 37!
		multi := fmt.Sprintf(`[
			{"id":"5h","unit":"percent","scope":"account","utilization":0.10,"window_seconds":18000,"reset_at_ms":%d},
			{"id":"7d","unit":"percent","scope":"account","utilization":0.05,"window_seconds":604800,"reset_at_ms":%d}
		]`, reset5h.UnixMilli(), reset7d.UnixMilli())
		resetWin := fmt.Sprintf(`[
			{"id":"5h","unit":"percent","scope":"account","utilization":0.10,"window_seconds":18000,"reset_at_ms":%d},
			{"id":"7d","unit":"percent","scope":"account","utilization":0,"window_seconds":604800,"reset_at_ms":%d}
		]`, reset5h.UnixMilli(), reset7d.UnixMilli())

		got := runScenario(t, []credSpec{
			{id: 1, email: "a@e.com", cfgStatus: "active", effStatus: "available", weight: 50, costNano: "10000000000", windows: multi},
			{id: 2, email: "b@e.com", cfgStatus: "active", effStatus: "available", weight: 50, costNano: "6800000000", windows: win5h(0.10)},
			{id: 3, email: "c@e.com", cfgStatus: "active", effStatus: "available", weight: 37, costNano: "0", windows: resetWin},
		})
		if got[1] != 100 || got[2] != 50 || got[3] != 37 {
			t.Fatalf("multi-window weights = %v, want map[1:100 2:50 3:37]", got)
		}
	})

	t.Run("pricing_incomplete_cooldown_blacklisted_and_disabled", func(t *testing.T) {
		got := runScenario(t, []credSpec{
			{id: 1, email: "a@e.com", cfgStatus: "active", effStatus: "cooldown", weight: 50, costNano: "20000000000", partial: 1, windows: win7d(0.05)},
			{id: 2, email: "b@e.com", cfgStatus: "active", effStatus: "blacklisted", weight: 50, costNano: "10000000000", windows: win7d(0.05)},
			{id: 3, email: "c@e.com", cfgStatus: "disabled", effStatus: "disabled", weight: 42, costNano: "100000000000", windows: win7d(0.05)},
		})
		if got[1] != 100 || got[2] != 50 || got[3] != 42 {
			t.Fatalf("status participation weights = %v, want map[1:100 2:50 3:42]", got)
		}
	})
}

func TestWeightAdjusterPartialPutFailure(t *testing.T) {
	now := time.Date(2026, 11, 23, 6, 54, 0, 0, time.UTC)
	start := now.Add(-5*time.Hour - 54*time.Minute)
	reset := start.Add(7 * 24 * time.Hour)

	var mu sync.Mutex
	failCred1 := true
	weights := map[int64]int{1: 50, 2: 30}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/modern/groups":
			fmt.Fprintf(w, `{"code":0,"data":{"observed_at_ms":%d,"items":[{"id":1,"name":"Codex","channel_id":"codex","connection_type":"subscription","enabled":true}]}}`, now.UnixMilli())
		case r.Method == http.MethodGet && r.URL.Path == "/api/modern/groups/1/credentials":
			fmt.Fprintf(w, `{"code":0,"data":{"observed_at_ms":%d,"pagination":{"page":1,"page_size":100,"total_items":2,"total_pages":1},"items":[
				{"credential_id":1,"account":{"email":"private@email.com"},"configured_status":"active","effective_status":"available","weight":%d,"observation":{"state":"fresh","observed_at_ms":%d,"snapshot":{"quota_windows":[{"id":"7d","unit":"percent","scope":"account","utilization":0.05,"window_seconds":604800,"reset_at_ms":%d}]}}},
				{"credential_id":2,"account":{"email":"other@email.com"},"configured_status":"active","effective_status":"available","weight":%d,"observation":{"state":"fresh","observed_at_ms":%d,"snapshot":{"quota_windows":[{"id":"7d","unit":"percent","scope":"account","utilization":0.05,"window_seconds":604800,"reset_at_ms":%d}]}}}
			]}}`, now.UnixMilli(), weights[1], now.UnixMilli(), reset.UnixMilli(), weights[2], now.UnixMilli(), reset.UnixMilli())
		case r.Method == http.MethodGet && r.URL.Path == "/api/usage":
			cost := map[string]string{"1": "20000000000", "2": "10000000000"}[r.URL.Query().Get("credential_id")]
			fmt.Fprintf(w, `{"code":0,"data":{"summary":{"request_count":1,"estimated_cost_nano_usd":%q}}}`, cost)
		case r.Method == http.MethodPut && r.URL.Path == "/api/groups/1/credentials/1":
			if failCred1 {
				http.Error(w, "secret-admin-key private@email.com", http.StatusInternalServerError)
				return
			}
			var body struct {
				WeightManual int `json:"weight_manual"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			weights[1] = body.WeightManual
			fmt.Fprintf(w, `{"code":0,"data":{"credential_id":1}}`)
		case r.Method == http.MethodPut && r.URL.Path == "/api/groups/1/credentials/2":
			var body struct {
				WeightManual int `json:"weight_manual"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			weights[2] = body.WeightManual
			fmt.Fprintf(w, `{"code":0,"data":{"credential_id":2}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	adjuster := newWeightAdjuster(upstream{baseURL: srv.URL, key: "secret-admin-key", slots: make(chan struct{}, 8), client: srv.Client()}, true, 30*time.Minute, func() time.Time { return now })
	if err := adjuster.RunOnce(context.Background()); err == nil {
		t.Fatal("expected error when credential 1 PUT fails")
	}

	mu.Lock()
	w1, w2 := weights[1], weights[2]
	failCred1 = false
	mu.Unlock()
	if w1 != 50 || w2 != 50 {
		t.Fatalf("partial failure weights = (%d, %d), want (50, 50)", w1, w2)
	}
	status := adjuster.Status()
	if status.LastRunAt != now.UnixMilli() || status.Error == "" {
		t.Fatalf("expected non-empty sanitized error and timestamp, got %+v", status)
	}
	if strings.Contains(status.Error, "secret-admin-key") || strings.Contains(status.Error, "private@email.com") {
		t.Fatalf("status.Error leaked sensitive upstream details: %q", status.Error)
	}

	// Next healthy run clears error.
	if err := adjuster.RunOnce(context.Background()); err != nil {
		t.Fatalf("second RunOnce failed: %v", err)
	}
	status = adjuster.Status()
	if status.Error != "" {
		t.Fatalf("expected error to clear after healthy run, got %q", status.Error)
	}
	mu.Lock()
	if weights[1] != 100 {
		t.Errorf("credential 1 weight = %d, want 100", weights[1])
	}
	mu.Unlock()
}

func TestWeightAdjusterSkipsOnPartialUsageFetchError(t *testing.T) {
	now := time.Date(2026, 11, 23, 6, 54, 0, 0, time.UTC)
	start := now.Add(-5*time.Hour - 54*time.Minute)
	reset := start.Add(7 * 24 * time.Hour)

	var mu sync.Mutex
	weights := map[int64]int{11: 45, 12: 30, 21: 50}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/modern/groups":
			fmt.Fprintf(w, `{"code":0,"data":{"observed_at_ms":%d,"items":[
				{"id":1,"name":"Codex 1","channel_id":"codex","connection_type":"subscription","enabled":true},
				{"id":2,"name":"Codex Disabled","channel_id":"codex","connection_type":"subscription","enabled":false}
			]}}`, now.UnixMilli())
		case r.Method == http.MethodGet && r.URL.Path == "/api/modern/groups/1/credentials":
			fmt.Fprintf(w, `{"code":0,"data":{"observed_at_ms":%d,"pagination":{"page":1,"page_size":100,"total_items":2,"total_pages":1},"items":[
				{"credential_id":11,"account":{"email":"shared@example.com"},"configured_status":"active","effective_status":"available","weight":%d,"observation":{"state":"fresh","observed_at_ms":%d,"snapshot":{"quota_windows":[{"id":"7d","unit":"percent","scope":"account","utilization":0.05,"window_seconds":604800,"reset_at_ms":%d}]}}},
				{"credential_id":12,"account":{"email":"peer@example.com"},"configured_status":"active","effective_status":"available","weight":%d,"observation":{"state":"fresh","observed_at_ms":%d,"snapshot":{"quota_windows":[{"id":"7d","unit":"percent","scope":"account","utilization":0.05,"window_seconds":604800,"reset_at_ms":%d}]}}}
			]}}`, now.UnixMilli(), weights[11], now.UnixMilli(), reset.UnixMilli(), weights[12], now.UnixMilli(), reset.UnixMilli())
		case r.Method == http.MethodGet && r.URL.Path == "/api/modern/groups/2/credentials":
			fmt.Fprintf(w, `{"code":0,"data":{"observed_at_ms":%d,"pagination":{"page":1,"page_size":100,"total_items":1,"total_pages":1},"items":[
				{"credential_id":21,"account":{"email":"shared@example.com"},"configured_status":"active","effective_status":"disabled","weight":%d,"observation":{"state":"fresh","observed_at_ms":%d,"snapshot":{"quota_windows":[{"id":"7d","unit":"percent","scope":"account","utilization":0.05,"window_seconds":604800,"reset_at_ms":%d}]}}}
			]}}`, now.UnixMilli(), weights[21], now.UnixMilli(), reset.UnixMilli())
		case r.Method == http.MethodGet && r.URL.Path == "/api/usage":
			switch r.URL.Query().Get("credential_id") {
			case "21":
				http.Error(w, "upstream error", http.StatusServiceUnavailable)
			case "11", "12":
				fmt.Fprint(w, `{"code":0,"data":{"summary":{"request_count":1,"estimated_cost_nano_usd":"10000000000"}}}`)
			default:
				http.NotFound(w, r)
			}
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/groups/1/credentials/"):
			idStr := strings.TrimPrefix(r.URL.Path, "/api/groups/1/credentials/")
			id, _ := strconv.ParseInt(idStr, 10, 64)
			var body struct {
				WeightManual int `json:"weight_manual"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			weights[id] = body.WeightManual
			fmt.Fprintf(w, `{"code":0,"data":{"credential_id":%d}}`, id)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	adjuster := newWeightAdjuster(upstream{baseURL: srv.URL, key: "k", slots: make(chan struct{}, 8), client: srv.Client()}, true, 30*time.Minute, func() time.Time { return now })
	if err := adjuster.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if weights[11] != 45 {
		t.Errorf("credential 11 with partial usage fetch error should be skipped, got weight %d want 45", weights[11])
	}
	if weights[12] != 100 {
		t.Errorf("healthy peer credential 12 weight = %d, want 100", weights[12])
	}
}
