package test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	openaihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestSubscriptionRoutingHTTP(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
			const model = "gpt-5.4"
			now := time.Now()
			quotaHeaders := func(primary float64, reset time.Time) http.Header {
				return http.Header{
					"X-Codex-Primary-Used-Percent":     {strconv.FormatFloat(primary, 'f', -1, 64)},
					"X-Codex-Primary-Window-Minutes":   {"300"},
					"X-Codex-Primary-Reset-At":         {strconv.FormatInt(now.Add(2*time.Hour).Unix(), 10)},
					"X-Codex-Secondary-Used-Percent":   {"20"},
					"X-Codex-Secondary-Window-Minutes": {"10080"},
					"X-Codex-Secondary-Reset-At":       {strconv.FormatInt(reset.Unix(), 10)},
				}
			}
			var mu sync.Mutex
			var attempts []string
			rejected := make(map[string]bool)
			primaryUsage := map[string]float64{"included-a": 20, "included-b": 20}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				account := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				mu.Lock()
				attempts = append(attempts, account)
				blocked := rejected[account]
				used, included := primaryUsage[account]
				mu.Unlock()
				if blocked {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusTooManyRequests)
					_, errWrite := io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"Included allowance exhausted","resets_in_seconds":3600}}`)
					if errWrite != nil {
						t.Errorf("write quota response: %v", errWrite)
					}
					return
				}
				if included {
					reset := now.Add(24 * time.Hour)
					if account == "included-b" {
						reset = now.Add(48 * time.Hour)
					}
					for name, values := range quotaHeaders(used, reset) {
						w.Header()[name] = values
					}
				}
				w.Header().Set("Content-Type", "text/event-stream")
				response := map[string]any{
					"id": "subscription-routing-response", "model": model, "status": "completed",
					"output": []any{map[string]any{
						"id": "message", "type": "message", "role": "assistant", "status": "completed",
						"content": []any{map[string]any{"type": "output_text", "text": account, "annotations": []any{}}},
					}},
					"usage": map[string]int{"input_tokens": 10, "output_tokens": 1, "total_tokens": 11},
				}
				body, errMarshal := json.Marshal(map[string]any{"type": "response.completed", "response": response})
				if errMarshal != nil {
					t.Errorf("marshal fixture response: %v", errMarshal)
					return
				}
				if _, errWrite := fmt.Fprintf(w, "data: %s\n\n", body); errWrite != nil {
					t.Errorf("write fixture response: %v", errWrite)
				}
			}))
			defer upstream.Close()

			selector := cliproxyauth.NewSessionAffinitySelector(&cliproxyauth.ExpiringFirstSelector{})
			defer selector.Stop()
			manager := cliproxyauth.NewManager(nil, selector, nil)
			manager.SetRetryConfig(0, 0, 3)
			cfg := &config.Config{}
			manager.RegisterExecutor(runtimeexecutor.NewCodexExecutor(cfg))
			ids := map[string]string{}
			for i, account := range []string{"metered", "included-a", "included-b"} {
				id := fmt.Sprintf("subscription-http-%t-%s", streaming, account)
				ids[account] = id
				credential := &cliproxyauth.Auth{
					ID: id, Provider: "codex", Status: cliproxyauth.StatusActive,
					Attributes: map[string]string{"base_url": upstream.URL},
				}
				if account == "metered" {
					credential.Attributes["api_key"] = account
				} else {
					credential.Metadata = map[string]any{"access_token": account, "account_id": account}
					credential.Quota.ObserveResponseHeadersForProvider("codex", quotaHeaders(20, now.Add(time.Duration(i)*24*time.Hour)), now)
				}
				registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
				if _, errRegister := manager.Register(context.Background(), credential); errRegister != nil {
					t.Fatal(errRegister)
				}
			}
			gin.SetMode(gin.TestMode)
			engine := gin.New()
			base := handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager)
			engine.POST("/v1/responses", openaihandlers.NewOpenAIResponsesAPIHandler(base).Responses)
			proxy := httptest.NewServer(engine)
			defer proxy.Close()

			request := func(session, child, want string) {
				t.Helper()
				payload := fmt.Sprintf(`{"model":%q,"input":"hello","stream":%t}`, model, streaming)
				req, errRequest := http.NewRequest(http.MethodPost, proxy.URL+"/v1/responses", strings.NewReader(payload))
				if errRequest != nil {
					t.Fatal(errRequest)
				}
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Claude-Code-Session-Id", session)
				if child != "" {
					req.Header.Set("X-Claude-Code-Agent-Id", child)
				}
				resp, errDo := proxy.Client().Do(req)
				if errDo != nil {
					t.Fatal(errDo)
				}
				body, errRead := io.ReadAll(resp.Body)
				errClose := resp.Body.Close()
				if errRead != nil || errClose != nil {
					t.Fatalf("read response: %v; close: %v", errRead, errClose)
				}
				if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), want) {
					t.Fatalf("response status=%d body=%s, want %s", resp.StatusCode, body, want)
				}
			}
			request("parent", "", "included-a")
			mu.Lock()
			primaryUsage["included-a"] = 98
			mu.Unlock()
			request("parent", "", "included-a")
			pressured, ok := manager.GetByID(ids["included-a"])
			if !ok || pressured.Quota.Signals["X-Codex-Primary-Used-Percent"] != "98" {
				t.Fatal("real upstream quota headers were not observed")
			}
			request("parent", "child", "included-b")
			request("parent", "child", "included-b")
			request("new-session", "", "included-b")
			mu.Lock()
			rejected["included-a"] = true
			mu.Unlock()
			request("parent", "", "included-b")
			mu.Lock()
			for _, account := range attempts {
				if account == "metered" {
					t.Error("metered fallback used while included subscription was available")
				}
			}
			rejected["included-b"] = true
			mu.Unlock()
			request("parent", "", "metered")
			mu.Lock()
			defer mu.Unlock()
			if attempts[len(attempts)-1] != "metered" {
				t.Fatal("metered fallback was not used after both subscriptions exhausted")
			}
			t.Logf("routing acceptance: healthy parent/child stayed pinned; metered calls only after both included accounts exhausted; upstream attempts=%d", len(attempts))
		})
	}
}
