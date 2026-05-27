package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func TestShouldMarkBatchTestAccountError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       []byte
		want       bool
	}{
		{
			name:       "forbidden is account scoped",
			statusCode: http.StatusForbidden,
			body:       []byte(`{"error":{"code":"unsupported_country_region_territory"}}`),
			want:       true,
		},
		{
			name:       "payment required deactivated workspace is account scoped",
			statusCode: http.StatusPaymentRequired,
			body:       []byte(`{"detail":{"code":"deactivated_workspace"}}`),
			want:       true,
		},
		{
			name:       "invalid grant bad request is account scoped",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"error":"invalid_grant"}`),
			want:       true,
		},
		{
			name:       "model version bad request is global",
			statusCode: http.StatusBadRequest,
			body:       []byte(`{"detail":"The 'gpt-5.5' model requires a newer version of Codex"}`),
			want:       false,
		},
		{
			name:       "server error is not marked as account error",
			statusCode: http.StatusBadGateway,
			body:       []byte(`bad gateway`),
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldMarkBatchTestAccountError(tt.statusCode, tt.body); got != tt.want {
				t.Fatalf("shouldMarkBatchTestAccountError() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolveBatchTestAccountsDefaultsToAllAccounts(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "token-1", Status: auth.StatusReady})
	store.AddAccount(&auth.Account{DBID: 2, AccessToken: "token-2", Status: auth.StatusReady})

	accounts, missing := resolveBatchTestAccounts(store, nil)
	if missing != 0 {
		t.Fatalf("missing = %d, want 0", missing)
	}
	if len(accounts) != 2 {
		t.Fatalf("len(accounts) = %d, want 2", len(accounts))
	}
}

func TestResolveBatchTestAccountsUsesSelectedIDs(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "token-1", Status: auth.StatusReady})
	store.AddAccount(&auth.Account{DBID: 2, AccessToken: "token-2", Status: auth.StatusReady})

	ids := []int64{2, 99, 2, 1}
	accounts, missing := resolveBatchTestAccounts(store, &ids)
	if missing != 1 {
		t.Fatalf("missing = %d, want 1", missing)
	}
	if len(accounts) != 2 {
		t.Fatalf("len(accounts) = %d, want 2", len(accounts))
	}
	if accounts[0].DBID != 2 || accounts[1].DBID != 1 {
		t.Fatalf("account order = [%d, %d], want [2, 1]", accounts[0].DBID, accounts[1].DBID)
	}
}

func TestRunSingleBatchTestTimesOutSlowStreamingBody(t *testing.T) {
	previousTimeout := batchTestAccountTimeout
	batchTestAccountTimeout = 20 * time.Millisecond
	t.Cleanup(func() {
		batchTestAccountTimeout = previousTimeout
	})

	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-started:
		default:
			close(started)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
		time.Sleep(50 * time.Millisecond)
	}))
	defer server.Close()

	store := auth.NewStore(nil, nil, nil)
	account := &auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      server.URL,
		APIKey:       "test-key",
		Models:       []string{"gpt-4o-mini"},
		Status:       auth.StatusReady,
		HealthTier:   auth.HealthTierHealthy,
	}
	store.AddAccount(account)
	handler := &Handler{store: store}

	start := time.Now()
	status, msg := handler.runSingleBatchTest(context.Background(), account)
	elapsed := time.Since(start)

	if status != "failed" {
		t.Fatalf("status = %q, want failed", status)
	}
	if !strings.Contains(msg, "测试超时") {
		t.Fatalf("message = %q, want timeout message", msg)
	}
	if elapsed > time.Second {
		t.Fatalf("batch test took %s, want bounded timeout", elapsed)
	}
	if account.Status == auth.StatusError {
		t.Fatal("timeout should not mark account as permanent error")
	}
	if account.LastTimeoutAt.IsZero() {
		t.Fatal("timeout should be recorded in scheduler health")
	}
}
