package proxy

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ttimasdf/qoder2api/auth"
)

func newQoderTestAccount() *auth.Account {
	return &auth.Account{
		DBID:              7,
		UpstreamType:      auth.UpstreamQoder,
		AccessToken:       "at-test-token",
		QoderUserID:       "user-1",
		QoderOrgID:        "org-1",
		QoderMachineID:    "machine-1",
		QoderMachineToken: "mt-1",
		QoderClientVer:    "2.6.0",
	}
}

func TestComputeQoderSignatureDeterministic(t *testing.T) {
	sr := signedQoderRequest{
		sigPath: "/algo/api/v2/service/invoke/choose_model",
		body:    []byte(`{"model":"qwen3-coder"}`),
		date:    "1700000000000",
		key:     "fixed-key",
	}
	got := computeQoderSignature(sr)

	var b strings.Builder
	b.WriteString(base64.StdEncoding.EncodeToString(sr.body))
	b.WriteString(sr.key)
	b.WriteString(sr.date)
	b.Write(sr.body)
	b.WriteString(sr.sigPath)
	sum := md5.Sum([]byte(b.String()))
	want := hex.EncodeToString(sum[:])

	if got != want {
		t.Fatalf("signature = %q, want %q", got, want)
	}
}

func TestBuildQoderCosyRequestSetsHeaders(t *testing.T) {
	acc := newQoderTestAccount()
	req, err := buildQoderCosyRequest(context.Background(), acc, http.MethodPost,
		"https://example.test/algo/api/v2/service/invoke/choose_model?Encode=1", []byte(`{"model":"x"}`))
	if err != nil {
		t.Fatalf("buildQoderCosyRequest: %v", err)
	}
	for _, h := range []string{"Authorization", "Cosy-User", "Cosy-MachineToken", "Cosy-Date", "Cosy-Key", "Cosy-SigPath", "Cosy-BodyHash", "Cosy-Sign", "X-Request-ID"} {
		if req.Header.Get(h) == "" {
			t.Errorf("missing header %s", h)
		}
	}
	if got := req.Header.Get("Authorization"); got != "Bearer at-test-token" {
		t.Errorf("Authorization = %q", got)
	}
	if got := req.Header.Get("Cosy-SigPath"); got != "/algo/api/v2/service/invoke/choose_model" {
		t.Errorf("Cosy-SigPath = %q (query should be trimmed)", got)
	}
}

func TestExecuteQoderRequestRoutesThroughChooseModel(t *testing.T) {
	var chooseHit, chatHit bool

	// 推理节点
	infer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatHit = true
		if r.URL.Path != "/chat/completions" {
			t.Errorf("inference path = %s", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "qwen-max-routed") {
			t.Errorf("model not rewritten from choose_model: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`))
	}))
	defer infer.Close()

	// big-model：choose_model
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chooseHit = true
		if !strings.HasSuffix(r.URL.Path, "/choose_model") {
			t.Errorf("choose_model path = %s", r.URL.Path)
		}
		if r.Header.Get("Cosy-Sign") == "" {
			t.Error("choose_model missing Cosy-Sign")
		}
		resp := qoderChooseModelResponse{ModelName: "qwen-max-routed", Endpoint: infer.URL, Token: "route-token"}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer big.Close()

	prev := qoderBigModelBaseOverride
	qoderBigModelBaseOverride = big.URL
	defer func() { qoderBigModelBaseOverride = prev }()

	acc := newQoderTestAccount()
	resp, err := ExecuteQoderRequest(context.Background(), acc, []byte(`{"model":"qwen-max","messages":[]}`), "", nil)
	if err != nil {
		t.Fatalf("ExecuteQoderRequest: %v", err)
	}
	defer resp.Body.Close()
	if !chooseHit {
		t.Error("choose_model not called")
	}
	if !chatHit {
		t.Error("inference chat/completions not called")
	}
	body, _ := io.ReadAll(resp.Body)
	if usage := extractUsageFromChatCompletion(body); usage == nil || usage.TotalTokens != 5 {
		t.Errorf("usage parse failed: %+v", usage)
	}
}
