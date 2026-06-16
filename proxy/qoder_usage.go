package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/ttimasdf/qoder2api/auth"
)

// ============================================================================
// Qoder 用量探针
//
// codex2api 通过 /backend-api/wham/usage 获取账号用量；qoder 的等价接口是
// <BigModel>/api/v2/user/plan（配额）。为了让 admin 的批量测试/用量探针代码
// 无需改动，这里保留 WhamUsage / QueryWhamUsage / ApplyWhamUsage 的契约，
// 内部改为查询 Qoder plan 接口。
//
// 注意：Qoder plan 响应字段尚未通过真实账号联调确认（见 docs/QODER_PROTOCOL.md），
// 这里按常见的 { planType, quota:{ used, total, resetAt } } 形态解析；联调后按需调整。
// ============================================================================

const qoderUserPlanPath = "/api/v2/user/plan"

// WhamUsage 复用 codex 时代的结构名，承载 Qoder 配额信息。
type WhamUsage struct {
	UserID   string `json:"user_id"`
	PlanType string `json:"plan_type"`

	RateLimit struct {
		LimitReached    bool             `json:"limit_reached"`
		PrimaryWindow   *WhamUsageWindow `json:"primary_window"`
		SecondaryWindow *WhamUsageWindow `json:"secondary_window"`
	} `json:"rate_limit"`
}

// WhamUsageWindow 单个限流窗口。
type WhamUsageWindow struct {
	UsedPercent       float64 `json:"used_percent"`
	ResetAfterSeconds int64   `json:"reset_after_seconds"`
	ResetAt           int64   `json:"reset_at"`
}

// QueryWhamUsage 查询 Qoder 账号配额。签名与 codex 版保持一致，供 admin 复用。
func QueryWhamUsage(ctx context.Context, account *auth.Account, proxyURL string) (*WhamUsage, *http.Response, error) {
	if account == nil {
		return nil, nil, fmt.Errorf("account is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	ep := qoderEndpointSet(account)
	encode := ep.MessageEncode == "1"
	rawURL := qoderBigModelURL(account, qoderUserPlanPath, encode)

	req, err := buildQoderCosyRequest(ctx, account, http.MethodGet, rawURL, []byte("{}"))
	if err != nil {
		return nil, nil, fmt.Errorf("build qoder plan request: %w", err)
	}

	resp, err := getPooledClient(account, proxyURL).Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("qoder plan request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp, fmt.Errorf("qoder plan returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	if err != nil {
		return nil, resp, fmt.Errorf("read qoder plan response: %w", err)
	}

	usage := parseQoderPlan(body)
	return usage, resp, nil
}

// parseQoderPlan 把 plan 响应映射为 WhamUsage。
func parseQoderPlan(body []byte) *WhamUsage {
	u := &WhamUsage{}
	u.PlanType = firstGJSONString(body, "planType", "plan_type", "data.planType")
	u.UserID = firstGJSONString(body, "userId", "user_id", "data.userId")

	// quota.used / quota.total → used_percent
	used := gjson.GetBytes(body, "quota.used").Float()
	total := gjson.GetBytes(body, "quota.total").Float()
	if total <= 0 {
		used = gjson.GetBytes(body, "data.quota.used").Float()
		total = gjson.GetBytes(body, "data.quota.total").Float()
	}
	if total > 0 {
		pct := used / total * 100
		resetAt := firstGJSONInt(body, "quota.resetAt", "data.quota.resetAt")
		u.RateLimit.PrimaryWindow = &WhamUsageWindow{
			UsedPercent: pct,
			ResetAt:     resetAt,
		}
		if pct >= 100 {
			u.RateLimit.LimitReached = true
		}
	}
	return u
}

// ApplyWhamUsage 把配额写入账号 state + 持久化。返回与 codex 版兼容的同步结果。
func ApplyWhamUsage(store *auth.Store, account *auth.Account, usage *WhamUsage) CodexUsageSyncResult {
	result := CodexUsageSyncResult{}
	if account == nil || usage == nil {
		return result
	}
	if store != nil && usage.PlanType != "" {
		store.UpdateAccountPlanType(account, usage.PlanType)
	}
	now := time.Now()
	if w := usage.RateLimit.PrimaryWindow; w != nil {
		resetAt := qoderWindowResetAt(w, now)
		// Qoder 当前仅有单一配额窗口，落到 5h 槽位即可参与调度惩罚。
		account.SetUsageSnapshot5h(w.UsedPercent, resetAt)
		result.UsagePct5h = w.UsedPercent
		result.Reset5hAt = resetAt
		result.HasUsage5h = true
		result.Used5hHeaders = true
		if store != nil {
			store.PersistUsageSnapshot5hOnly(account)
			result.Persisted5hOnly = true
		}
	}
	return result
}

func qoderWindowResetAt(w *WhamUsageWindow, now time.Time) time.Time {
	if w == nil {
		return time.Time{}
	}
	if w.ResetAt > 0 {
		// 毫秒或秒时间戳都兼容
		if w.ResetAt > 1e12 {
			return time.UnixMilli(w.ResetAt)
		}
		return time.Unix(w.ResetAt, 0)
	}
	if w.ResetAfterSeconds > 0 {
		return now.Add(time.Duration(w.ResetAfterSeconds) * time.Second)
	}
	return time.Time{}
}

var _ = strings.TrimSpace
