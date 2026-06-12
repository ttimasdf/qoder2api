package proxy

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ttimasdf/qoder2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ============================================================================
// Qoder / Qoder CN 上游（"Cosy" 签名协议）
//
// 协议来源：QoderCN 后端 Go 二进制（cosy/remoting 包）+ 运行态抓取，
// 详见 docs/QODER_PROTOCOL.md。请求流程：
//
//  1. POST <BigModel>/api/v2/service/invoke/choose_model?Encode=1
//     —— 路由到一个推理节点，返回 { model, endpoint, token }。
//  2. POST <inferEndpoint>/chat/completions  —— OpenAI 兼容的流式生成。
//
// 所有 big-model 请求都带 Cosy 签名头（见 applyQoderCosyHeaders）。
// ============================================================================

const (
	// QoderDefaultClientVersion 默认 Cosy-Version。
	QoderDefaultClientVersion = "2.6.0"
	// QoderClientType 对应 Cosy-ClientType。
	QoderClientType = "vscode"

	qoderChooseModelPath = "/api/v2/service/invoke/choose_model"
	qoderModelListPath   = "/api/v2/model/list"
)

// QoderEdition 控制使用国际版还是 CN 版端点，默认 CN（qoder-cn）。
var QoderEdition = auth.QoderEditionCN

// qoderChooseModelResponse 是 choose_model 的响应。
type qoderChooseModelResponse struct {
	Model         string `json:"model"`
	ModelName     string `json:"modelName"`
	Endpoint      string `json:"endpoint"`
	Token         string `json:"token"`
	BizType       string `json:"bizType"`
	SecurityToken string `json:"securityToken"`
}

// signedQoderRequest 携带 Cosy 签名所需的全部输入。
// qoderSignatureSalt 是 Cosy 签名固定盐，逆向自 qodercncli
// addBigModelSignatureHeaders（见 docs/QODER_SIGNING.md）。这是一个 base64 文本
// 字面量，按原文参与 MD5，不做解码。
const (
	qoderSignaturePrefix = "cosy"
	qoderSignatureSalt   = "d2FyLCB3YXIgbmV2ZXIgY2hhbmdlcw=="
	// qoderHTTPDateLayout 是签名所用 RFC1123 GMT 时间格式。
	qoderHTTPDateLayout = "Mon, 02 Jan 2006 15:04:05 GMT"
)

type signedQoderRequest struct {
	method  string
	fullURL string
	body    []byte // 请求体，原样发送（不参与签名）
	date    string // Date 头：RFC1123 GMT，参与签名
	cosyKey string // Cosy-Key
}

// buildQoderCosyRequest 构造一个带 Cosy 签名头的 *http.Request。
//
// 签名算法逆向自 qodercncli core/utils/qoder.addBigModelSignatureHeaders：
//
//	date = time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")
//	Signature = hex(md5("cosy" + "d2FyLCB3YXIgbmV2ZXIgY2hhbmdlcw==" + date))
//
// 请求体不参与签名，原样发送（推理节点为 OpenAI 兼容 /chat/completions）。
func buildQoderCosyRequest(ctx context.Context, account *auth.Account, method, rawURL string, rawBody []byte) (*http.Request, error) {
	accessToken, userID, orgID, machineID, machineToken, clientVer := account.QoderCredentials()
	if accessToken == "" {
		return nil, ErrNoAvailableAccount()
	}
	if clientVer == "" {
		clientVer = QoderDefaultClientVersion
	}

	sr := signedQoderRequest{
		method:  method,
		fullURL: rawURL,
		body:    rawBody,
		date:    time.Now().UTC().Format(qoderHTTPDateLayout),
		cosyKey: strconv.FormatInt(time.Now().UnixMilli(), 10),
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(sr.body))
	if err != nil {
		return nil, ErrInternalError("创建 Qoder 请求失败", err)
	}

	applyQoderCosyHeaders(req, sr, accessToken, userID, orgID, machineID, machineToken, clientVer)
	return req, nil
}

// applyQoderCosyHeaders 写入 Cosy 协议要求的请求头与签名。
func applyQoderCosyHeaders(req *http.Request, sr signedQoderRequest, accessToken, userID, orgID, machineID, machineToken, clientVer string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	// addBigModelAuthorizationHeaders
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Cosy-User", userID)
	req.Header.Set("Cosy-Key", sr.cosyKey)
	req.Header.Set("Cosy-Date", strconv.FormatInt(time.Now().Unix(), 10))
	// 设备/组织标识（IDE 守护进程一并发送；推理路径非必需但保持一致）
	if orgID != "" {
		req.Header.Set("Cosy-Organization-Id", orgID)
	}
	if machineID != "" {
		req.Header.Set("Cosy-MachineId", machineID)
	}
	if machineToken != "" {
		req.Header.Set("Cosy-MachineToken", machineToken)
	}
	req.Header.Set("Cosy-ClientType", QoderClientType)
	req.Header.Set("Cosy-Version", clientVer)

	// addBigModelSignatureHeaders
	req.Header.Set("Date", sr.date)
	req.Header.Set("Signature", computeQoderSignature(sr))
}

// computeQoderSignature 复刻 Cosy 签名：
//
//	hex(md5("cosy" + salt + date))
//
// 其中 date 是 RFC1123 GMT 的 Date 头值。请求体不参与。
func computeQoderSignature(sr signedQoderRequest) string {
	sum := md5.Sum([]byte(qoderSignaturePrefix + qoderSignatureSalt + sr.date))
	return hex.EncodeToString(sum[:])
}

// qoderBigModelBaseOverride 仅用于测试：非空时覆盖 big-model 基址。
var qoderBigModelBaseOverride string

// qoderBigModelURL 拼接 big-model 端点 + path（+ ?Encode=1）。
func qoderBigModelURL(path string, encode bool) string {
	base := strings.TrimRight(qoderBigModelBaseOverride, "/")
	if base == "" {
		ep := auth.QoderEndpoints(QoderEdition)
		base = strings.TrimRight(ep.BigModel, "/")
	}
	u := base + path
	if encode {
		u += "?Encode=1"
	}
	return u
}

// ExecuteQoderRequest 是 Qoder 账号的主执行入口，等价于 codex 的 ExecuteRequest。
//
// requestBody 为 OpenAI 风格的 /chat/completions 请求体。流程：
//  1. choose_model 路由出推理节点；
//  2. 向推理节点 /chat/completions 发起（可流式）请求并回传 *http.Response。
func ExecuteQoderRequest(ctx context.Context, account *auth.Account, requestBody []byte, proxyOverride string, headers http.Header) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	account.Mu().RLock()
	proxyURL := account.ProxyURL
	account.Mu().RUnlock()
	if proxyOverride != "" {
		proxyURL = proxyOverride
	}

	model := strings.TrimSpace(gjson.GetBytes(requestBody, "model").String())

	// 1. choose_model 路由
	route, err := qoderChooseModel(ctx, account, model, proxyURL)
	if err != nil {
		return nil, err
	}

	// 2. 推理请求：把 choose_model 返回的真实模型名写回请求体。
	body := requestBody
	if route.ModelName != "" {
		body, _ = sjson.SetBytes(body, "model", route.ModelName)
	} else if route.Model != "" {
		body, _ = sjson.SetBytes(body, "model", route.Model)
	}

	inferBase := strings.TrimRight(route.Endpoint, "/")
	if inferBase == "" {
		inferBase = strings.TrimRight(auth.QoderEndpoints(QoderEdition).Infer, "/")
	}
	endpoint := inferBase + "/chat/completions"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, ErrInternalError("创建 Qoder 推理请求失败", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	// choose_model 返回的短时 token 优先用于推理节点鉴权；否则回退账号 access_token。
	accessToken, _, _, _, _, _ := account.QoderCredentials()
	bearer := route.Token
	if bearer == "" {
		bearer = accessToken
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	if route.SecurityToken != "" {
		req.Header.Set("X-Security-Token", route.SecurityToken)
	}

	resp, err := getPooledClient(account, proxyURL).Do(req)
	if err != nil {
		if shouldRecyclePooledClient(err) {
			recyclePooledClient(account, proxyURL)
		}
		return nil, ErrUpstream(0, "请求 Qoder 推理节点失败", err)
	}
	return resp, nil
}

// qoderChooseModel 调用 choose_model 路由接口。
func qoderChooseModel(ctx context.Context, account *auth.Account, model, proxyURL string) (*qoderChooseModelResponse, error) {
	ep := auth.QoderEndpoints(QoderEdition)
	encode := ep.MessageEncode == "1"

	reqPayload, _ := json.Marshal(map[string]any{"model": model})
	rawURL := qoderBigModelURL(qoderChooseModelPath, encode)

	req, err := buildQoderCosyRequest(ctx, account, http.MethodPost, rawURL, reqPayload)
	if err != nil {
		return nil, err
	}

	resp, err := getPooledClient(account, proxyURL).Do(req)
	if err != nil {
		if shouldRecyclePooledClient(err) {
			recyclePooledClient(account, proxyURL)
		}
		return nil, ErrUpstream(0, "Qoder choose_model 失败", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, ErrUpstream(resp.StatusCode, "Qoder choose_model 返回错误", fmt.Errorf("%s", string(respBody)))
	}
	var route qoderChooseModelResponse
	if err := json.Unmarshal(respBody, &route); err != nil {
		return nil, ErrInternalError("解析 Qoder choose_model 响应失败", err)
	}
	return &route, nil
}

// QoderModel 是 /api/v2/model/list 返回的单个模型。
type QoderModel struct {
	ModelID     string   `json:"modelId"`
	ModelName   string   `json:"modelName"`
	DisplayName string   `json:"displayName"`
	Provider    string   `json:"provider"`
	MaxTokens   int64    `json:"maxTokens"`
	Capability  []string `json:"capabilities"`
}

// ListQoderModels 拉取上游模型列表。
func ListQoderModels(ctx context.Context, account *auth.Account, proxyURL string) ([]QoderModel, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ep := auth.QoderEndpoints(QoderEdition)
	encode := ep.MessageEncode == "1"
	rawURL := qoderBigModelURL(qoderModelListPath, encode)
	req, err := buildQoderCosyRequest(ctx, account, http.MethodGet, rawURL, []byte("{}"))
	if err != nil {
		return nil, err
	}
	resp, err := getPooledClient(account, proxyURL).Do(req)
	if err != nil {
		return nil, ErrUpstream(0, "Qoder model/list 失败", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, ErrUpstream(resp.StatusCode, "Qoder model/list 返回错误", fmt.Errorf("%s", string(body)))
	}
	// 响应可能为 {"data":[...]} 或裸数组。
	var out []QoderModel
	if gjson.GetBytes(body, "data").IsArray() {
		_ = json.Unmarshal([]byte(gjson.GetBytes(body, "data").Raw), &out)
	} else {
		_ = json.Unmarshal(body, &out)
	}
	return out, nil
}
