package proxy

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
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
type signedQoderRequest struct {
	method  string
	fullURL string
	sigPath string // Cosy-SigPath：去掉 query 的请求路径
	body    []byte // 实际发送的 body（可能已 base64 编码）
	date    string // Cosy-Date：unix 毫秒
	key     string // Cosy-Key：每请求 uuid
	encode  bool   // message_encode == "1"
}

// buildQoderRequest 构造一个带 Cosy 签名头的 *http.Request。
//
// 注意：签名摘要算法依据二进制中验证器打印的 "Signature Components (used for MD5)"
// 复刻为：
//
//	MD5( base64(payload) + Cosy-Key + Cosy-Date + body + Cosy-SigPath )
//
// 其中 message_encode=1 时 payload/ body 为 base64 编码后的请求体。
// getAppSalt 派生的 body 加密尚未从静态分析中完全恢复（见 docs/QODER_PROTOCOL.md），
// 当前实现按"仅 base64 编码、不二次加密"处理；接入真实账号联调时如返回签名错误，
// 需要根据抓包补齐 salt。
func buildQoderCosyRequest(ctx context.Context, account *auth.Account, method, rawURL string, rawBody []byte) (*http.Request, error) {
	accessToken, userID, orgID, machineID, machineToken, clientVer := account.QoderCredentials()
	if accessToken == "" {
		return nil, ErrNoAvailableAccount()
	}
	if clientVer == "" {
		clientVer = QoderDefaultClientVersion
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, ErrInternalError("解析 Qoder URL 失败", err)
	}

	ep := auth.QoderEndpoints(QoderEdition)
	encode := ep.MessageEncode == "1"

	// payload：message_encode=1 时 body 用 base64 编码后发送。
	payload := rawBody
	if encode {
		encoded := make([]byte, base64.StdEncoding.EncodedLen(len(rawBody)))
		base64.StdEncoding.Encode(encoded, rawBody)
		payload = encoded
	}

	sr := signedQoderRequest{
		method:  method,
		fullURL: rawURL,
		sigPath: parsed.Path,
		body:    payload,
		date:    strconv.FormatInt(time.Now().UnixMilli(), 10),
		key:     uuid.NewString(),
		encode:  encode,
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

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Cosy-User", userID)
	req.Header.Set("Cosy-Organization-Id", orgID)
	req.Header.Set("Cosy-MachineId", machineID)
	req.Header.Set("Cosy-MachineToken", machineToken)
	req.Header.Set("Cosy-ClientType", QoderClientType)
	req.Header.Set("Cosy-Version", clientVer)
	req.Header.Set("Cosy-Date", sr.date)
	req.Header.Set("Cosy-Key", sr.key)
	req.Header.Set("Cosy-SigPath", sr.sigPath)
	req.Header.Set("Cosy-BodyLength", strconv.Itoa(len(sr.body)))

	bodyHash := md5.Sum(sr.body)
	req.Header.Set("Cosy-BodyHash", hex.EncodeToString(bodyHash[:]))

	req.Header.Set("X-Request-ID", uuid.NewString())

	sig := computeQoderSignature(sr)
	req.Header.Set("Cosy-Sign", sig)
}

// computeQoderSignature 复刻 Cosy MD5 签名。
//
//	MD5( base64(payload) + Cosy-Key + Cosy-Date + body + Cosy-SigPath )
//
// 二进制中检查 body 的 isCompleteUTF8；此处保留该信号以便后续按需调整。
func computeQoderSignature(sr signedQoderRequest) string {
	var b strings.Builder
	// payload 已在 buildQoderCosyRequest 中按需 base64；这里再次 base64 以匹配
	// 验证器 "1. Payload (base64)" 的语义（对最终 body 再做 base64）。
	b.WriteString(base64.StdEncoding.EncodeToString(sr.body))
	b.WriteString(sr.key)
	b.WriteString(sr.date)
	b.Write(sr.body)
	b.WriteString(sr.sigPath)
	_ = utf8.Valid(sr.body) // isCompleteUTF8 占位
	sum := md5.Sum([]byte(b.String()))
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
