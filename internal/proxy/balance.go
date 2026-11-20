package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"model-router/internal/config"
)

// §24.31 M154: the balance query is a lightweight call with a fixed timeout,
// independent of the instance's own request timeout.
const balanceQueryTimeout = 10 * time.Second

// §24.31 M154: default balance endpoints per kind.
const (
	miniMaxPlanBalanceURL = "https://api.minimax.cn/v1/api/openplatform/coding_plan/remains"
	deepSeekBalanceURL    = "https://api.deepseek.com/user/balance"
)

// unknownPercent marks a quota window whose remaining percentage could not be
// determined (§24.31 M155).
const unknownPercent = -1

// defaultBalanceURL returns the default endpoint of a balance kind.
func defaultBalanceURL(kind string) string {
	switch kind {
	case config.BalanceKindMiniMaxPlan:
		return miniMaxPlanBalanceURL
	case config.BalanceKindDeepSeek:
		return deepSeekBalanceURL
	default:
		return ""
	}
}

// balanceInfo is the unified balance/quota snapshot (§24.31 M155). It is the
// shape of the optional "balance" field of GET /api/stats.
type balanceInfo struct {
	// Kind is the balance check kind of the instance.
	Kind string `json:"kind"`
	// Interval5hPercent is the remaining percentage of the 5 hour window,
	// -1 when unknown.
	Interval5hPercent int `json:"interval5hPercent"`
	// Weekly7dPercent is the remaining percentage of the 7 day window,
	// -1 when unknown.
	Weekly7dPercent int `json:"weekly7dPercent"`
	// Amount is the remaining money of a prepaid balance, empty for
	// subscription quotas.
	Amount string `json:"amount"`
	// Currency is the currency of Amount.
	Currency string `json:"currency"`
	// Exhausted reports whether the quota/balance is used up.
	Exhausted bool `json:"exhausted"`
	// CheckedAt is the millisecond timestamp of the last successful query.
	CheckedAt int64 `json:"checkedAt"`
	// Err holds the reason of the last failed query, empty after a success.
	Err string `json:"err"`
}

// balanceResult is the outcome of one balance query.
type balanceResult struct {
	info balanceInfo
	// failed marks a query that could not be completed at all (network error,
	// non-2xx status or an unparsable body). Such a result keeps the previous
	// values and the previous exhausted state (§24.31 M158).
	failed bool
}

// balanceOutcome describes what a balance query changed on its instance.
type balanceOutcome struct {
	failed       bool
	stateChanged bool
	restored     bool
	valueChanged bool
}

// newBalanceClient builds the HTTP client used for balance queries.
func newBalanceClient() *http.Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: balanceQueryTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   balanceQueryTimeout,
		ResponseHeaderTimeout: balanceQueryTimeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   4,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{Transport: transport, Timeout: balanceQueryTimeout}
}

// queryInstanceBalance performs one balance query for inst. It never mutates
// the instance; the caller applies the result.
func queryInstanceBalance(ctx context.Context, client *http.Client, inst *instanceRuntime) balanceResult {
	info := balanceInfo{
		Kind:              inst.balanceKind,
		Interval5hPercent: unknownPercent,
		Weekly7dPercent:   unknownPercent,
	}
	res := balanceResult{info: info}

	endpoint := inst.balanceURL
	if endpoint == "" {
		endpoint = defaultBalanceURL(inst.balanceKind)
	}
	if endpoint == "" {
		res.info.Err = fmt.Sprintf("unknown balance kind %q", inst.balanceKind)
		res.failed = true
		return res
	}

	reqCtx, cancel := context.WithTimeout(ctx, balanceQueryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		res.info.Err = err.Error()
		res.failed = true
		return res
	}
	req.Header.Set("Authorization", "Bearer "+inst.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		res.info.Err = err.Error()
		res.failed = true
		return res
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		res.info.Err = err.Error()
		res.failed = true
		return res
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		res.info.Err = fmt.Sprintf("unexpected status %d: %s", resp.StatusCode,
			truncateRunes(strings.TrimSpace(string(body)), statsErrorSummaryRune))
		res.failed = true
		return res
	}

	var note string
	switch inst.balanceKind {
	case config.BalanceKindMiniMaxPlan:
		note, err = parseMiniMaxPlan(body, &res.info)
	case config.BalanceKindDeepSeek:
		note, err = parseDeepSeekBalance(body, &res.info)
	default:
		err = fmt.Errorf("unknown balance kind %q", inst.balanceKind)
	}
	if err != nil {
		res.info.Err = err.Error()
		res.failed = true
		return res
	}
	// The payload was read but a field was missing: the quota cannot be
	// judged, so the instance counts as not exhausted (§24.31 M156).
	res.info.Err = note
	res.info.CheckedAt = time.Now().UnixMilli()
	return res
}

// miniMaxPlanResponse is the MiniMax M Plan quota payload.
type miniMaxPlanResponse struct {
	ModelRemains []miniMaxPlanEntry `json:"model_remains"`
}

// miniMaxPlanEntry is one model entry of the MiniMax M Plan quota payload.
type miniMaxPlanEntry struct {
	ModelName                   string   `json:"model_name"`
	CurrentIntervalRemainingPct *float64 `json:"current_interval_remaining_percent"`
	CurrentWeeklyRemainingPct   *float64 `json:"current_weekly_remaining_percent"`
}

// parseMiniMaxPlan reads the remaining quota of the "general" model. The
// returned error is fatal; a non-empty note means a field was missing.
func parseMiniMaxPlan(body []byte, info *balanceInfo) (string, error) {
	var payload miniMaxPlanResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("decode minimax plan response: %w", err)
	}
	var entry *miniMaxPlanEntry
	for i := range payload.ModelRemains {
		if payload.ModelRemains[i].ModelName == "general" {
			entry = &payload.ModelRemains[i]
			break
		}
	}
	if entry == nil {
		return `model_remains has no "general" entry, quota unknown`, nil
	}
	if entry.CurrentIntervalRemainingPct != nil {
		info.Interval5hPercent = int(math.Round(*entry.CurrentIntervalRemainingPct))
	}
	if entry.CurrentWeeklyRemainingPct != nil {
		info.Weekly7dPercent = int(math.Round(*entry.CurrentWeeklyRemainingPct))
	}
	info.Exhausted = quotaExhausted(info.Interval5hPercent, info.Weekly7dPercent)
	if info.Interval5hPercent == unknownPercent && info.Weekly7dPercent == unknownPercent {
		return "remaining percentages are missing, quota unknown", nil
	}
	return "", nil
}

// quotaExhausted applies the MiniMax rule (§24.31 M156): both windows carry
// their own quota and a model needs both, so one empty window is enough. An
// unknown window (-1) is never treated as exhausted.
func quotaExhausted(interval5h, weekly7d int) bool {
	if interval5h != unknownPercent && interval5h <= 0 {
		return true
	}
	if weekly7d != unknownPercent && weekly7d <= 0 {
		return true
	}
	return false
}

// deepSeekBalanceResponse is the DeepSeek prepaid balance payload.
type deepSeekBalanceResponse struct {
	IsAvailable  *bool             `json:"is_available"`
	BalanceInfos []deepSeekBalance `json:"balance_infos"`
}

// deepSeekBalance is one currency entry of the DeepSeek balance payload.
type deepSeekBalance struct {
	Currency        string       `json:"currency"`
	TotalBalance    balanceValue `json:"total_balance"`
	GrantedBalance  balanceValue `json:"granted_balance"`
	ToppedUpBalance balanceValue `json:"topped_up_balance"`
}

// balanceValue accepts both the string ("51.01") and the numeric (51.01)
// spelling of an upstream amount and keeps the upstream text.
type balanceValue string

func (v *balanceValue) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "null" {
		*v = ""
		return nil
	}
	if strings.HasPrefix(trimmed, "\"") {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		*v = balanceValue(strings.TrimSpace(text))
		return nil
	}
	var number json.Number
	if err := json.Unmarshal(data, &number); err != nil {
		return err
	}
	*v = balanceValue(number.String())
	return nil
}

// numeric parses the amount, reporting whether it could be read.
func (v balanceValue) numeric() (float64, bool) {
	text := strings.TrimSpace(string(v))
	if text == "" {
		return 0, false
	}
	amount, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, false
	}
	return amount, true
}

// parseDeepSeekBalance reads the prepaid balance. The returned error is fatal;
// a non-empty note means a field was missing.
func parseDeepSeekBalance(body []byte, info *balanceInfo) (string, error) {
	var payload deepSeekBalanceResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("decode deepseek balance response: %w", err)
	}
	// §24.31 M155: prefer CNY, fall back to the first entry and report the
	// currency that was actually read.
	entry := -1
	for i := range payload.BalanceInfos {
		if strings.EqualFold(strings.TrimSpace(payload.BalanceInfos[i].Currency), "CNY") {
			entry = i
			break
		}
	}
	if entry < 0 && len(payload.BalanceInfos) > 0 {
		entry = 0
	}
	if entry >= 0 {
		chosen := payload.BalanceInfos[entry]
		info.Currency = strings.TrimSpace(chosen.Currency)
		info.Amount = strings.TrimSpace(string(chosen.TotalBalance))
	}
	note := ""
	switch {
	case payload.IsAvailable != nil && !*payload.IsAvailable:
		info.Exhausted = true
	case entry >= 0:
		if amount, ok := payload.BalanceInfos[entry].TotalBalance.numeric(); ok {
			info.Exhausted = amount <= 0
		} else {
			note = "total_balance is unreadable, balance unknown"
		}
	default:
		note = "balance_infos is empty, balance unknown"
	}
	if payload.IsAvailable == nil && note == "" {
		note = "is_available is missing, balance unknown"
	}
	return note, nil
}

// applyBalance stores a balance query result and moves the quota state of the
// instance (§24.31 M156/M158/M160). A failed query keeps the previous values
// and the previous exhausted state.
func (inst *instanceRuntime) applyBalance(res balanceResult) balanceOutcome {
	inst.mu.Lock()
	defer inst.mu.Unlock()

	out := balanceOutcome{failed: res.failed}
	if res.failed {
		// §24.31 M158: keep the last successful values, only record why the
		// query failed.
		inst.balanceInfo.Err = res.info.Err
		return out
	}

	previous := inst.balanceInfo
	inst.balanceInfo = res.info
	out.valueChanged = previous.Interval5hPercent != res.info.Interval5hPercent ||
		previous.Weekly7dPercent != res.info.Weekly7dPercent ||
		previous.Amount != res.info.Amount ||
		previous.Currency != res.info.Currency ||
		previous.Exhausted != res.info.Exhausted
	if inst.quotaExhausted != res.info.Exhausted {
		inst.quotaExhausted = res.info.Exhausted
		if res.info.Exhausted {
			inst.quotaExhaustedSince = inst.nowTime()
		} else {
			inst.quotaExhaustedSince = time.Time{}
		}
		out.stateChanged = true
		out.restored = !res.info.Exhausted
	}
	return out
}

// isExhausted reports whether the quota/balance of the instance is used up
// (§24.31 M161).
func (inst *instanceRuntime) isExhausted() bool {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return inst.quotaExhausted
}

// balanceSnapshot returns the last balance snapshot of the instance and
// whether the instance takes part in a balance check at all (§24.31 M165).
func (inst *instanceRuntime) balanceSnapshot() (balanceInfo, bool) {
	inst.mu.Lock()
	defer inst.mu.Unlock()
	if inst.balanceKind == "" {
		return balanceInfo{}, false
	}
	return inst.balanceInfo, true
}

// logBalanceOutcome prints the §24.31 M164 log lines of one balance query.
func logBalanceOutcome(inst *instanceRuntime, res balanceResult, out balanceOutcome) {
	if out.failed {
		log.Printf("balance check failed: inst=%s err=%s", inst.name, res.info.Err)
		return
	}
	if out.stateChanged {
		if res.info.Exhausted {
			log.Printf("instance %s quota exhausted", inst.name)
		} else {
			log.Printf("instance %s quota restored", inst.name)
		}
	}
	if out.valueChanged {
		log.Printf("balance changed: inst=%s 5h=%d%% 7d=%d%% amount=%s exhausted=%t",
			inst.name, res.info.Interval5hPercent, res.info.Weekly7dPercent,
			res.info.Amount, res.info.Exhausted)
	}
}

// balancePoller runs the periodic balance checks of one server state
// (§24.31 M157).
type balancePoller struct {
	client *http.Client
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// newBalancePoller starts one checker goroutine per instance that declares a
// balance check. The first check of every instance runs right away so the
// state is known within seconds of the startup.
func newBalancePoller(client *http.Client, insts []*instanceRuntime) *balancePoller {
	ctx, cancel := context.WithCancel(context.Background())
	p := &balancePoller{client: client, cancel: cancel}
	for _, inst := range insts {
		if inst.balanceKind == "" {
			continue
		}
		p.wg.Add(1)
		go func(inst *instanceRuntime) {
			defer p.wg.Done()
			p.run(ctx, inst)
		}(inst)
	}
	return p
}

// stop cancels the running checks and waits for them to finish.
func (p *balancePoller) stop() {
	if p == nil {
		return
	}
	p.cancel()
	p.wg.Wait()
}

// run performs the first check and then polls until the context is cancelled.
func (p *balancePoller) run(ctx context.Context, inst *instanceRuntime) {
	p.check(ctx, inst)
	interval := inst.balanceInterval
	if interval <= 0 {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.check(ctx, inst)
		}
	}
}

// check runs one balance query and applies the result.
func (p *balancePoller) check(ctx context.Context, inst *instanceRuntime) {
	res := queryInstanceBalance(ctx, p.client, inst)
	out := inst.applyBalance(res)
	logBalanceOutcome(inst, res, out)
}

// balanceInstanceResult is one row of the POST /api/instances/balance response
// (§24.31 M159).
type balanceInstanceResult struct {
	Name     string      `json:"name"`
	Protocol string      `json:"protocol"`
	Kind     string      `json:"kind"`
	State    string      `json:"state"`
	Balance  balanceInfo `json:"balance"`
}

// balanceRefreshResponse is the body of POST /api/instances/balance.
type balanceRefreshResponse struct {
	Instances []balanceInstanceResult `json:"instances"`
}

// serveInstanceBalance implements POST /api/instances/balance (§24.31 M159):
// it queries the requested instances synchronously and returns their fresh
// balance and state.
func (s *Server) serveInstanceBalance(w http.ResponseWriter, r *http.Request, st *serverState) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeAdminError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body struct {
		Protocol string `json:"protocol"`
		Name     string `json:"name"`
	}
	if err := decodeInstanceRequest(r, &body); err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	proto, err := normalizeLockProtocol(body.Protocol)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, err.Error())
		return
	}
	insts := st.handler.byProtocol[proto]
	name := strings.TrimSpace(body.Name)

	var targets []*instanceRuntime
	if name != "" {
		for _, inst := range insts {
			if inst.name == name {
				targets = append(targets, inst)
				break
			}
		}
		if len(targets) == 0 {
			writeAdminError(w, http.StatusNotFound, "instance not found: "+name)
			return
		}
		if targets[0].balanceKind == "" {
			writeAdminError(w, http.StatusNotFound, "instance has no balance check: "+name)
			return
		}
	} else {
		for _, inst := range insts {
			if inst.balanceKind != "" {
				targets = append(targets, inst)
			}
		}
	}

	results := make([]balanceInstanceResult, 0, len(targets))
	for _, inst := range targets {
		res := queryInstanceBalance(r.Context(), s.balanceClient, inst)
		out := inst.applyBalance(res)
		logBalanceOutcome(inst, res, out)
		state, _, _ := inst.displayState()
		info, _ := inst.balanceSnapshot()
		results = append(results, balanceInstanceResult{
			Name:     inst.name,
			Protocol: string(proto),
			Kind:     inst.balanceKind,
			State:    state,
			Balance:  info,
		})
	}
	writeJSONError(w, http.StatusOK, balanceRefreshResponse{Instances: results})
}
