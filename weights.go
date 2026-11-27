package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

const (
	minAutoWeight       = 5
	maxAutoWeight       = 100
	weightAdjustTimeout = 45 * time.Second
)

type weightAdjustStatus struct {
	Enabled   bool   `json:"enabled"`
	LastRunAt int64  `json:"last_run_at_ms,omitempty"`
	Error     string `json:"error,omitempty"`
}

type weightAdjuster struct {
	upstream upstream
	enabled  bool
	interval time.Duration
	now      func() time.Time

	mu     sync.RWMutex
	status weightAdjustStatus
}

func newWeightAdjuster(u upstream, enabled bool, interval time.Duration, now func() time.Time) *weightAdjuster {
	if now == nil {
		now = time.Now
	}
	return &weightAdjuster{
		upstream: u,
		enabled:  enabled,
		interval: interval,
		now:      now,
		status:   weightAdjustStatus{Enabled: enabled},
	}
}

func (w *weightAdjuster) Status() weightAdjustStatus {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.status
}

func (w *weightAdjuster) Start(ctx context.Context) {
	if !w.enabled || w.interval <= 0 {
		return
	}
	run := func() {
		runCtx, cancel := context.WithTimeout(ctx, weightAdjustTimeout)
		defer cancel()
		_ = w.RunOnce(runCtx)
	}
	run()

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

type groupSafeQuota struct {
	credentialID int64
	safeUSD      float64
}

type plannedWeightUpdate struct {
	groupID      int64
	credentialID int64
	target       int
}

func (w *weightAdjuster) RunOnce(ctx context.Context) error {
	if !w.enabled {
		return nil
	}
	now := w.now()
	snap, err := w.upstream.loadSubscriptionSnapshot(ctx, now)
	if err != nil {
		log.Printf("GPT-Load auto-weight snapshot: %v", err)
		w.recordStatus(now, "订阅账号与额度拉取失败，本轮未调整权重。")
		return err
	}

	var groupOrder []int64
	membersByGroup := make(map[int64][]subscriptionCredentialLink)
	for _, m := range snap.Memberships {
		if _, exists := membersByGroup[m.GroupID]; !exists {
			groupOrder = append(groupOrder, m.GroupID)
		}
		membersByGroup[m.GroupID] = append(membersByGroup[m.GroupID], m)
	}

	var planned []plannedWeightUpdate
	for _, groupID := range groupOrder {
		members := membersByGroup[groupID]
		calculables := make([]groupSafeQuota, 0, len(members))
		for _, m := range members {
			safe, ok := m.Observation.remainingSafeQuota()
			if !ok {
				continue
			}
			calculables = append(calculables, groupSafeQuota{credentialID: m.CredentialID, safeUSD: safe})
		}
		targets := computeGroupTargetWeights(calculables)
		for _, m := range members {
			target, ok := targets[m.CredentialID]
			if !ok {
				continue
			}
			if m.CurrentWeight != nil && *m.CurrentWeight == target {
				continue
			}
			planned = append(planned, plannedWeightUpdate{
				groupID:      m.GroupID,
				credentialID: m.CredentialID,
				target:       target,
			})
		}
	}

	var failed atomic.Int64
	var wg sync.WaitGroup
	for _, item := range planned {
		wg.Go(func() {
			if err := w.upstream.updateCredentialWeight(ctx, item.groupID, item.credentialID, item.target); err != nil {
				failed.Add(1)
				log.Printf("GPT-Load auto-weight update group=%d credential=%d target=%d: %v", item.groupID, item.credentialID, item.target, err)
			}
		})
	}
	wg.Wait()

	if count := failed.Load(); count > 0 {
		msg := fmt.Sprintf("本轮 %d/%d 个凭据权重更新失败，详见服务端日志。", count, len(planned))
		w.recordStatus(now, msg)
		return errors.New(msg)
	}
	w.recordStatus(now, "")
	return nil
}

func (w *weightAdjuster) recordStatus(now time.Time, errMsg string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status.LastRunAt = now.UnixMilli()
	w.status.Error = errMsg
}

func computeGroupTargetWeights(entries []groupSafeQuota) map[int64]int {
	targets := make(map[int64]int, len(entries))
	if len(entries) == 0 {
		return targets
	}
	var maxSafe float64
	for _, entry := range entries {
		if entry.safeUSD > maxSafe {
			maxSafe = entry.safeUSD
		}
	}
	for _, entry := range entries {
		if maxSafe <= 0 || entry.safeUSD <= 0 {
			targets[entry.credentialID] = minAutoWeight
			continue
		}
		weight := int(math.Round(entry.safeUSD / maxSafe * maxAutoWeight))
		targets[entry.credentialID] = max(minAutoWeight, min(maxAutoWeight, weight))
	}
	return targets
}
