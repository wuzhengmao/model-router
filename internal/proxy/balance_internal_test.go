package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"model-router/internal/config"
)

// balBackend is a fake balance endpoint of §24.31.
type balBackend struct {
	server *httptest.Server

	mu       sync.Mutex
	payload  string
	status   int
	requests int
	auth     []string
}

func newBalBackend(t *testing.T, payload string) *balBackend {
	t.Helper()
	b := &balBackend{payload: payload, status: http.StatusOK}
	b.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.requests++
		b.auth = append(b.auth, r.Header.Get("Authorization"))
		payload := b.payload
		status := b.status
		b.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, payload)
	}))
	t.Cleanup(b.server.Close)
	return b
}

func (b *balBackend) set(payload string, status int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.payload = payload
	b.status = status
}

func (b *balBackend) calls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.requests
}

func (b *balBackend) lastAuth() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.auth) == 0 {
		return ""
	}
	return b.auth[len(b.auth)-1]
}

// balNoPoll disables the periodic poll so only the startup check and the
// manual refresh query the endpoint.
func balNoPoll() *config.OptionalDuration {
	value := config.OptionalDuration(0)
	return &value
}

// balPoll sets an explicit polling period.
func balPoll(d time.Duration) *config.OptionalDuration {
	value := config.OptionalDuration(d)
	return &value
}

// balInstance returns the runtime of a configured instance.
func balInstance(t *testing.T, server *Server, proto protocol, name string) *instanceRuntime {
	t.Helper()
	for _, inst := range server.state.Load().handler.byProtocol[proto] {
		if inst.name == name {
			return inst
		}
	}
	t.Fatalf("instance %s not found", name)
	return nil
}

// balWaitState waits until the instance reaches the wanted state.
func balWaitState(t *testing.T, inst *instanceRuntime, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if inst.state() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("instance %s state = %q, want %q", inst.name, inst.state(), want)
}

// balWaitChecked waits until the instance has a successful balance snapshot.
func balWaitChecked(t *testing.T, inst *instanceRuntime) balanceInfo {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		info, ok := inst.balanceSnapshot()
		if ok && info.CheckedAt > 0 {
			return info
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("instance %s has no balance snapshot yet", inst.name)
	return balanceInfo{}
}

// balRefresh calls POST /api/instances/balance and returns the decoded body.
func balRefresh(t *testing.T, serverURL, body string, auth bool) (int, balanceRefreshResponse, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, serverURL+PathInstancesBalance, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build balance request: %v", err)
	}
	if auth {
		req.SetBasicAuth("admin", "secret")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST balance: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var payload balanceRefreshResponse
	_ = json.Unmarshal(raw, &payload)
	return resp.StatusCode, payload, string(raw)
}

// balPost sends a chat completion and returns the status and the body.
func balPost(t *testing.T, serverURL, model string) (int, string) {
	t.Helper()
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequest(http.MethodPost, serverURL+PathOpenAI, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// balStatsRow mirrors the instance shape of GET /api/stats needed by §24.31.
type balStatsRow struct {
	Name          string           `json:"name"`
	Protocol      string           `json:"protocol"`
	Requests      int64            `json:"requests"`
	State         string           `json:"state"`
	Reason        string           `json:"reason"`
	DegradedSince int64            `json:"degradedSince"`
	Balance       *json.RawMessage `json:"balance"`
}

// balGetStats reads GET /api/stats.
func balGetStats(t *testing.T, serverURL string) []balStatsRow {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, serverURL+PathStats, nil)
	if err != nil {
		t.Fatalf("build stats request: %v", err)
	}
	req.SetBasicAuth("admin", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/stats: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/stats = %d, want 200", resp.StatusCode)
	}
	var payload struct {
		Instances []balStatsRow `json:"instances"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	return payload.Instances
}

// balRow finds one instance row of the stats payload.
func balRow(rows []balStatsRow, name string) *balStatsRow {
	for i := range rows {
		if rows[i].Name == name {
			return &rows[i]
		}
	}
	return nil
}

// balRecentError is one entry of the recentErrors array of GET /api/stats.
type balRecentError struct {
	InstName string `json:"instName"`
	Error    string `json:"error"`
	Status   int    `json:"status"`
}

// balGetRecentErrors reads the recentErrors array of GET /api/stats.
func balGetRecentErrors(t *testing.T, serverURL string) []balRecentError {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, serverURL+PathStats, nil)
	if err != nil {
		t.Fatalf("build stats request: %v", err)
	}
	req.SetBasicAuth("admin", "secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/stats: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/stats = %d, want 200", resp.StatusCode)
	}
	var payload struct {
		RecentErrors []balRecentError `json:"recentErrors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	return payload.RecentErrors
}

// balServer starts a Server for cfg on a local HTTP listener.
func balServer(t *testing.T, cfg *config.Config) (*Server, *httptest.Server) {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.json")
	server, err := NewServer(configPath, cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	return server, httpServer
}

// TestBalanceParsesMiniMaxPlan covers §24.31 M155/M156: the remaining
// percentages of the "general" model are read and any empty window counts as
// exhausted.
func TestBalanceParsesMiniMaxPlan(t *testing.T) {
	cases := []struct {
		name          string
		payload       string
		want5h        int
		want7d        int
		wantExhausted bool
		wantNote      bool
	}{
		{
			name:          "both windows left",
			payload:       `{"model_remains":[{"model_name":"general","current_interval_remaining_percent":90,"current_weekly_remaining_percent":97}]}`,
			want5h:        90,
			want7d:        97,
			wantExhausted: false,
		},
		{
			name:          "interval window empty",
			payload:       `{"model_remains":[{"model_name":"general","current_interval_remaining_percent":0,"current_weekly_remaining_percent":97}]}`,
			want5h:        0,
			want7d:        97,
			wantExhausted: true,
		},
		{
			name:          "weekly window empty",
			payload:       `{"model_remains":[{"model_name":"general","current_interval_remaining_percent":90,"current_weekly_remaining_percent":0}]}`,
			want5h:        90,
			want7d:        0,
			wantExhausted: true,
		},
		{
			name:          "other models are ignored",
			payload:       `{"model_remains":[{"model_name":"other","current_interval_remaining_percent":1,"current_weekly_remaining_percent":1},{"model_name":"general","current_interval_remaining_percent":50,"current_weekly_remaining_percent":60}]}`,
			want5h:        50,
			want7d:        60,
			wantExhausted: false,
		},
		{
			name:          "missing percentages are unknown, never exhausted",
			payload:       `{"model_remains":[{"model_name":"general"}]}`,
			want5h:        unknownPercent,
			want7d:        unknownPercent,
			wantExhausted: false,
			wantNote:      true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := balanceInfo{Kind: config.BalanceKindMiniMaxPlan, Interval5hPercent: unknownPercent, Weekly7dPercent: unknownPercent}
			note, err := parseMiniMaxPlan([]byte(tc.payload), &info)
			if err != nil {
				t.Fatalf("parseMiniMaxPlan: %v", err)
			}
			if info.Interval5hPercent != tc.want5h || info.Weekly7dPercent != tc.want7d {
				t.Errorf("percentages = %d/%d, want %d/%d",
					info.Interval5hPercent, info.Weekly7dPercent, tc.want5h, tc.want7d)
			}
			if info.Exhausted != tc.wantExhausted {
				t.Errorf("exhausted = %v, want %v", info.Exhausted, tc.wantExhausted)
			}
			if (note != "") != tc.wantNote {
				t.Errorf("note = %q, want a note: %v", note, tc.wantNote)
			}
		})
	}
}

// TestBalanceParsesDeepSeek covers §24.31 M155/M156: CNY wins over other
// currencies and an unavailable or empty account is exhausted.
func TestBalanceParsesDeepSeek(t *testing.T) {
	cases := []struct {
		name          string
		payload       string
		wantAmount    string
		wantCurrency  string
		wantExhausted bool
		wantNote      bool
	}{
		{
			name:          "cny is preferred",
			payload:       `{"is_available":true,"balance_infos":[{"currency":"USD","total_balance":"1.00"},{"currency":"CNY","total_balance":"51.01"}]}`,
			wantAmount:    "51.01",
			wantCurrency:  "CNY",
			wantExhausted: false,
		},
		{
			name:          "first entry when there is no cny",
			payload:       `{"is_available":true,"balance_infos":[{"currency":"USD","total_balance":"1.00"}]}`,
			wantAmount:    "1.00",
			wantCurrency:  "USD",
			wantExhausted: false,
		},
		{
			name:          "not available is exhausted",
			payload:       `{"is_available":false,"balance_infos":[{"currency":"CNY","total_balance":"51.01"}]}`,
			wantAmount:    "51.01",
			wantCurrency:  "CNY",
			wantExhausted: true,
		},
		{
			name:          "zero balance is exhausted",
			payload:       `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"0.00"}]}`,
			wantAmount:    "0.00",
			wantCurrency:  "CNY",
			wantExhausted: true,
		},
		{
			name:          "numeric amounts are accepted",
			payload:       `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":51.01}]}`,
			wantAmount:    "51.01",
			wantCurrency:  "CNY",
			wantExhausted: false,
		},
		{
			name:          "empty balance infos are unknown",
			payload:       `{"is_available":true,"balance_infos":[]}`,
			wantAmount:    "",
			wantCurrency:  "",
			wantExhausted: false,
			wantNote:      true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			info := balanceInfo{Kind: config.BalanceKindDeepSeek}
			note, err := parseDeepSeekBalance([]byte(tc.payload), &info)
			if err != nil {
				t.Fatalf("parseDeepSeekBalance: %v", err)
			}
			if info.Amount != tc.wantAmount {
				t.Errorf("amount = %q, want %q", info.Amount, tc.wantAmount)
			}
			if info.Currency != tc.wantCurrency {
				t.Errorf("currency = %q, want %q", info.Currency, tc.wantCurrency)
			}
			if info.Exhausted != tc.wantExhausted {
				t.Errorf("exhausted = %v, want %v", info.Exhausted, tc.wantExhausted)
			}
			if (note != "") != tc.wantNote {
				t.Errorf("note = %q, want a note: %v", note, tc.wantNote)
			}
		})
	}
}

// TestBalanceConfigParsing covers §24.31 M153: the balance object is strict
// and the kind is checked.
func TestBalanceConfigParsing(t *testing.T) {
	t.Run("known kinds are accepted", func(t *testing.T) {
		for _, kind := range []string{config.BalanceKindMiniMaxPlan, config.BalanceKindDeepSeek} {
			cfg, err := config.Parse([]byte(`{"auth_token":"t","instances":{"openai":[{"name":"a","base_url":"http://127.0.0.1:1","api_key":"k","models":["m"],"balance":{"kind":"` + kind + `"}}]}}`))
			if err != nil {
				t.Fatalf("Parse(%s): %v", kind, err)
			}
			if cfg.Instances[0].Balance == nil || cfg.Instances[0].Balance.Kind != kind {
				t.Fatalf("balance = %+v, want kind %q", cfg.Instances[0].Balance, kind)
			}
			if got := cfg.Instances[0].Balance.PollInterval(); got != config.DefaultBalanceInterval {
				t.Errorf("PollInterval = %s, want the default %s", got, config.DefaultBalanceInterval)
			}
		}
	})
	t.Run("unknown kind is rejected", func(t *testing.T) {
		_, err := config.Parse([]byte(`{"auth_token":"t","instances":{"openai":[{"name":"a","base_url":"http://127.0.0.1:1","api_key":"k","models":["m"],"balance":{"kind":"nope"}}]}}`))
		if err == nil || !strings.Contains(err.Error(), "balance.kind") {
			t.Fatalf("Parse error = %v, want a balance.kind error", err)
		}
	})
	t.Run("unknown field is rejected", func(t *testing.T) {
		_, err := config.Parse([]byte(`{"auth_token":"t","instances":{"openai":[{"name":"a","base_url":"http://127.0.0.1:1","api_key":"k","models":["m"],"balance":{"kind":"deepseek","nope":1}}]}}`))
		if err == nil || !strings.Contains(err.Error(), "nope") {
			t.Fatalf("Parse error = %v, want an unknown field error", err)
		}
	})
	t.Run("interval zero disables the poll", func(t *testing.T) {
		cfg, err := config.Parse([]byte(`{"auth_token":"t","instances":{"openai":[{"name":"a","base_url":"http://127.0.0.1:1","api_key":"k","models":["m"],"balance":{"kind":"deepseek","interval":"0s"}}]}}`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if got := cfg.Instances[0].Balance.PollInterval(); got != 0 {
			t.Errorf("PollInterval = %s, want 0", got)
		}
	})
	t.Run("an instance without balance is untouched", func(t *testing.T) {
		cfg, err := config.Parse([]byte(`{"auth_token":"t","instances":{"openai":[{"name":"a","base_url":"http://127.0.0.1:1","api_key":"k","models":["m"]}]}}`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if cfg.Instances[0].Balance != nil {
			t.Fatalf("balance = %+v, want none", cfg.Instances[0].Balance)
		}
	})
}

// TestBalanceStartupCheck covers §24.31 M157: the first check runs right
// after the startup and the state follows the quota.
func TestBalanceStartupCheck(t *testing.T) {
	backend := newBalBackend(t, `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"51.01"}]}`)
	cfg := &config.Config{
		AuthToken:     "tok",
		AdminUser:     "admin",
		AdminPassword: "secret",
		Instances: config.Instances{{
			Protocol: config.ProtocolOpenAI,
			Name:     "deepseek",
			BaseURL:  "http://127.0.0.1:1",
			APIKey:   "sk-test",
			Timeout:  config.Duration(2 * time.Second),
			Models:   []string{"deepseek-chat"},
			Balance: &config.Balance{
				Kind:     config.BalanceKindDeepSeek,
				URL:      backend.server.URL,
				Interval: balNoPoll(),
			},
		}},
	}
	server, httpServer := balServer(t, cfg)
	inst := balInstance(t, server, protocolOpenAI, "deepseek")
	info := balWaitChecked(t, inst)

	if info.Amount != "51.01" || info.Currency != "CNY" {
		t.Errorf("balance = %+v, want 51.01 CNY", info)
	}
	if info.CheckedAt <= 0 {
		t.Errorf("checkedAt = %d, want a timestamp", info.CheckedAt)
	}
	if want := "Bearer sk-test"; backend.lastAuth() != want {
		t.Errorf("Authorization = %q, want %q", backend.lastAuth(), want)
	}

	rows := balGetStats(t, httpServer.URL)
	row := balRow(rows, "deepseek")
	if row == nil || row.Balance == nil {
		t.Fatalf("stats row = %+v, want a balance field", row)
	}
	var snapshot balanceInfo
	if err := json.Unmarshal(*row.Balance, &snapshot); err != nil {
		t.Fatalf("decode balance: %v", err)
	}
	if snapshot.Amount != "51.01" || snapshot.Currency != "CNY" {
		t.Errorf("stats balance = %+v, want 51.01 CNY", snapshot)
	}
}

// TestBalanceStatsOmitsFieldWithoutBalance covers §24.31 M165/M15fx: an
// instance without a balance check carries no balance field.
func TestBalanceStatsOmitsFieldWithoutBalance(t *testing.T) {
	backend := newCBBackend(t, http.StatusOK)
	cfg := cbStatsTestConfig(backend.server.URL, backend.server.URL)
	server, httpServer := balServer(t, cfg)
	_ = server

	rows := balGetStats(t, httpServer.URL)
	if len(rows) == 0 {
		t.Fatal("no instance rows")
	}
	for _, row := range rows {
		if row.Balance != nil {
			t.Errorf("row %s carries a balance field, want none", row.Name)
		}
	}
}

// TestBalanceCheckFailureKeepsState covers §24.31 M158/M15gd: a failed query
// keeps the last successful values and the exhausted state.
func TestBalanceCheckFailureKeepsState(t *testing.T) {
	backend := newBalBackend(t, `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"0.00"}]}`)
	cfg := &config.Config{
		AuthToken:     "tok",
		AdminUser:     "admin",
		AdminPassword: "secret",
		Instances: config.Instances{{
			Protocol: config.ProtocolOpenAI,
			Name:     "deepseek",
			BaseURL:  "http://127.0.0.1:1",
			APIKey:   "sk-test",
			Timeout:  config.Duration(2 * time.Second),
			Models:   []string{"deepseek-chat"},
			Balance: &config.Balance{
				Kind:     config.BalanceKindDeepSeek,
				URL:      backend.server.URL,
				Interval: balNoPoll(),
			},
		}},
	}
	server, httpServer := balServer(t, cfg)
	inst := balInstance(t, server, protocolOpenAI, "deepseek")
	balWaitState(t, inst, stateExhausted)

	before, _ := inst.balanceSnapshot()
	backend.set("not json at all", http.StatusInternalServerError)

	code, payload, _ := balRefresh(t, httpServer.URL, `{"protocol":"openai","name":"deepseek"}`, true)
	if code != http.StatusOK {
		t.Fatalf("POST balance = %d, want 200", code)
	}
	if len(payload.Instances) != 1 {
		t.Fatalf("instances = %+v, want one row", payload.Instances)
	}
	if payload.Instances[0].State != stateExhausted {
		t.Errorf("state = %q, want %q", payload.Instances[0].State, stateExhausted)
	}
	if payload.Instances[0].Balance.Err == "" {
		t.Error("balance.err is empty, want the failure reason")
	}
	after, _ := inst.balanceSnapshot()
	if after.Amount != before.Amount || after.Currency != before.Currency {
		t.Errorf("balance = %+v, want the previous values %+v", after, before)
	}
	if after.CheckedAt != before.CheckedAt {
		t.Errorf("checkedAt = %d, want the previous timestamp %d", after.CheckedAt, before.CheckedAt)
	}
	if inst.state() != stateExhausted {
		t.Errorf("state = %q, want %q", inst.state(), stateExhausted)
	}
}

// TestExhaustedInstanceIsSkipped covers §24.31 M161/M15ga: an exhausted
// instance never receives a request.
func TestExhaustedInstanceIsSkipped(t *testing.T) {
	upstream := newCBBackend(t, http.StatusOK)
	balance := newBalBackend(t, `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"0.00"}]}`)
	cfg := &config.Config{
		AuthToken:     "tok",
		AdminUser:     "admin",
		AdminPassword: "secret",
		Instances: config.Instances{
			{
				Protocol: config.ProtocolOpenAI,
				Name:     "exhausted",
				BaseURL:  upstream.server.URL,
				APIKey:   "k1",
				Timeout:  config.Duration(2 * time.Second),
				Models:   []string{"model-a"},
				Balance: &config.Balance{
					Kind:     config.BalanceKindDeepSeek,
					URL:      balance.server.URL,
					Interval: balNoPoll(),
				},
			},
			{
				Protocol: config.ProtocolOpenAI,
				Name:     "healthy",
				BaseURL:  upstream.server.URL,
				APIKey:   "k2",
				Timeout:  config.Duration(2 * time.Second),
				Models:   []string{"model-b"},
			},
		},
	}
	server, httpServer := balServer(t, cfg)
	inst := balInstance(t, server, protocolOpenAI, "exhausted")
	balWaitState(t, inst, stateExhausted)

	if code, _ := balPost(t, httpServer.URL, "model-a"); code != http.StatusOK {
		t.Fatalf("request status = %d, want 200", code)
	}
	rows := balGetStats(t, httpServer.URL)
	exhausted := balRow(rows, "exhausted")
	healthy := balRow(rows, "healthy")
	if exhausted == nil || exhausted.State != stateExhausted {
		t.Fatalf("exhausted row = %+v, want state exhausted", exhausted)
	}
	if exhausted.Reason != reasonQuotaExhausted {
		t.Errorf("reason = %q, want %q", exhausted.Reason, reasonQuotaExhausted)
	}
	if exhausted.DegradedSince <= 0 {
		t.Errorf("degradedSince = %d, want a timestamp", exhausted.DegradedSince)
	}
	if healthy == nil || healthy.Requests != 1 {
		t.Errorf("healthy row = %+v, want one request", healthy)
	}
}

// TestAllExhaustedReturns502 covers §24.31 M162/M15gc: a chain whose
// candidates all ran out of quota answers 502.
func TestAllExhaustedReturns502(t *testing.T) {
	upstream := newCBBackend(t, http.StatusOK)
	balance := newBalBackend(t, `{"is_available":false,"balance_infos":[{"currency":"CNY","total_balance":"0.00"}]}`)
	cfg := &config.Config{
		AuthToken:     "tok",
		AdminUser:     "admin",
		AdminPassword: "secret",
		Instances: config.Instances{
			{
				Protocol: config.ProtocolOpenAI,
				Name:     "quota-a",
				BaseURL:  upstream.server.URL,
				APIKey:   "k1",
				Timeout:  config.Duration(2 * time.Second),
				Models:   []string{"model-a"},
				Balance: &config.Balance{
					Kind:     config.BalanceKindDeepSeek,
					URL:      balance.server.URL,
					Interval: balNoPoll(),
				},
			},
			{
				Protocol: config.ProtocolOpenAI,
				Name:     "quota-b",
				BaseURL:  upstream.server.URL,
				APIKey:   "k2",
				Timeout:  config.Duration(2 * time.Second),
				Models:   []string{"model-b"},
				Balance: &config.Balance{
					Kind:     config.BalanceKindDeepSeek,
					URL:      balance.server.URL,
					Interval: balNoPoll(),
				},
			},
		},
	}
	server, httpServer := balServer(t, cfg)
	for _, name := range []string{"quota-a", "quota-b"} {
		balWaitState(t, balInstance(t, server, protocolOpenAI, name), stateExhausted)
	}

	code, body := balPost(t, httpServer.URL, "model-a")
	if code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", code)
	}
	if !strings.Contains(body, errAllQuotasExhausted) {
		t.Errorf("body = %q, want it to contain %q", body, errAllQuotasExhausted)
	}
	if !strings.Contains(body, "quota-a") {
		t.Errorf("body = %q, want it to name the exhausted instances", body)
	}
	if upstream.calls() != 0 {
		t.Errorf("upstream calls = %d, want 0", upstream.calls())
	}

	snapshot := server.monitor.stats.snapshot(0, server.startedAt)
	if len(snapshot.RecentErrors) == 0 {
		t.Fatal("recentErrors is empty, want the exhausted request")
	}
	entry := snapshot.RecentErrors[0]
	if entry.Error != errAllQuotasExhausted {
		t.Errorf("recentErrors error = %q, want %q", entry.Error, errAllQuotasExhausted)
	}
	if entry.InstName != "quota-a" && entry.InstName != "quota-b" {
		t.Errorf("recentErrors instName = %q, want an exhausted instance", entry.InstName)
	}

	// §24.31 M162: the same record is served by GET /api/stats.
	errors := balGetRecentErrors(t, httpServer.URL)
	if len(errors) == 0 {
		t.Fatal("/api/stats recentErrors is empty, want the exhausted request")
	}
	found := false
	for _, item := range errors {
		if item.Error == errAllQuotasExhausted &&
			(item.InstName == "quota-a" || item.InstName == "quota-b") {
			found = true
		}
	}
	if !found {
		t.Errorf("/api/stats recentErrors = %+v, want an exhausted instance with %q", errors, errAllQuotasExhausted)
	}
}

// TestBalanceRefreshRestoresInstance covers §24.31 M160/M15gb: a quota that
// is back restores the instance.
func TestBalanceRefreshRestoresInstance(t *testing.T) {
	upstream := newCBBackend(t, http.StatusOK)
	balance := newBalBackend(t, `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"0.00"}]}`)
	cfg := &config.Config{
		AuthToken:     "tok",
		AdminUser:     "admin",
		AdminPassword: "secret",
		Instances: config.Instances{{
			Protocol: config.ProtocolOpenAI,
			Name:     "restored",
			BaseURL:  upstream.server.URL,
			APIKey:   "k1",
			Timeout:  config.Duration(2 * time.Second),
			Models:   []string{"model-a"},
			Balance: &config.Balance{
				Kind:     config.BalanceKindDeepSeek,
				URL:      balance.server.URL,
				Interval: balNoPoll(),
			},
		}},
	}
	server, httpServer := balServer(t, cfg)
	inst := balInstance(t, server, protocolOpenAI, "restored")
	balWaitState(t, inst, stateExhausted)

	balance.set(`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"12.34"}]}`, http.StatusOK)
	code, payload, _ := balRefresh(t, httpServer.URL, `{"protocol":"openai","name":"restored"}`, true)
	if code != http.StatusOK {
		t.Fatalf("POST balance = %d, want 200", code)
	}
	if len(payload.Instances) != 1 || payload.Instances[0].State != stateClosed {
		t.Fatalf("instances = %+v, want one closed row", payload.Instances)
	}
	if payload.Instances[0].Balance.Amount != "12.34" {
		t.Errorf("amount = %q, want 12.34", payload.Instances[0].Balance.Amount)
	}
	if inst.state() != stateClosed {
		t.Errorf("state = %q, want %q", inst.state(), stateClosed)
	}
	if code, _ := balPost(t, httpServer.URL, "model-a"); code != http.StatusOK {
		t.Fatalf("request status = %d, want 200", code)
	}
}

// TestBalanceRefreshAPI covers §24.31 M159/M15gf: the endpoint validates its
// input and needs the admin authentication.
func TestBalanceRefreshAPI(t *testing.T) {
	upstream := newCBBackend(t, http.StatusOK)
	balance := newBalBackend(t, `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"51.01"}]}`)
	cfg := &config.Config{
		AuthToken:     "tok",
		AdminUser:     "admin",
		AdminPassword: "secret",
		Instances: config.Instances{
			{
				Protocol: config.ProtocolOpenAI,
				Name:     "checked",
				BaseURL:  upstream.server.URL,
				APIKey:   "k1",
				Timeout:  config.Duration(2 * time.Second),
				Models:   []string{"model-a"},
				Balance: &config.Balance{
					Kind:     config.BalanceKindDeepSeek,
					URL:      balance.server.URL,
					Interval: balNoPoll(),
				},
			},
			{
				Protocol: config.ProtocolOpenAI,
				Name:     "plain",
				BaseURL:  upstream.server.URL,
				APIKey:   "k2",
				Timeout:  config.Duration(2 * time.Second),
				Models:   []string{"model-b"},
			},
		},
	}
	_, httpServer := balServer(t, cfg)

	t.Run("refreshes one instance", func(t *testing.T) {
		code, payload, _ := balRefresh(t, httpServer.URL, `{"protocol":"openai","name":"checked"}`, true)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		if len(payload.Instances) != 1 || payload.Instances[0].Name != "checked" {
			t.Fatalf("instances = %+v, want the checked instance", payload.Instances)
		}
		if payload.Instances[0].Kind != config.BalanceKindDeepSeek {
			t.Errorf("kind = %q, want %q", payload.Instances[0].Kind, config.BalanceKindDeepSeek)
		}
		if payload.Instances[0].Protocol != config.ProtocolOpenAI {
			t.Errorf("protocol = %q, want openai", payload.Instances[0].Protocol)
		}
	})

	t.Run("refreshes every instance of the protocol", func(t *testing.T) {
		code, payload, _ := balRefresh(t, httpServer.URL, `{"protocol":"openai"}`, true)
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		if len(payload.Instances) != 1 || payload.Instances[0].Name != "checked" {
			t.Fatalf("instances = %+v, want only the instance with a balance", payload.Instances)
		}
	})

	t.Run("rejects an unknown protocol", func(t *testing.T) {
		code, _, _ := balRefresh(t, httpServer.URL, `{"protocol":"gemini"}`, true)
		if code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", code)
		}
	})

	t.Run("rejects an unknown instance", func(t *testing.T) {
		code, _, _ := balRefresh(t, httpServer.URL, `{"protocol":"openai","name":"missing"}`, true)
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", code)
		}
	})

	t.Run("rejects an instance without a balance check", func(t *testing.T) {
		code, _, _ := balRefresh(t, httpServer.URL, `{"protocol":"openai","name":"plain"}`, true)
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", code)
		}
	})

	t.Run("requires the admin authentication", func(t *testing.T) {
		code, _, _ := balRefresh(t, httpServer.URL, `{"protocol":"openai"}`, false)
		if code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", code)
		}
	})
}

// TestBalanceUI covers §24.31 M167/M168/M169/M15gg: both instance tables
// carry a balance column in front of the state column and the cell refreshes
// the balance.
func TestBalanceUI(t *testing.T) {
	data, err := os.ReadFile("../web/index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	html := string(data)
	for _, want := range []string{
		"<th>余额</th>",
		"stat-balance-clickable",
		"点击刷新余额",
		"/api/instances/balance",
		"刷新中…",
		"配额耗尽",
		"stat-exhausted",
		// §24.31 M171: the visual page has no balance editor, so it must
		// carry the object through instead of dropping it on save.
		"raw.balance",
		"item.balance",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html missing %q", want)
		}
	}
	// The balance column sits in front of the state column in both tables.
	for _, id := range []string{"stat-instances-openai-table", "stat-instances-anthropic-table"} {
		table := strings.Index(html, `id="`+id+`"`)
		if table < 0 {
			t.Fatalf("table %s missing", id)
		}
		rest := html[table:]
		balance := strings.Index(rest, "<th>余额</th>")
		state := strings.Index(rest, "<th>状态</th>")
		if balance < 0 || state < 0 || balance > state {
			t.Errorf("table %s: the balance column must precede the state column", id)
		}
	}
}

// TestBalanceNotPersisted covers §24.31 M166/M15gi: the balance never lands in
// stats.json.
func TestBalanceNotPersisted(t *testing.T) {
	upstream := newCBBackend(t, http.StatusOK)
	balance := newBalBackend(t, `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"51.01"}]}`)
	configPath := filepath.Join(t.TempDir(), "config.json")
	cfg := &config.Config{
		AuthToken:     "tok",
		AdminUser:     "admin",
		AdminPassword: "secret",
		Instances: config.Instances{{
			Protocol: config.ProtocolOpenAI,
			Name:     "checked",
			BaseURL:  upstream.server.URL,
			APIKey:   "k1",
			Timeout:  config.Duration(2 * time.Second),
			Models:   []string{"model-a"},
			Balance: &config.Balance{
				Kind:     config.BalanceKindDeepSeek,
				URL:      balance.server.URL,
				Interval: balNoPoll(),
			},
		}},
	}
	server, err := NewServer(configPath, cfg)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(server.Close)
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)

	if code, _ := balPost(t, httpServer.URL, "model-a"); code != http.StatusOK {
		t.Fatalf("request status = %d, want 200", code)
	}
	server.monitor.stats.flush(true)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(configPath), statsFileName))
	if err != nil {
		t.Fatalf("read stats.json: %v", err)
	}
	if strings.Contains(string(data), "balance") {
		t.Errorf("stats.json carries a balance field: %s", data)
	}
}

// TestBalancePeriodicPoll covers §24.31 M157: an instance with a positive
// interval is queried again after the first check.
func TestBalancePeriodicPoll(t *testing.T) {
	backend := newBalBackend(t, `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"51.01"}]}`)
	cfg := &config.Config{
		AuthToken:     "tok",
		AdminUser:     "admin",
		AdminPassword: "secret",
		Instances: config.Instances{{
			Protocol: config.ProtocolOpenAI,
			Name:     "polled",
			BaseURL:  "http://127.0.0.1:1",
			APIKey:   "sk-test",
			Timeout:  config.Duration(2 * time.Second),
			Models:   []string{"deepseek-chat"},
			Balance: &config.Balance{
				Kind:     config.BalanceKindDeepSeek,
				URL:      backend.server.URL,
				Interval: balPoll(50 * time.Millisecond),
			},
		}},
	}
	_, _ = balServer(t, cfg)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if backend.calls() >= 3 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("balance endpoint calls = %d, want the periodic poll to repeat", backend.calls())
}

// TestBalanceStatsMiniMaxPlan covers §24.31 M15fy: a minimax_plan instance
// reports the two remaining quota windows in /api/stats.
func TestBalanceStatsMiniMaxPlan(t *testing.T) {
	backend := newBalBackend(t, `{"model_remains":[{"model_name":"general","current_interval_remaining_percent":90,"current_weekly_remaining_percent":97}]}`)
	cfg := &config.Config{
		AuthToken:     "tok",
		AdminUser:     "admin",
		AdminPassword: "secret",
		Instances: config.Instances{{
			Protocol: config.ProtocolOpenAI,
			Name:     "minimax",
			BaseURL:  "http://127.0.0.1:1",
			APIKey:   "sk-test",
			Timeout:  config.Duration(2 * time.Second),
			Models:   []string{"MiniMax-M3"},
			Balance: &config.Balance{
				Kind:     config.BalanceKindMiniMaxPlan,
				URL:      backend.server.URL,
				Interval: balNoPoll(),
			},
		}},
	}
	server, httpServer := balServer(t, cfg)
	inst := balInstance(t, server, protocolOpenAI, "minimax")
	info := balWaitChecked(t, inst)
	if info.Interval5hPercent != 90 || info.Weekly7dPercent != 97 {
		t.Errorf("percentages = %d/%d, want 90/97", info.Interval5hPercent, info.Weekly7dPercent)
	}
	if info.Exhausted {
		t.Error("exhausted = true, want false")
	}
	if info.Amount != "" || info.Currency != "" {
		t.Errorf("amount/currency = %q/%q, want empty for a subscription quota", info.Amount, info.Currency)
	}

	rows := balGetStats(t, httpServer.URL)
	row := balRow(rows, "minimax")
	if row == nil || row.Balance == nil {
		t.Fatalf("stats row = %+v, want a balance field", row)
	}
	var snapshot balanceInfo
	if err := json.Unmarshal(*row.Balance, &snapshot); err != nil {
		t.Fatalf("decode balance: %v", err)
	}
	if snapshot.Kind != config.BalanceKindMiniMaxPlan {
		t.Errorf("kind = %q, want %q", snapshot.Kind, config.BalanceKindMiniMaxPlan)
	}
	if snapshot.Interval5hPercent != 90 || snapshot.Weekly7dPercent != 97 {
		t.Errorf("stats percentages = %d/%d, want 90/97", snapshot.Interval5hPercent, snapshot.Weekly7dPercent)
	}
}

// TestBalanceLogs covers §24.31 M164: the state switch, the value change and
// the failed check each print one line.
func TestBalanceLogs(t *testing.T) {
	logs := captureStreamLogs(t)
	backend := newBalBackend(t, `{"is_available":false,"balance_infos":[{"currency":"CNY","total_balance":"0.00"}]}`)
	cfg := &config.Config{
		AuthToken:     "tok",
		AdminUser:     "admin",
		AdminPassword: "secret",
		Instances: config.Instances{{
			Protocol: config.ProtocolOpenAI,
			Name:     "logged",
			BaseURL:  "http://127.0.0.1:1",
			APIKey:   "sk-test",
			Timeout:  config.Duration(2 * time.Second),
			Models:   []string{"deepseek-chat"},
			Balance: &config.Balance{
				Kind:     config.BalanceKindDeepSeek,
				URL:      backend.server.URL,
				Interval: balNoPoll(),
			},
		}},
	}
	server, httpServer := balServer(t, cfg)
	inst := balInstance(t, server, protocolOpenAI, "logged")
	balWaitState(t, inst, stateExhausted)

	if !strings.Contains(logs(), "instance logged quota exhausted") {
		t.Errorf("logs = %q, want the quota exhausted line", logs())
	}
	if !strings.Contains(logs(), "balance changed: inst=logged") {
		t.Errorf("logs = %q, want the balance changed line", logs())
	}

	backend.set(`{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"12.34"}]}`, http.StatusOK)
	if code, _, _ := balRefresh(t, httpServer.URL, `{"protocol":"openai","name":"logged"}`, true); code != http.StatusOK {
		t.Fatalf("POST balance = %d, want 200", code)
	}
	if !strings.Contains(logs(), "instance logged quota restored") {
		t.Errorf("logs = %q, want the quota restored line", logs())
	}

	backend.set("upstream is down", http.StatusInternalServerError)
	if code, _, _ := balRefresh(t, httpServer.URL, `{"protocol":"openai","name":"logged"}`, true); code != http.StatusOK {
		t.Fatalf("POST balance = %d, want 200", code)
	}
	if !strings.Contains(logs(), "balance check failed: inst=logged err=") {
		t.Errorf("logs = %q, want the balance check failed line", logs())
	}
}
