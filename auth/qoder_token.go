package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Qoder / Qoder CN OAuth + 端点常量。
//
// 端点取自 QoderCN 后端二进制中嵌入的 remote_config（详见 docs/QODER_PROTOCOL.md）。
// 国际版（qoder.com）与 CN 版（Aliyun RDC）端点不同，通过 QoderEndpoints 选择。
const (
	// QoderEditionIntl 国际版（qoder.sh / qoder.com）。
	QoderEditionIntl = "intl"
	// QoderEditionCN 中国版（Aliyun RDC anquan-cn-beijing）。
	QoderEditionCN = "cn"

	qoderDefaultClientVersion = "2.6.0"
)

// QoderEndpointSet 描述一个 Qoder 版本的服务端点。
type QoderEndpointSet struct {
	// BigModel 是 /algo/api/... 调用的基址（含可选 /algo 前缀已在 path 中体现）。
	BigModel string
	// Infer 推理节点基址（choose_model 通常会返回动态 endpoint 覆盖该值）。
	Infer string
	// OpenAPI 账号/配额等管理 API 基址。
	OpenAPI string
	// LoginURL 浏览器登录入口。
	LoginURL string
	// MessageEncode 对应 remote_config.message_encode（"1" 表示 body base64 编码）。
	MessageEncode string
}

// QoderEndpoints 返回指定版本的端点集合。
func QoderEndpoints(edition string) QoderEndpointSet {
	switch strings.ToLower(strings.TrimSpace(edition)) {
	case QoderEditionCN:
		return QoderEndpointSet{
			BigModel:      "https://qoder.com.cn",
			Infer:         "https://qoder.com.cn",
			OpenAPI:       "https://qoder.com.cn",
			LoginURL:      "https://devops.aliyun.com/lingma/login",
			MessageEncode: "1",
		}
	default: // intl
		return QoderEndpointSet{
			BigModel:      "https://center.qoder.sh/algo",
			Infer:         "https://api3.qoder.sh",
			OpenAPI:       "https://openapi.qoder.sh",
			LoginURL:      "https://www.qoder.com/device/selectAccounts",
			MessageEncode: "1",
		}
	}
}

// QoderTokenData 保存一次刷新得到的 Qoder 令牌。
type QoderTokenData struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"-"`
}

// QoderAccountInfo 是登录/刷新响应携带的账号信息。
type QoderAccountInfo struct {
	UserID         string
	OrganizationID string
	Email          string
	PlanType       string
}

// qoderLoginResponse 是 deviceToken/poll 与 refresh_token 的通用响应体。
// 字段名取自二进制中出现的 JSON key（access_token / refresh_token / userId /
// organizationId / expireTime）。
type qoderLoginResponse struct {
	AccessToken    string `json:"access_token"`
	RefreshToken   string `json:"refresh_token"`
	Token          string `json:"token"`
	UserID         string `json:"userId"`
	OrganizationID string `json:"organizationId"`
	OrgID          string `json:"orgId"`
	Email          string `json:"email"`
	PlanType       string `json:"planType"`
	Expires        int64  `json:"expires"`
	ExpireTime     int64  `json:"expireTime"`
	ExpiresIn      int64  `json:"expires_in"`
}

func (r *qoderLoginResponse) token() QoderTokenData {
	at := strings.TrimSpace(r.AccessToken)
	if at == "" {
		at = strings.TrimSpace(r.Token)
	}
	expiresAt := time.Time{}
	switch {
	case r.ExpireTime > 0:
		expiresAt = time.UnixMilli(r.ExpireTime)
	case r.Expires > 0:
		expiresAt = time.UnixMilli(r.Expires)
	case r.ExpiresIn > 0:
		expiresAt = time.Now().Add(time.Duration(r.ExpiresIn) * time.Second)
	}
	return QoderTokenData{AccessToken: at, RefreshToken: strings.TrimSpace(r.RefreshToken), ExpiresAt: expiresAt}
}

func (r *qoderLoginResponse) info() QoderAccountInfo {
	org := strings.TrimSpace(r.OrganizationID)
	if org == "" {
		org = strings.TrimSpace(r.OrgID)
	}
	return QoderAccountInfo{
		UserID:         strings.TrimSpace(r.UserID),
		OrganizationID: org,
		Email:          strings.TrimSpace(r.Email),
		PlanType:       strings.TrimSpace(r.PlanType),
	}
}

// RefreshQoderToken 使用 refresh_token 换取新的 access_token。
// 对应上游 POST <OpenAPI>/api/v3/user/refresh_token。
func RefreshQoderToken(ctx context.Context, edition, refreshToken, proxyURL string) (*QoderTokenData, *QoderAccountInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil, nil, fmt.Errorf("qoder: 缺少 refresh_token")
	}
	ep := QoderEndpoints(edition)
	endpoint := strings.TrimRight(ep.OpenAPI, "/") + "/api/v3/user/refresh_token"

	payload, _ := json.Marshal(map[string]string{"refresh_token": refreshToken})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, fmt.Errorf("qoder: 创建刷新请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := buildHTTPClient(proxyURL).Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("qoder: 刷新请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("qoder: 刷新失败 status=%d body=%s", resp.StatusCode, string(body))
	}

	var parsed qoderLoginResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, nil, fmt.Errorf("qoder: 解析刷新响应失败: %w", err)
	}
	td := parsed.token()
	if td.AccessToken == "" {
		return nil, nil, fmt.Errorf("qoder: 刷新响应缺少 access_token: %s", string(body))
	}
	if td.RefreshToken == "" {
		td.RefreshToken = refreshToken
	}
	info := parsed.info()
	return &td, &info, nil
}

// QoderDeviceLogin 描述一次设备登录流程的中间态。
type QoderDeviceLogin struct {
	Nonce           string
	LoginURL        string
	VerificationURI string
	DeviceCode      string
	Interval        time.Duration
}

// StartQoderDeviceLogin 发起设备登录：generate_nonce -> generate_url。
// 返回供用户在浏览器打开的登录 URL；随后用 PollQoderDeviceLogin 轮询结果。
func StartQoderDeviceLogin(ctx context.Context, edition, proxyURL string) (*QoderDeviceLogin, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ep := QoderEndpoints(edition)
	client := buildHTTPClient(proxyURL)
	base := strings.TrimRight(ep.OpenAPI, "/")

	// 1. generate_nonce
	nonce, err := qoderPostJSON(ctx, client, base+"/api/auth/login/generate_nonce", nil)
	if err != nil {
		return nil, fmt.Errorf("qoder: generate_nonce 失败: %w", err)
	}
	nonceVal := firstNonEmpty(nonce["nonce"], nonce["data"])

	// 2. generate_url
	urlResp, err := qoderPostJSON(ctx, client, base+"/api/auth/login/generate_url", map[string]any{"nonce": nonceVal})
	if err != nil {
		return nil, fmt.Errorf("qoder: generate_url 失败: %w", err)
	}
	loginURL := firstNonEmpty(urlResp["url"], urlResp["login_url"], urlResp["data"])
	if loginURL == "" {
		loginURL = ep.LoginURL
	}
	return &QoderDeviceLogin{
		Nonce:    nonceVal,
		LoginURL: loginURL,
		Interval: 2 * time.Second,
	}, nil
}

// PollQoderDeviceLogin 轮询设备登录结果。登录完成返回令牌与账号信息。
// 对应上游 POST <OpenAPI>/api/v3/user/oauth2/deviceToken/poll。
func PollQoderDeviceLogin(ctx context.Context, edition, nonce, proxyURL string) (*QoderTokenData, *QoderAccountInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ep := QoderEndpoints(edition)
	endpoint := strings.TrimRight(ep.OpenAPI, "/") + "/api/v3/user/oauth2/deviceToken/poll"
	payload, _ := json.Marshal(map[string]string{"nonce": nonce})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := buildHTTPClient(proxyURL).Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("qoder: poll 请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("qoder: poll status=%d body=%s", resp.StatusCode, string(body))
	}
	var parsed qoderLoginResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, nil, fmt.Errorf("qoder: 解析 poll 响应失败: %w", err)
	}
	td := parsed.token()
	if td.AccessToken == "" {
		// 尚未完成登录
		return nil, nil, fmt.Errorf("qoder: 登录尚未完成")
	}
	info := parsed.info()
	return &td, &info, nil
}

func qoderPostJSON(ctx context.Context, client *http.Client, url string, payload map[string]any) (map[string]string, error) {
	var rdr io.Reader
	if payload != nil {
		b, _ := json.Marshal(payload)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status=%d body=%s", resp.StatusCode, string(body))
	}
	// 响应可能是 {"data": "..."} 或 {"nonce": "..."} 等，统一解析为扁平 string map。
	raw := map[string]any{}
	_ = json.Unmarshal(body, &raw)
	out := map[string]string{}
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// QoderDefaultClientVersion 暴露默认 Cosy-Version 供其它包使用。
func QoderDefaultClientVersion() string { return qoderDefaultClientVersion }
