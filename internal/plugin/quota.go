package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/dillonzq/cpa-minimax/internal/catalog"
	"github.com/dillonzq/cpa-minimax/internal/config"
	"github.com/dillonzq/cpa-minimax/internal/errclass"
)

// quotaModelRemains is one model_remains entry of the subscription remains
// endpoint. Times are Unix milliseconds; *_remains_time is milliseconds until
// the window resets. MiniMax acknowledged that *_usage_count carries the
// remaining count, so quota.usage-count selects the interpretation.
type quotaModelRemains struct {
	ModelName         string   `json:"model_name"`
	StartTime         *float64 `json:"start_time"`
	EndTime           *float64 `json:"end_time"`
	RemainsTime       *float64 `json:"remains_time"`
	IntervalTotal     *float64 `json:"current_interval_total_count"`
	IntervalUsage     *float64 `json:"current_interval_usage_count"`
	WeeklyTotal       *float64 `json:"current_weekly_total_count"`
	WeeklyUsage       *float64 `json:"current_weekly_usage_count"`
	WeeklyEndTime     *float64 `json:"weekly_end_time"`
	WeeklyRemainsTime *float64 `json:"weekly_remains_time"`
}

// FetchQuota uses the credential selected by the host, never a default config
// key. Subscription keys report their 5-hour and weekly windows; MiniMax has
// no official balance API for pay-as-you-go keys, so those report the plan
// only and run out reactively through error 1008 (HTTP 402 cooldown).
func (m *Manager) FetchQuota(ctx context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	empty := pluginapi.QuotaFetchResponse{}
	if req.Provider != ProviderID {
		return empty, fmt.Errorf("unsupported quota provider")
	}
	key := strings.TrimSpace(req.Attributes["api_key"])
	if key == "" {
		return empty, fmt.Errorf("quota credential has no API key")
	}
	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()
	entry, err := resolveCredential(cfg, key, req.Attributes["region"], req.Attributes["key_type"], req.Attributes["base_url"])
	if err != nil {
		return empty, err
	}
	if entry.Type == config.KeyTypeAPI {
		return pluginapi.QuotaFetchResponse{Subscription: &pluginapi.QuotaSubscription{Plan: "Pay-as-you-go", TierName: regionName(entry.Region)}}, nil
	}
	if m.bridge == nil {
		return empty, fmt.Errorf("quota bridge unavailable")
	}
	timeout := cfg.RequestTimeout
	if timeout <= 0 || timeout > 20*time.Second {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	lastErr := fmt.Errorf("quota upstream request failed")
	for _, endpoint := range cfg.Quota.Endpoints {
		resp, err := m.bridge.Do(ctx, pluginapi.HTTPRequest{
			Method:  http.MethodGet,
			URL:     catalog.JoinUpstreamURL(cfg.UpstreamBaseURL(entry), endpoint),
			Headers: http.Header{"Authorization": {"Bearer " + key}, "Accept": {"application/json"}},
		})
		if err != nil {
			if ctx.Err() != nil {
				return empty, fmt.Errorf("quota upstream request failed")
			}
			continue
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return empty, fmt.Errorf("quota credential rejected (http %d)", resp.StatusCode)
		}
		if resp.StatusCode != http.StatusOK {
			// Try the next documented endpoint (e.g. the legacy path).
			lastErr = fmt.Errorf("quota upstream request failed (http %d)", resp.StatusCode)
			continue
		}
		if eErr := errclass.BaseRespError(resp.Body); eErr != nil {
			lastErr = fmt.Errorf("quota upstream error: %s", eErr.Message)
			if eErr.Class == errclass.ClassAuth {
				return empty, lastErr
			}
			continue
		}
		return normalizeQuota(resp.Body, entry.Region, cfg.Quota.UsageCount, time.Now())
	}
	return empty, lastErr
}

func normalizeQuota(body []byte, region, usageCount string, now time.Time) (pluginapi.QuotaFetchResponse, error) {
	var decoded struct {
		ModelRemains []quotaModelRemains `json:"model_remains"`
	}
	if json.Unmarshal(body, &decoded) != nil {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("quota response invalid")
	}
	groups := []pluginapi.QuotaGroup{}
	for _, entry := range decoded.ModelRemains {
		var buckets []pluginapi.QuotaBucket
		windows := []struct {
			name                   string
			total, count, end, rem *float64
			length                 string
		}{
			{"rolling", entry.IntervalTotal, entry.IntervalUsage, entry.EndTime, entry.RemainsTime, windowLength(entry.StartTime, entry.EndTime)},
			{"weekly", entry.WeeklyTotal, entry.WeeklyUsage, entry.WeeklyEndTime, entry.WeeklyRemainsTime, "7d"},
		}
		for _, w := range windows {
			bucket, ok, err := quotaBucket(w.name, w.length, w.total, w.count, w.end, w.rem, usageCount, now)
			if err != nil {
				return pluginapi.QuotaFetchResponse{}, err
			}
			if ok {
				buckets = append(buckets, bucket)
			}
		}
		if len(buckets) == 0 {
			continue
		}
		name := strings.TrimSpace(entry.ModelName)
		if name == "" {
			name = "MiniMax"
		}
		groups = append(groups, pluginapi.QuotaGroup{DisplayName: name, Buckets: buckets})
	}
	if len(groups) == 0 {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("quota response has no readings")
	}
	return pluginapi.QuotaFetchResponse{Subscription: &pluginapi.QuotaSubscription{Plan: "M Plan", TierName: regionName(region)}, Groups: groups}, nil
}

// quotaBucket normalizes one window. A missing or zero total means the
// window is not limited and is omitted; counts outside [0,total] are invalid
// readings, not clamped into invented values.
func quotaBucket(window, length string, total, count, endMs, remainsMs *float64, usageCount string, now time.Time) (pluginapi.QuotaBucket, bool, error) {
	if total == nil || *total == 0 || count == nil {
		return pluginapi.QuotaBucket{}, false, nil
	}
	if !finite(*total) || !finite(*count) || *total < 0 || *count < 0 || *count > *total {
		return pluginapi.QuotaBucket{}, false, fmt.Errorf("quota count invalid")
	}
	remaining := *count
	if usageCount == config.UsageCountUsed {
		remaining = *total - *count
	}
	reset := ""
	switch {
	case endMs != nil && finite(*endMs) && *endMs > 0:
		reset = time.UnixMilli(int64(*endMs)).UTC().Format(time.RFC3339Nano)
	case remainsMs != nil && finite(*remainsMs) && *remainsMs > 0:
		reset = now.Add(time.Duration(*remainsMs) * time.Millisecond).UTC().Format(time.RFC3339Nano)
	}
	description := fmt.Sprintf("%.0f of %.0f requests remaining", remaining, *total)
	if length != "" {
		description += " (" + length + " window)"
	}
	return pluginapi.QuotaBucket{Window: window, RemainingFraction: remaining / *total, ResetTime: reset, Description: description}, true, nil
}

// windowLength renders the observed interval length, e.g. "5h".
func windowLength(startMs, endMs *float64) string {
	if startMs == nil || endMs == nil || !finite(*startMs) || !finite(*endMs) || *endMs <= *startMs {
		return ""
	}
	d := time.Duration(*endMs-*startMs) * time.Millisecond
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int64(d/time.Hour))
	}
	return d.Round(time.Minute).String()
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func regionName(region string) string {
	if region == config.RegionCN {
		return "China"
	}
	return "Global"
}
