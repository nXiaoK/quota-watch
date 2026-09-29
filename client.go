package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	adminReadTimeout    = 30 * time.Second
	adminResetTimeout   = 150 * time.Second
	adminUpdateTimeout  = 15 * time.Minute
	adminResponseLimit  = 8 << 20
	weeklyWindowMinutes = 7 * 24 * 60
)

type APIError struct {
	Status     int
	RetryAfter time.Duration
	Message    string
	Unknown    bool
}

func (e *APIError) Error() string { return e.Message }

type SnapshotUnavailableError struct{ Message string }

func (e *SnapshotUnavailableError) Error() string { return e.Message }

type AdminClient struct {
	baseURL           *url.URL
	apiKey            string
	http              *http.Client
	logger            *slog.Logger
	logEnabled        func() bool
	logSource         string
	subscriptionMu    sync.Mutex
	subscriptionHints map[int64]subscriptionHint
}

type subscriptionHint struct{ userID, groupID int64 }

func NewAdminClient(cfg Config) (*AdminClient, error) {
	u, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("main service URL must be an absolute HTTP or HTTPS URL without credentials, query, or fragment")
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return nil, errors.New("main service URL contains an invalid path")
		}
	}
	if strings.Contains(u.Path, "//") || u.RawPath != "" {
		return nil, errors.New("main service URL contains an invalid path")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(u.Path, "/api/v1") {
		u.Path += "/api/v1"
	}
	key := strings.TrimSpace(cfg.AdminAPIKey)
	if key == "" || strings.ContainsAny(key, "\r\n") {
		return nil, errors.New("a valid administrator API key is required")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &AdminClient{
		baseURL: u, apiKey: key,
		http:              &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		logger:            slog.Default(),
		logEnabled:        func() bool { return cfg.VerboseLogging },
		subscriptionHints: make(map[int64]subscriptionHint),
	}, nil
}

func (c *AdminClient) Connection(ctx context.Context) (ConnectionInfo, error) {
	_, err := c.ListAccounts(ctx, url.Values{"page": {"1"}, "page_size": {"1"}})
	if err != nil {
		return ConnectionInfo{}, err
	}
	return ConnectionInfo{Message: "administrator API connection succeeded using stored account data"}, err
}

func copyQuery(source url.Values, keys ...string) url.Values {
	result := make(url.Values)
	for _, key := range keys {
		if value := source.Get(key); value != "" {
			result.Set(key, value)
		}
	}
	return result
}

func (c *AdminClient) ListAccounts(ctx context.Context, query url.Values) (Page[Account], error) {
	query = copyQuery(query, "page", "page_size", "status", "search", "group", "sort_by", "sort_order")
	query.Set("platform", "openai")
	query.Set("type", "oauth")
	query.Set("lite", "true")
	query.Set("include_scheduler_score", "false")
	var records Page[accountRecord]
	if err := c.request(ctx, http.MethodGet, "/admin/accounts", query, nil, "", &records, false); err != nil {
		return Page[Account]{}, err
	}
	page := Page[Account]{Items: make([]Account, 0, len(records.Items)), Total: records.Total, Page: records.Page, PageSize: records.PageSize, Pages: records.Pages}
	for _, record := range records.Items {
		page.Items = append(page.Items, record.monitorAccount())
	}
	return page, nil
}

func sanitizeAccount(account Account) Account {
	extra := make(map[string]any)
	if enabled, ok := account.Extra["auto_reset_credit_enabled"].(bool); ok {
		extra["auto_reset_credit_enabled"] = enabled
	}
	for _, key := range []string{"auto_reset_credit_5h_threshold", "auto_reset_credit_7d_threshold"} {
		if value, ok := account.Extra[key].(float64); ok && !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 100 {
			extra[key] = value
		}
	}
	account.Extra = nil
	if len(extra) > 0 {
		account.Extra = extra
	}
	return account
}

func (c *AdminClient) GetAccount(ctx context.Context, id int64) (Account, error) {
	var record accountRecord
	if id <= 0 {
		return Account{}, errors.New("account ID must be positive")
	}
	if err := c.request(ctx, http.MethodGet, "/admin/accounts/"+strconv.FormatInt(id, 10), nil, nil, "", &record, false); err != nil {
		return Account{}, err
	}
	if record.ID != id {
		return Account{}, errors.New("administrator API returned a different account")
	}
	return record.monitorAccount(), nil
}

func (c *AdminClient) ListSubscriptions(ctx context.Context, query url.Values) (Page[Subscription], error) {
	query = copyQuery(query, "page", "page_size", "user_id", "group_id", "status", "platform", "sort_by", "sort_order")
	var page Page[Subscription]
	if err := c.request(ctx, http.MethodGet, "/admin/subscriptions", query, nil, "", &page, false); err != nil {
		return page, err
	}
	if page.Items == nil {
		page.Items = []Subscription{}
	}
	c.subscriptionMu.Lock()
	for _, sub := range page.Items {
		if sub.ID > 0 {
			c.subscriptionHints[sub.ID] = subscriptionHint{sub.UserID, sub.GroupID}
		}
	}
	c.subscriptionMu.Unlock()
	return page, nil
}

func (c *AdminClient) GetSubscriptions(ctx context.Context, ids []int64) (map[int64]Subscription, error) {
	targets := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, errors.New("subscription IDs must be positive")
		}
		targets[id] = struct{}{}
	}
	found := make(map[int64]Subscription, len(targets))
	if len(targets) == 0 {
		return found, nil
	}
	type scope struct {
		targets map[int64]struct{}
		groupID int64
		userID  int64
	}
	scopes := make(map[string]*scope)
	c.subscriptionMu.Lock()
	for id := range targets {
		hint := c.subscriptionHints[id]
		name := "unknown"
		if hint.groupID > 0 {
			name = "group:" + strconv.FormatInt(hint.groupID, 10)
		} else if hint.userID > 0 {
			name = "user:" + strconv.FormatInt(hint.userID, 10)
		}
		current := scopes[name]
		if current == nil {
			current = &scope{targets: make(map[int64]struct{}), groupID: hint.groupID, userID: hint.userID}
			scopes[name] = current
		} else if current.userID != hint.userID {
			current.userID = 0
		}
		current.targets[id] = struct{}{}
	}
	c.subscriptionMu.Unlock()
	names := make([]string, 0, len(scopes))
	for name := range scopes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		current := scopes[name]
		query := url.Values{"page_size": {"1000"}, "sort_by": {"created_at"}, "sort_order": {"desc"}}
		if current.userID > 0 {
			query.Set("user_id", strconv.FormatInt(current.userID, 10))
		} else if current.groupID > 0 {
			query.Set("group_id", strconv.FormatInt(current.groupID, 10))
		}
		selected, err := c.scanSubscriptions(ctx, current.targets, query)
		if err != nil {
			return nil, err
		}
		for id, sub := range selected {
			found[id] = sub
		}
	}
	return found, nil
}

func (c *AdminClient) PrimeSubscriptionHints(refs map[string]SubscriptionRef) {
	c.subscriptionMu.Lock()
	defer c.subscriptionMu.Unlock()
	for rawID, ref := range refs {
		id, err := strconv.ParseInt(rawID, 10, 64)
		if err == nil && id > 0 && (ref.UserID > 0 || ref.GroupID > 0) {
			c.subscriptionHints[id] = subscriptionHint{ref.UserID, ref.GroupID}
		}
	}
}

func (c *AdminClient) scanSubscriptions(ctx context.Context, targets map[int64]struct{}, query url.Values) (map[int64]Subscription, error) {
	found := make(map[int64]Subscription, len(targets))
	for pageNumber := 1; ; pageNumber++ {
		query.Set("page", strconv.Itoa(pageNumber))
		page, err := c.ListSubscriptions(ctx, query)
		if err != nil {
			return nil, err
		}
		if page.Page != pageNumber || page.PageSize <= 0 || page.Pages < 0 || (len(page.Items) > 0 && page.Pages == 0) {
			return nil, errors.New("administrator API returned invalid subscription pagination")
		}
		for _, sub := range page.Items {
			if _, wanted := targets[sub.ID]; wanted {
				found[sub.ID] = sub
			}
		}
		if len(found) == len(targets) || len(page.Items) == 0 || pageNumber >= page.Pages {
			return found, nil
		}
	}
}

type storedQuotaRecord struct {
	UsedPercent   *float64
	WindowMinutes *float64
	SampledAt     *time.Time
	ResetAt       *time.Time
	Identity      string
	Plan          string
}

type accountRecord struct {
	Account
	CreatedAt   time.Time `json:"created_at"`
	Credentials struct {
		ChatGPTAccountID string `json:"chatgpt_account_id"`
		OrganizationID   string `json:"organization_id"`
		Plan             string `json:"plan_type"`
	} `json:"credentials"`
	ParentChatGPTAccountID string `json:"parent_chatgpt_account_id"`
	ParentPlan             string `json:"parent_plan_type"`
}

func (record accountRecord) monitorAccount() Account {
	account := record.Account
	snapshot := &storedQuotaRecord{}
	if value, ok := account.Extra["codex_7d_used_percent"].(float64); ok {
		snapshot.UsedPercent = &value
	}
	if value, ok := account.Extra["codex_7d_window_minutes"].(float64); ok {
		snapshot.WindowMinutes = &value
	}
	for key, target := range map[string]**time.Time{"codex_usage_updated_at": &snapshot.SampledAt, "codex_7d_reset_at": &snapshot.ResetAt} {
		if raw, ok := account.Extra[key].(string); ok {
			if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
				*target = &parsed
			}
		}
	}
	identity := strings.TrimSpace(record.Credentials.ChatGPTAccountID)
	if identity == "" {
		identity = strings.TrimSpace(record.Credentials.OrganizationID)
	}
	snapshot.Plan = strings.TrimSpace(record.Credentials.Plan)
	if account.ParentAccountID != nil {
		identity = strings.TrimSpace(record.ParentChatGPTAccountID)
		snapshot.Plan = strings.TrimSpace(record.ParentPlan)
	}
	if identity != "" {
		parent := int64(0)
		if account.ParentAccountID != nil {
			parent = *account.ParentAccountID
		}
		encoded, _ := json.Marshal([]any{identity, account.ID, parent, record.CreatedAt})
		digest := sha256.Sum256(encoded)
		snapshot.Identity = "stored-account:" + hex.EncodeToString(digest[:])
	}
	account.storedQuota = snapshot
	return sanitizeAccount(account)
}

func (c *AdminClient) ReadStoredQuota(account Account) (QuotaSnapshot, error) {
	if account.ID <= 0 || account.Platform != "openai" || account.Type != "oauth" {
		return QuotaSnapshot{}, &SnapshotUnavailableError{Message: "quota monitoring requires an OpenAI OAuth account"}
	}
	snapshot := account.storedQuota
	if snapshot == nil || snapshot.Identity == "" || snapshot.Plan == "" || snapshot.SampledAt == nil || snapshot.SampledAt.Unix() <= 0 {
		return QuotaSnapshot{}, &SnapshotUnavailableError{Message: "stored quota is missing account identity, plan, or observation time; upstream refresh is disabled"}
	}
	dimension := "global"
	if account.ParentAccountID != nil || account.QuotaDimension == "spark" {
		if account.QuotaDimension != "" && account.QuotaDimension != "spark" {
			return QuotaSnapshot{}, &SnapshotUnavailableError{Message: "unsupported shadow account quota dimension"}
		}
		dimension = "spark"
	} else if account.QuotaDimension != "" && account.QuotaDimension != "global" {
		return QuotaSnapshot{}, &SnapshotUnavailableError{Message: "unsupported account quota dimension"}
	}
	if snapshot.WindowMinutes == nil || *snapshot.WindowMinutes != weeklyWindowMinutes || snapshot.UsedPercent == nil || snapshot.ResetAt == nil || snapshot.ResetAt.Unix() <= 0 {
		return QuotaSnapshot{}, &SnapshotUnavailableError{Message: "stored 7d quota usage, window duration, or reset time is unavailable; upstream refresh is disabled"}
	}
	if math.IsNaN(*snapshot.UsedPercent) || math.IsInf(*snapshot.UsedPercent, 0) || *snapshot.UsedPercent < 0 {
		return QuotaSnapshot{}, &SnapshotUnavailableError{Message: "stored quota contains an invalid usage percentage"}
	}
	return QuotaSnapshot{AccountID: account.ID, AccountName: account.Name, Identity: snapshot.Identity, Plan: snapshot.Plan,
		Source: "stored", Dimension: dimension, UsedPercent: *snapshot.UsedPercent, ResetAt: snapshot.ResetAt.Unix(), FetchedAt: snapshot.SampledAt.Unix()}, nil
}

type subscriptionResetRequest struct {
	SubscriptionIDs []int64 `json:"subscription_ids"`
	Action          string  `json:"action"`
	Daily           bool    `json:"daily,omitempty"`
	Weekly          bool    `json:"weekly,omitempty"`
	Monthly         bool    `json:"monthly,omitempty"`
}

func (c *AdminClient) ResetSubscriptions(ctx context.Context, ids []int64, mask ResetMask, key string) (ResetResult, error) {
	var result ResetResult
	if len(ids) == 0 || len(ids) > 100 || (!mask.Daily && !mask.Weekly && !mask.Monthly) {
		return result, errors.New("subscription reset requires 1 to 100 IDs and a selected quota window")
	}
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			return result, errors.New("subscription reset requires distinct positive IDs")
		}
		seen[id] = true
	}
	if key == "" || len(key) > 128 {
		return result, errors.New("a valid reset idempotency key is required")
	}
	for _, ch := range key {
		if ch < 33 || ch > 126 {
			return result, errors.New("a valid reset idempotency key is required")
		}
	}
	body := subscriptionResetRequest{ids, "reset_quota", mask.Daily, mask.Weekly, mask.Monthly}
	if err := c.request(ctx, http.MethodPost, "/admin/subscriptions/bulk-action", nil, body, key, &result, true); err != nil {
		return ResetResult{}, err
	}
	succeeded, failed := 0, 0
	for i := range result.Results {
		item := &result.Results[i]
		if !seen[item.SubscriptionID] || (!item.Success && item.Error == "") {
			return ResetResult{}, &APIError{Status: http.StatusOK, Message: "reset result could not be verified; execution outcome is unknown", Unknown: true}
		}
		delete(seen, item.SubscriptionID)
		if item.Success {
			succeeded++
			item.Error = ""
		} else {
			failed++
			item.Error = "subscription reset failed; inspect the main service logs"
		}
	}
	if len(seen) != 0 || succeeded != result.SuccessCount || failed != result.FailedCount {
		return ResetResult{}, &APIError{Status: http.StatusOK, Message: "reset result could not be verified; execution outcome is unknown", Unknown: true}
	}
	return result, nil
}

func (c *AdminClient) request(ctx context.Context, method, route string, query url.Values, body any, key string, result any, mutation bool) error {
	allowed := method == http.MethodGet && body == nil && !mutation && key == "" && (route == "/admin/accounts" || route == "/admin/subscriptions")
	if method == http.MethodGet && body == nil && !mutation && key == "" {
		switch route {
		case "/admin/usage":
			allowed = query.Encode() == "page=1&page_size=1&sort_by=created_at&sort_order=desc"
		case "/admin/system/check-updates":
			allowed = query.Encode() == "force=true"
		case "/admin/system/version":
			allowed = len(query) == 0
		}
	}
	if method == http.MethodGet && body == nil && !mutation && key == "" && strings.HasPrefix(route, "/admin/accounts/") {
		rawID := strings.TrimPrefix(route, "/admin/accounts/")
		id, err := strconv.ParseInt(rawID, 10, 64)
		allowed = err == nil && id > 0 && rawID == strconv.FormatInt(id, 10)
	}
	if method == http.MethodPost && mutation && route == "/admin/subscriptions/bulk-action" {
		reset, ok := body.(subscriptionResetRequest)
		allowed = ok && reset.Action == "reset_quota" && len(reset.SubscriptionIDs) > 0 && len(reset.SubscriptionIDs) <= 100 && (reset.Daily || reset.Weekly || reset.Monthly) && key != ""
	}
	if method == http.MethodPost && mutation && (route == "/admin/system/update" || route == "/admin/system/restart") {
		allowed = body == nil && len(query) == 0 && validSystemOperationKey(key)
	}
	if allowed && method == http.MethodGet && route == "/admin/accounts" {
		cloned := make(url.Values, len(query)+3)
		for name, values := range query {
			cloned[name] = append([]string(nil), values...)
		}
		query = cloned
		query.Set("platform", "openai")
		query.Set("type", "oauth")
		query.Set("include_scheduler_score", "false")
	}
	requestLog := c.startRequestLog(ctx, method, route, query, allowed)
	defer requestLog.end()
	if !allowed {
		requestLog.failure("route_not_permitted")
		return errors.New("administrator API route or action is not permitted by stored-data monitoring")
	}
	timeout := adminReadTimeout
	if mutation {
		timeout = adminResetTimeout
	}
	if route == "/admin/system/update" {
		timeout = adminUpdateTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	u := *c.baseURL
	u.Path += route
	u.RawQuery = query.Encode()
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			requestLog.failure("request_encode")
			return errors.New("failed to encode administrator API request")
		}
		payload = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), payload)
	if err != nil {
		requestLog.failure("request_build")
		return errors.New("failed to create administrator API request")
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		requestLog.failure("connection")
		message := "administrator API connection failed"
		if ctx.Err() != nil {
			requestLog.failure("context_canceled")
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				requestLog.failure("deadline_exceeded")
			}
			message = "administrator API request was canceled or timed out"
		}
		return &APIError{Message: message, Unknown: mutation}
	}
	defer resp.Body.Close()
	requestLog.response(resp.StatusCode)
	retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		requestLog.failure("http_status")
		unknown := mutation && (resp.StatusCode < 400 || resp.StatusCode >= 500)
		if mutation && resp.StatusCode == http.StatusConflict {
			var detail struct {
				Reason string `json:"reason"`
			}
			if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&detail) == nil && (detail.Reason == "IDEMPOTENCY_IN_PROGRESS" || detail.Reason == "IDEMPOTENCY_RETRY_BACKOFF") {
				unknown = true
			}
		}
		return &APIError{Status: resp.StatusCode, RetryAfter: retryAfter, Message: fmt.Sprintf("administrator API rejected request (HTTP %d)", resp.StatusCode), Unknown: unknown}
	}
	content, err := io.ReadAll(io.LimitReader(resp.Body, adminResponseLimit+1))
	if err != nil || len(content) > adminResponseLimit {
		requestLog.failure("response_read")
		return &APIError{Status: resp.StatusCode, Message: "administrator API response could not be read", Unknown: mutation}
	}
	var envelope struct {
		Code json.RawMessage `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(content, &envelope) != nil || len(envelope.Code) == 0 || (string(envelope.Code) != "0" && string(envelope.Code) != `"0"`) || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		requestLog.failure("response_envelope")
		return &APIError{Status: resp.StatusCode, Message: "administrator API returned an invalid or unsuccessful response", Unknown: mutation}
	}
	if json.Unmarshal(envelope.Data, result) != nil {
		requestLog.failure("response_payload")
		return &APIError{Status: resp.StatusCode, Message: "administrator API returned an invalid response payload", Unknown: mutation}
	}
	return nil
}

func parseRetryAfter(raw string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil && seconds > 0 {
		if seconds > int64((24*time.Hour)/time.Second) {
			seconds = int64((24 * time.Hour) / time.Second)
		}
		return time.Duration(seconds) * time.Second
	}
	if until, err := http.ParseTime(raw); err == nil && until.After(now) {
		delay := until.Sub(now)
		if delay > 24*time.Hour {
			delay = 24 * time.Hour
		}
		return delay
	}
	return 0
}
