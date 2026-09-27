package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type memoryEngineStore struct {
	mu     sync.Mutex
	cfg    Config
	state  State
	reject func(State) bool
}

func cloneEngineState(state State) State {
	data, _ := json.Marshal(state)
	var out State
	_ = json.Unmarshal(data, &out)
	return out
}
func (s *memoryEngineStore) Config() (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg, nil
}
func (s *memoryEngineStore) Snapshot() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneEngineState(s.state), nil
}
func (s *memoryEngineStore) Update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := cloneEngineState(s.state)
	if err := fn(&state); err != nil {
		return err
	}
	if s.reject != nil && s.reject(state) {
		return errors.New("simulated durable write failure")
	}
	s.state = state
	return nil
}

type engineTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *engineTestClock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *engineTestClock) Add(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

type engineQuotaAnswer struct {
	percent                           float64
	identity, plan, dimension, source string
	fetchedAt                         int64
	resetAt                           int64
	err                               error
}
type engineResetCall struct {
	ids  []int64
	mask ResetMask
	key  string
}
type engineFakeAdmin struct {
	mu                  sync.Mutex
	clock               *engineTestClock
	answers             map[int64][]engineQuotaAnswer
	quotaCalls          map[int64]int
	accountCalls        map[int64]int
	subscriptions       map[int64]Subscription
	resetCalls          []engineResetCall
	reset               func(context.Context, []int64, ResetMask, string) (ResetResult, error)
	beforeSubscriptions func()
}

func (a *engineFakeAdmin) Connection(context.Context) (ConnectionInfo, error) {
	return ConnectionInfo{}, nil
}
func (a *engineFakeAdmin) ListAccounts(context.Context, url.Values) (Page[Account], error) {
	return Page[Account]{}, nil
}
func (a *engineFakeAdmin) ListSubscriptions(context.Context, url.Values) (Page[Subscription], error) {
	return Page[Subscription]{}, nil
}
func (a *engineFakeAdmin) GetAccount(_ context.Context, id int64) (Account, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.accountCalls[id]++
	if answers := a.answers[id]; len(answers) > 0 {
		var apiError *APIError
		if errors.As(answers[0].err, &apiError) {
			a.answers[id] = answers[1:]
			return Account{}, answers[0].err
		}
	}
	return Account{ID: id, Name: "source", Platform: "openai", Type: "oauth", Status: "active"}, nil
}
func (a *engineFakeAdmin) ReadStoredQuota(account Account) (QuotaSnapshot, error) {
	a.mu.Lock()
	a.quotaCalls[account.ID]++
	answer := engineQuotaAnswer{}
	if answers := a.answers[account.ID]; len(answers) > 0 {
		answer = answers[0]
		a.answers[account.ID] = answers[1:]
	}
	a.mu.Unlock()
	if answer.err != nil {
		return QuotaSnapshot{}, answer.err
	}
	if answer.identity == "" {
		answer.identity = "upstream-identity"
	}
	if answer.plan == "" {
		answer.plan = "plus"
	}
	if answer.dimension == "" {
		answer.dimension = "global"
	}
	if answer.fetchedAt == 0 {
		answer.fetchedAt = a.clock.Now().Unix()
	}
	if answer.source == "" {
		answer.source = "stored"
	}
	if answer.resetAt == 0 {
		answer.resetAt = 123456
	}
	return QuotaSnapshot{Source: answer.source, AccountID: account.ID, AccountName: account.Name, Identity: answer.identity, Plan: answer.plan, Dimension: answer.dimension, UsedPercent: answer.percent, FetchedAt: answer.fetchedAt, ResetAt: answer.resetAt}, nil
}
func (a *engineFakeAdmin) GetSubscriptions(_ context.Context, ids []int64) (map[int64]Subscription, error) {
	if a.beforeSubscriptions != nil {
		a.beforeSubscriptions()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[int64]Subscription{}
	for _, id := range ids {
		if subscription, found := a.subscriptions[id]; found {
			out[id] = subscription
		}
	}
	return out, nil
}
func (a *engineFakeAdmin) ResetSubscriptions(ctx context.Context, ids []int64, mask ResetMask, key string) (ResetResult, error) {
	a.mu.Lock()
	a.resetCalls = append(a.resetCalls, engineResetCall{ids: append([]int64(nil), ids...), mask: mask, key: key})
	a.mu.Unlock()
	if a.reset != nil {
		return a.reset(ctx, ids, mask, key)
	}
	result := ResetResult{SuccessCount: len(ids)}
	for _, id := range ids {
		result.Results = append(result.Results, ResetItem{SubscriptionID: id, Success: true})
	}
	return result, nil
}
func engineFixture(t *testing.T) (*Engine, *memoryEngineStore, *engineFakeAdmin, *engineTestClock) {
	t.Helper()
	clock := &engineTestClock{now: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}
	cfg := DefaultConfig()
	cfg.BaseURL, cfg.AdminAPIKey, cfg.AutoResetEnabled = "https://admin.example", "secret", true
	cfg.Telegram.Enabled, cfg.Email.Enabled = true, true
	rule := Rule{ID: "rule-1", Name: "first", Enabled: true, AccountIDs: []int64{1}, SubscriptionIDs: []int64{10, 11}, AutoReset: true, Weekly: true}
	store := &memoryEngineStore{cfg: cfg, state: State{Rules: []Rule{rule}, Observations: map[string]Observation{"1": {AccountID: 1, AccountName: "source", Last: &QuotaSnapshot{Source: "stored", AccountID: 1, AccountName: "source", Identity: "upstream-identity", Plan: "plus", Dimension: "global", UsedPercent: 42, FetchedAt: clock.Now().Add(-time.Minute).Unix(), ResetAt: 123456}}}, Events: []Event{}, Actions: []Action{}, Deliveries: []Delivery{}}}
	api := &engineFakeAdmin{clock: clock, answers: map[int64][]engineQuotaAnswer{}, quotaCalls: map[int64]int{}, accountCalls: map[int64]int{}, subscriptions: map[int64]Subscription{}}
	for _, id := range rule.SubscriptionIDs {
		api.subscriptions[id] = Subscription{ID: id, UserID: id, GroupID: 2, Status: "active", StartsAt: clock.Now().Add(-time.Hour), ExpiresAt: clock.Now().Add(24 * time.Hour)}
	}
	engine := newEngine(store, nil)
	engine.NewAdmin = func(Config) (AdminAPI, error) { return api, nil }
	engine.Now = clock.Now
	engine.wait = func(ctx context.Context, d time.Duration) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		clock.Add(d)
		return nil
	}
	engine.Notify = func(context.Context, Config, string, string) error { return nil }
	return engine, store, api, clock
}
func mustEngineState(t *testing.T, store *memoryEngineStore) State {
	t.Helper()
	state, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestEngineConfirmedResetSharesSamplingAndMergesTargets(t *testing.T) {
	engine, store, api, clock := engineFixture(t)
	store.state.Rules[0].NotifyTelegram, store.state.Rules[0].NotifyEmail = true, true
	store.state.Rules = append(store.state.Rules, Rule{ID: "rule-2", Name: "second", Enabled: true, AccountIDs: []int64{1}, SubscriptionIDs: []int64{10, 10}, AutoReset: true, Daily: true, Monthly: true})
	var notifications sync.Map
	engine.Notify = func(_ context.Context, _ Config, channel, message string) error {
		notifications.Store(channel, message)
		return nil
	}
	api.reset = func(_ context.Context, ids []int64, _ ResetMask, key string) (ResetResult, error) {
		state := mustEngineState(t, store)
		if len(state.Events) != 1 || state.Observations["1"].Last.UsedPercent != 0 {
			t.Error("event and new baseline must be durable before reset")
		}
		found := false
		for _, action := range state.Actions {
			if action.IdempotencyKey == key && action.Status == "running" {
				found = true
			}
		}
		if !found {
			t.Error("request must have durable running action")
		}
		out := ResetResult{SuccessCount: len(ids)}
		for _, id := range ids {
			out.Results = append(out.Results, ResetItem{SubscriptionID: id, Success: true})
		}
		return out, nil
	}
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := mustEngineState(t, store)
	if api.quotaCalls[1] != 2 || len(state.Events) != 1 || len(api.resetCalls) != 2 {
		t.Fatalf("sampling/events/actions = %d/%d/%d", api.quotaCalls[1], len(state.Events), len(api.resetCalls))
	}
	seen := map[int64]ResetMask{}
	for _, call := range api.resetCalls {
		for _, id := range call.ids {
			if _, found := seen[id]; found {
				t.Fatalf("subscription %d reset twice", id)
			}
			seen[id] = call.mask
		}
	}
	if seen[10] != (ResetMask{Daily: true, Weekly: true, Monthly: true}) || seen[11] != (ResetMask{Weekly: true}) {
		t.Fatalf("incorrect merged masks: %#v", seen)
	}
	if len(state.Deliveries) != 2 || len(state.Events[0].RuleNames) != 2 {
		t.Fatal("missing consolidated notifications")
	}
	for _, channel := range []string{"telegram", "email"} {
		if _, found := notifications.Load(channel); !found {
			t.Errorf("missing %s notification", channel)
		}
	}
	clock.Add(time.Minute)
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(mustEngineState(t, store).Events) != 1 || len(api.resetCalls) != 2 {
		t.Fatal("unchanged zero triggered again")
	}
}

func TestEngineOnlyStrictValidStoredEdgesTrigger(t *testing.T) {
	tests := []struct {
		name        string
		first       bool
		answers     []engineQuotaAnswer
		wantPercent float64
		wantError   bool
	}{
		{name: "initial zero", first: true, answers: []engineQuotaAnswer{{}}},
		{name: "small raw positive is not zero", answers: []engineQuotaAnswer{{percent: 0.00001}}, wantPercent: 0.00001},
		{name: "identity change", answers: []engineQuotaAnswer{{identity: "different"}}},
		{name: "plan change", answers: []engineQuotaAnswer{{plan: "pro"}}},
		{name: "dimension change", answers: []engineQuotaAnswer{{dimension: "spark"}}},
		{name: "zero disappears on confirmation", answers: []engineQuotaAnswer{{}, {percent: 3}}, wantPercent: 3},
		{name: "identity changes during confirmation", answers: []engineQuotaAnswer{{}, {identity: "different"}}},
		{name: "older than last observed DB snapshot", answers: []engineQuotaAnswer{{fetchedAt: 1}}, wantPercent: 42},
		{name: "future sample", answers: []engineQuotaAnswer{{fetchedAt: 1900000000}}, wantPercent: 42},
		{name: "invalid raw number", answers: []engineQuotaAnswer{{percent: math.NaN()}}, wantPercent: 42},
		{name: "failed confirmation", answers: []engineQuotaAnswer{{}, {err: &SnapshotUnavailableError{Message: "stored snapshot temporarily unavailable"}}}, wantPercent: 42},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			engine, store, api, _ := engineFixture(t)
			if tc.first {
				observation := store.state.Observations["1"]
				observation.Last = nil
				store.state.Observations["1"] = observation
			}
			api.answers[1] = tc.answers
			err := engine.CheckNow(context.Background())
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v", err)
			}
			state := mustEngineState(t, store)
			if len(state.Events) != 0 || len(api.resetCalls) != 0 {
				t.Fatal("unexpected event or write")
			}
			if state.Observations["1"].Last.UsedPercent != tc.wantPercent {
				t.Fatalf("last valid = %g, want %g", state.Observations["1"].Last.UsedPercent, tc.wantPercent)
			}
		})
	}
	t.Run("same stored zero snapshot confirms", func(t *testing.T) {
		engine, store, api, clock := engineFixture(t)
		stamp := clock.Now().Unix()
		api.answers[1] = []engineQuotaAnswer{{fetchedAt: stamp}, {fetchedAt: stamp}}
		if err := engine.CheckNow(context.Background()); err != nil {
			t.Fatal(err)
		}
		if mustEngineState(t, store).Observations["1"].Last.UsedPercent != 0 || len(api.resetCalls) != 1 {
			t.Fatal("unchanged zero DB snapshot was not accepted as a passive reread")
		}
	})
}

func TestEngineRateLimitPreservesLastAndNotifiesOnceUntilRecovery(t *testing.T) {
	engine, store, api, clock := engineFixture(t)
	store.state.Rules[0].NotifyTelegram, store.state.Rules[0].NotifyEmail = true, true
	api.answers[1] = []engineQuotaAnswer{{err: &APIError{Status: 429, Message: "slow down", RetryAfter: 120 * time.Second}}, {err: &APIError{Status: 429, Message: "slow down", RetryAfter: 120 * time.Second}}, {}, {}}
	if err := engine.CheckNow(context.Background()); err == nil {
		t.Fatal("expected rate limit")
	}
	state := mustEngineState(t, store)
	if state.Observations["1"].Last.UsedPercent != 42 || len(state.Deliveries) != 2 || !state.Observations["1"].NextCheckAt.Equal(clock.Now().Add(120*time.Second)) {
		t.Fatal("lost baseline, backoff, or notification")
	}
	clock.Add(60 * time.Second)
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if api.accountCalls[1] != 1 {
		t.Fatal("429 backoff ignored")
	}
	clock.Add(60 * time.Second)
	if err := engine.CheckNow(context.Background()); err == nil {
		t.Fatal("expected second rate limit")
	}
	if len(mustEngineState(t, store).Deliveries) != 2 {
		t.Fatal("unchanged outage repeatedly notified")
	}
	clock.Add(120 * time.Second)
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	state = mustEngineState(t, store)
	if len(state.Events) != 1 || len(state.Deliveries) != 6 || state.Observations["1"].Failures != 0 {
		t.Fatalf("recovery/events/deliveries = %d/%d/%d", state.Observations["1"].Failures, len(state.Events), len(state.Deliveries))
	}
}

func TestEngineDurableWritesMustPrecedeNetworkReset(t *testing.T) {
	for _, phase := range []string{"event", "running"} {
		t.Run(phase, func(t *testing.T) {
			engine, store, api, clock := engineFixture(t)
			store.reject = func(state State) bool {
				if phase == "event" {
					return len(state.Events) > 0
				}
				for _, action := range state.Actions {
					if action.Status == "running" {
						return true
					}
				}
				return false
			}
			if err := engine.CheckNow(context.Background()); err == nil {
				t.Fatal("expected persistence failure")
			}
			if len(api.resetCalls) != 0 {
				t.Fatal("reset before durable event/running state")
			}
			state := mustEngineState(t, store)
			if phase == "event" && state.Observations["1"].Last.UsedPercent != 42 {
				t.Fatal("event and baseline not atomic")
			}
			pendingKey := ""
			if len(state.Actions) > 0 {
				pendingKey = state.Actions[0].IdempotencyKey
			}
			store.reject = nil
			clock.Add(time.Minute)
			if err := engine.CheckNow(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(api.resetCalls) != 1 {
				t.Fatal("pending action failed recovery")
			}
			if pendingKey != "" && api.resetCalls[0].key != pendingKey {
				t.Fatal("pending key changed")
			}
		})
	}
}

func TestEngineUnknownAndRecoveredRunningNeverReplay(t *testing.T) {
	engine, store, api, clock := engineFixture(t)
	store.state.Rules[0].NotifyTelegram = true
	api.reset = func(context.Context, []int64, ResetMask, string) (ResetResult, error) {
		return ResetResult{}, &APIError{Status: 500, Message: "response lost after commit", Unknown: true}
	}
	if err := engine.CheckNow(context.Background()); err == nil {
		t.Fatal("expected unknown result")
	}
	state := mustEngineState(t, store)
	if state.Actions[0].Status != "unknown" {
		t.Fatalf("status %s", state.Actions[0].Status)
	}
	if err := engine.RetryAction(context.Background(), state.Actions[0].ID); err == nil {
		t.Fatal("unknown allowed blind retry")
	}
	store.state.Actions[0].Status, store.state.Actions[0].Results = "running", nil
	clock.Add(time.Minute)
	restarted := newEngine(store, nil)
	restarted.NewAdmin, restarted.Notify, restarted.Now, restarted.wait = engine.NewAdmin, engine.Notify, engine.Now, engine.wait
	if err := restarted.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	state = mustEngineState(t, store)
	if len(api.resetCalls) != 1 || state.Actions[0].Status != "unknown" || len(state.Actions[0].Results) != 2 {
		t.Fatal("restart replayed running action or lost uncertain targets")
	}
	if message := eventActionMessage(state.Events[0], state.Actions); !strings.Contains(message, "结果未知 2") {
		t.Fatalf("misleading summary: %s", message)
	}
}

func TestEnginePartialRetryOnlyFailedSubsetWithNewKey(t *testing.T) {
	engine, store, api, clock := engineFixture(t)
	store.state.Rules[0].SubscriptionIDs = append(store.state.Rules[0].SubscriptionIDs, 12)
	api.subscriptions[12] = Subscription{ID: 12, Status: "active", StartsAt: clock.Now().Add(-time.Hour), ExpiresAt: clock.Now().Add(-time.Second)}
	calls := 0
	api.reset = func(_ context.Context, ids []int64, _ ResetMask, _ string) (ResetResult, error) {
		calls++
		if calls == 1 {
			return ResetResult{SuccessCount: 1, FailedCount: 1, Results: []ResetItem{{SubscriptionID: 10, Success: true}, {SubscriptionID: 11, Error: "temporary failure"}}}, nil
		}
		return ResetResult{SuccessCount: 1, Results: []ResetItem{{SubscriptionID: 11, Success: true}}}, nil
	}
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := mustEngineState(t, store)
	original := state.Actions[0]
	if original.Status != "partial" || len(original.Results) != 3 {
		t.Fatalf("outcome %#v", original)
	}
	if err := engine.RetryAction(context.Background(), original.ID); err != nil {
		t.Fatal(err)
	}
	state = mustEngineState(t, store)
	if len(api.resetCalls) != 2 || len(api.resetCalls[1].ids) != 1 || api.resetCalls[1].ids[0] != 11 {
		t.Fatal("retry included successful/skipped targets")
	}
	if api.resetCalls[0].key == api.resetCalls[1].key {
		t.Fatal("retry reused original key")
	}
	if len(state.Actions) != 2 || state.Actions[1].Status != "succeeded" || state.Actions[0].Status != "retried" {
		t.Fatal("retry lost original audit outcome")
	}
	if err := engine.RetryAction(context.Background(), original.ID); err == nil {
		t.Fatal("original retry should not be reusable after spawning a child")
	}
	if len(api.resetCalls) != 2 {
		t.Fatal("repeated parent retry cleared successful targets again")
	}
}

func TestEnginePendingActionsRespectSwitchAndRuleChanges(t *testing.T) {
	changes := map[string]func(*memoryEngineStore){
		"global off": func(s *memoryEngineStore) { s.cfg.AutoResetEnabled = false }, "rule disabled": func(s *memoryEngineStore) { s.state.Rules[0].Enabled = false }, "rule auto off": func(s *memoryEngineStore) { s.state.Rules[0].AutoReset = false },
		"targets changed": func(s *memoryEngineStore) { s.state.Rules[0].SubscriptionIDs = []int64{10} }, "mask changed": func(s *memoryEngineStore) { s.state.Rules[0].Daily = true }, "source changed": func(s *memoryEngineStore) { s.state.Rules[0].AccountIDs = []int64{2} }, "server changed": func(s *memoryEngineStore) { s.cfg.BaseURL = "https://different.example" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			engine, store, api, clock := engineFixture(t)
			event := Event{Source: "stored", ID: "old-event", AccountID: 1, DetectedAt: clock.Now()}
			store.state.Events = append(store.state.Events, event)
			store.state.Actions = buildEventActions(store.cfg, event, store.state.Rules, clock.Now())
			observation := store.state.Observations["1"]
			observation.Last.UsedPercent = 0
			store.state.Observations["1"] = observation
			change(store)
			if err := engine.CheckNow(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(api.resetCalls) != 0 || mustEngineState(t, store).Actions[0].Status != "skipped" {
				t.Fatal("old frozen action ran after safety change")
			}
		})
	}
	t.Run("switch during subscription check", func(t *testing.T) {
		engine, store, api, _ := engineFixture(t)
		api.beforeSubscriptions = func() { store.mu.Lock(); store.cfg.AutoResetEnabled = false; store.mu.Unlock() }
		if err := engine.CheckNow(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(api.resetCalls) != 0 || mustEngineState(t, store).Actions[0].Status != "skipped" {
			t.Fatal("final switch check missing")
		}
	})
}

func TestEngineLargeEventSplitsBatchesWithoutBusinessLimit(t *testing.T) {
	engine, store, api, clock := engineFixture(t)
	ids := make([]int64, 0, 202)
	for id := int64(10); id <= 210; id++ {
		ids = append(ids, id)
		api.subscriptions[id] = Subscription{ID: id, Status: "active", StartsAt: clock.Now().Add(-time.Hour), ExpiresAt: clock.Now().Add(24 * time.Hour)}
	}
	ids = append(ids, 10)
	store.state.Rules[0].SubscriptionIDs = ids
	store.state.Rules = append(store.state.Rules, Rule{ID: "second", Name: "overlap", Enabled: true, AccountIDs: []int64{1}, SubscriptionIDs: ids, AutoReset: true, Daily: true})
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(api.resetCalls) != 3 {
		t.Fatalf("201 targets batches = %d", len(api.resetCalls))
	}
	seen := map[int64]bool{}
	keys := map[string]bool{}
	for _, call := range api.resetCalls {
		if len(call.ids) > 100 || len(call.ids) == 0 {
			t.Fatal("invalid batch size")
		}
		if call.mask != (ResetMask{Daily: true, Weekly: true}) {
			t.Fatal("incorrect mask")
		}
		if keys[call.key] {
			t.Fatal("batches reused key")
		}
		keys[call.key] = true
		for _, id := range call.ids {
			if seen[id] {
				t.Fatalf("duplicate target %d", id)
			}
			seen[id] = true
		}
	}
	if len(seen) != 201 || api.quotaCalls[1] != 2 {
		t.Fatal("business targets limited or source duplicated")
	}
}

func TestEngineNotificationRetriesNeverRepeatReset(t *testing.T) {
	engine, store, api, clock := engineFixture(t)
	store.state.Rules[0].NotifyTelegram, store.state.Rules[0].NotifyEmail = true, true
	var mutex sync.Mutex
	attempts := map[string]int{}
	engine.Notify = func(_ context.Context, _ Config, channel, _ string) error {
		mutex.Lock()
		defer mutex.Unlock()
		attempts[channel]++
		if channel == "telegram" && attempts[channel] == 1 {
			return errors.New("telegram unavailable")
		}
		return nil
	}
	if err := engine.CheckNow(context.Background()); err == nil {
		t.Fatal("expected notification failure")
	}
	state := mustEngineState(t, store)
	if state.Actions[0].Status != "succeeded" || len(api.resetCalls) != 1 || attempts["email"] != 1 {
		t.Fatal("notification failure affected reset or other channel")
	}
	clock.Add(29 * time.Second)
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts["telegram"] != 1 {
		t.Fatal("notification backoff ignored")
	}
	clock.Add(time.Second)
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts["telegram"] != 2 || attempts["email"] != 1 || len(api.resetCalls) != 1 {
		t.Fatal("notification retry repeated other work")
	}
	for _, delivery := range mustEngineState(t, store).Deliveries {
		if delivery.Status != "succeeded" {
			t.Fatal("delivery not recovered")
		}
	}
}

func TestEngineMalformedResetResultBecomesUnknown(t *testing.T) {
	engine, store, api, _ := engineFixture(t)
	api.reset = func(context.Context, []int64, ResetMask, string) (ResetResult, error) {
		return ResetResult{SuccessCount: 2, Results: []ResetItem{{SubscriptionID: 10, Success: true}}}, nil
	}
	if err := engine.CheckNow(context.Background()); err == nil {
		t.Fatal("incomplete result trusted")
	}
	if mustEngineState(t, store).Actions[0].Status != "unknown" {
		t.Fatal("incomplete result not unknown")
	}
}
func TestEngineSimulationNeverMutatesOrSends(t *testing.T) {
	engine, store, api, clock := engineFixture(t)
	api.subscriptions[11] = Subscription{ID: 11, Status: "expired", StartsAt: clock.Now().Add(-time.Hour), ExpiresAt: clock.Now().Add(-time.Minute)}
	before := hashValue(mustEngineState(t, store))
	engine.Notify = func(context.Context, Config, string, string) error {
		t.Error("simulation sent notification")
		return nil
	}
	result, err := engine.Simulate(context.Background(), store.state.Rules[0])
	if err != nil {
		t.Fatal(err)
	}
	if !result.WillAutoReset || len(result.Subscriptions) != 1 || len(result.Skipped) != 1 {
		t.Fatal("incorrect simulation targets")
	}
	if hashValue(mustEngineState(t, store)) != before || api.quotaCalls[1] != 0 || len(api.resetCalls) != 0 {
		t.Fatal("simulation modified state, sampled quota, or wrote")
	}
}
func TestEngineBusyPreventsOverlappingTasks(t *testing.T) {
	engine, _, _, clock := engineFixture(t)
	waiting, release := make(chan struct{}), make(chan struct{})
	engine.wait = func(ctx context.Context, delay time.Duration) error {
		close(waiting)
		select {
		case <-release:
			clock.Add(delay)
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	done := make(chan error, 1)
	go func() { done <- engine.CheckNow(context.Background()) }()
	<-waiting
	if !engine.IsBusy() {
		t.Fatal("busy false during confirmation")
	}
	if err := engine.CheckNow(context.Background()); !errors.Is(err, errEngineBusy) {
		t.Fatal("overlapping check accepted")
	}
	if err := engine.RetryAction(context.Background(), "anything"); !errors.Is(err, errEngineBusy) {
		t.Fatal("overlapping retry accepted")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if engine.IsBusy() {
		t.Fatal("busy not released")
	}
}

func TestEngineSamePassMultipleSourcesShareOneResetAndKeepChannels(t *testing.T) {
	engine, store, api, clock := engineFixture(t)
	store.state.Rules[0].SubscriptionIDs = []int64{10}
	store.state.Rules[0].NotifyTelegram = true
	store.state.Rules = append(store.state.Rules, Rule{ID: "second-source", Name: "source two", Enabled: true, AccountIDs: []int64{2}, SubscriptionIDs: []int64{10}, AutoReset: true, Daily: true, NotifyEmail: true})
	second := *store.state.Observations["1"].Last
	second.AccountID = 2
	store.state.Observations["2"] = Observation{AccountID: 2, AccountName: "second", Last: &second}
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := mustEngineState(t, store)
	if len(state.Events) != 2 || len(state.Actions) != 1 || len(api.resetCalls) != 1 || len(state.Actions[0].EventIDs) != 2 {
		t.Fatal("same-pass sources did not share one action")
	}
	if api.resetCalls[0].mask != (ResetMask{Daily: true, Weekly: true}) || len(api.resetCalls[0].ids) != 1 {
		t.Fatal("shared target mask not merged")
	}
	if len(state.Deliveries) != 2 {
		t.Fatalf("expected source-specific channels, got %d", len(state.Deliveries))
	}
	eventAccounts := map[string]int64{}
	for _, event := range state.Events {
		eventAccounts[event.ID] = event.AccountID
	}
	for _, delivery := range state.Deliveries {
		expected := "telegram"
		if eventAccounts[delivery.EventID] == 2 {
			expected = "email"
		}
		if delivery.Channel != expected {
			t.Fatalf("event %s delivered to unintended %s", delivery.EventID, delivery.Channel)
		}
	}
	clock.Add(time.Minute)
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(api.resetCalls) != 1 || len(mustEngineState(t, store).Events) != 2 {
		t.Fatal("unchanged simultaneous zero replayed")
	}
}

func TestEngineAllowsAnotherConfirmedManualResetInSameResetTimestamp(t *testing.T) {
	engine, store, api, clock := engineFixture(t)
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Add(time.Minute)
	api.answers[1] = []engineQuotaAnswer{{percent: 12}}
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Add(time.Minute)
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := mustEngineState(t, store)
	if len(state.Events) != 2 || len(api.resetCalls) != 2 || state.Events[0].ResetAt != state.Events[1].ResetAt {
		t.Fatal("new >0 to 0 transition was suppressed by a calendar cooldown")
	}
}

func TestEngineVerifiesSelectedSubscriptionOwnership(t *testing.T) {
	engine, store, api, _ := engineFixture(t)
	store.state.Rules[0].SubscriptionRefs = map[string]SubscriptionRef{"10": {UserID: 999, GroupID: 2}, "11": {UserID: 11, GroupID: 999}}
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := mustEngineState(t, store)
	if len(api.resetCalls) != 0 || state.Actions[0].Status != "skipped" {
		t.Fatal("changed target ownership was accepted")
	}
	for _, result := range state.Actions[0].Results {
		if !strings.Contains(result.Error, "归属") {
			t.Fatal("missing ownership skip reason")
		}
	}
}

func TestEngineRunKeepsCadenceFromStartOfPass(t *testing.T) {
	engine, store, _, clock := engineFixture(t)
	store.state.Rules = nil
	started := clock.Now()
	store.reject = func(State) bool { clock.Add(3 * time.Second); return false }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	engine.wait = func(_ context.Context, delay time.Duration) error {
		if !clock.Now().Add(delay).Equal(started.Add(time.Minute)) {
			t.Errorf("next pass was scheduled relative to completion, delay=%s", delay)
		}
		cancel()
		return context.Canceled
	}
	engine.Run(ctx)
}

func TestEngineResultPersistenceFailureBecomesUnknownWithoutReplay(t *testing.T) {
	engine, store, api, clock := engineFixture(t)
	store.reject = func(state State) bool {
		for _, action := range state.Actions {
			if action.Status == "succeeded" {
				return true
			}
		}
		return false
	}
	if err := engine.CheckNow(context.Background()); err == nil {
		t.Fatal("expected post-write ledger failure")
	}
	state := mustEngineState(t, store)
	if len(api.resetCalls) != 1 || state.Actions[0].Status != "running" {
		t.Fatal("reset or durable running marker missing")
	}
	store.reject = nil
	clock.Add(time.Minute)
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	state = mustEngineState(t, store)
	if len(api.resetCalls) != 1 || state.Actions[0].Status != "unknown" {
		t.Fatal("ledger failure caused blind reset replay")
	}
	if err := engine.RetryAction(context.Background(), state.Actions[0].ID); err == nil {
		t.Fatal("uncertain post-write outcome allowed retry")
	}
}

func TestEngineOldStoredSnapshotIsBaselineThenWaitsWithoutPrediction(t *testing.T) {
	engine, store, api, clock := engineFixture(t)
	observation := store.state.Observations["1"]
	observation.Last = nil
	store.state.Observations["1"] = observation
	sampled := clock.Now().Add(-72 * time.Hour).Unix()
	expiredReset := clock.Now().Add(-48 * time.Hour).Unix()
	api.answers[1] = []engineQuotaAnswer{{percent: 67, fetchedAt: sampled, resetAt: expiredReset}, {percent: 67, fetchedAt: sampled, resetAt: expiredReset}}
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := mustEngineState(t, store)
	if state.Observations["1"].Status != "baseline" || state.Observations["1"].Last.UsedPercent != 67 {
		t.Fatal("an old stored raw snapshot was discarded or inferred to zero")
	}
	clock.Add(time.Minute)
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	state = mustEngineState(t, store)
	if state.Observations["1"].Status != "waiting" || state.Observations["1"].Last.UsedPercent != 67 || len(state.Events) != 0 || len(api.resetCalls) != 0 {
		t.Fatal("unchanged DB sample must wait without predicting reset")
	}
}

func TestEngineSameStoredTimestampMutationPreservesBaseline(t *testing.T) {
	tests := []struct {
		name    string
		percent float64
		reset   int64
	}{
		{name: "changed percent", percent: 0, reset: 123456},
		{name: "changed reset_at", percent: 42, reset: 123457},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			engine, store, api, _ := engineFixture(t)
			previous := *store.state.Observations["1"].Last
			api.answers[1] = []engineQuotaAnswer{{percent: tc.percent, fetchedAt: previous.FetchedAt, resetAt: tc.reset}}
			if err := engine.CheckNow(context.Background()); err != nil {
				t.Fatal(err)
			}
			state := mustEngineState(t, store)
			if state.Observations["1"].Status != "unknown" || !sameBaseline(state.Observations["1"].Last, &previous) || len(state.Events) != 0 || len(api.resetCalls) != 0 {
				t.Fatal("inconsistent DB sample overwrote baseline or reset subscriptions")
			}
		})
	}
}

func TestEngineConfirmationRejectsChangedSameStoredSample(t *testing.T) {
	engine, store, api, clock := engineFixture(t)
	stamp := clock.Now().Unix()
	api.answers[1] = []engineQuotaAnswer{{fetchedAt: stamp}, {percent: 3, fetchedAt: stamp}}
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := mustEngineState(t, store)
	if state.Observations["1"].Status != "unknown" || state.Observations["1"].Last.UsedPercent != 42 || len(api.resetCalls) != 0 {
		t.Fatal("inconsistent confirmation changed baseline or reset subscriptions")
	}
}

func TestEngineUpgradeClearsActiveBaselinesAndCancelsOldActions(t *testing.T) {
	for _, status := range []string{"pending", "failed", "running"} {
		t.Run(status, func(t *testing.T) {
			engine, store, api, clock := engineFixture(t)
			observation := store.state.Observations["1"]
			observation.Last.Source = ""
			store.state.Observations["1"] = observation
			event := Event{ID: "legacy-active-event", AccountID: 1, DetectedAt: clock.Now()}
			store.state.Events = append(store.state.Events, event)
			store.state.Actions = buildEventActions(store.cfg, event, store.state.Rules, clock.Now())
			store.state.Actions[0].Status = status
			store.state.Actions[0].Results = failedItems(store.state.Actions[0].SubscriptionIDs, "prior failure")
			if err := engine.CheckNow(context.Background()); err != nil {
				t.Fatal(err)
			}
			state := mustEngineState(t, store)
			if len(api.resetCalls) != 0 || len(state.Events) != 1 || state.Observations["1"].Last.Source != "stored" || state.Observations["1"].Status != "baseline" {
				t.Fatal("upgrade reused active baseline or executed old pending reset")
			}
			expected := status
			if status == "pending" {
				expected = "skipped"
			}
			if status == "running" {
				expected = "unknown"
			}
			if state.Actions[0].Status != expected {
				t.Fatalf("legacy %s became %s", status, state.Actions[0].Status)
			}
			if err := engine.RetryAction(context.Background(), state.Actions[0].ID); err == nil {
				t.Fatal("old active-source action allowed a manual blind retry")
			}
			if len(api.resetCalls) != 0 {
				t.Fatal("legacy action reset subscriptions after upgrade")
			}
		})
	}
}

func TestEngineDelayedNewStoredZeroIncludesSampleTime(t *testing.T) {
	engine, store, api, clock := engineFixture(t)
	observation := store.state.Observations["1"]
	observation.Last.FetchedAt = clock.Now().Add(-30 * 24 * time.Hour).Unix()
	store.state.Observations["1"] = observation
	stamp := clock.Now().Add(-21 * 24 * time.Hour).Unix()
	api.answers[1] = []engineQuotaAnswer{{fetchedAt: stamp}, {fetchedAt: stamp}}
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := mustEngineState(t, store)
	if len(state.Events) != 1 || len(api.resetCalls) != 1 || state.Events[0].SampledAt != stamp || state.Events[0].Source != "stored" {
		t.Fatal("newer stored reset was lost or had no sample provenance")
	}
	if message := eventMessage(state.Events[0]); !strings.Contains(message, "实际重置可能早于检测时间") || !strings.Contains(message, time.Unix(stamp, 0).UTC().Format(time.RFC3339)) {
		t.Fatal("delayed detection notification omitted saved sample time")
	}
}

func TestEngineMissingStoredSnapshotWaitsWithoutOutageNotifications(t *testing.T) {
	for _, first := range []bool{false, true} {
		name := "preserve existing baseline"
		if first {
			name = "first missing snapshot"
		}
		t.Run(name, func(t *testing.T) {
			engine, store, api, clock := engineFixture(t)
			store.state.Rules[0].NotifyTelegram, store.state.Rules[0].NotifyEmail = true, true
			if first {
				observation := store.state.Observations["1"]
				observation.Last = nil
				store.state.Observations["1"] = observation
			}
			unknown := &SnapshotUnavailableError{Message: "stored quota not available"}
			api.answers[1] = []engineQuotaAnswer{{err: unknown}, {err: unknown}, {percent: 42}}
			for i := 0; i < 2; i++ {
				if err := engine.CheckNow(context.Background()); err != nil {
					t.Fatal(err)
				}
				state := mustEngineState(t, store)
				observation := state.Observations["1"]
				if observation.Status != "unknown" || observation.Failures != 0 || !observation.NextCheckAt.IsZero() || len(state.Deliveries) != 0 || len(api.resetCalls) != 0 {
					t.Fatal("missing snapshot was classified as outage, throttled, notified, or reset")
				}
				if !first && (observation.Last == nil || observation.Last.UsedPercent != 42) {
					t.Fatal("missing snapshot lost previous raw baseline")
				}
				clock.Add(time.Minute)
			}
			if api.quotaCalls[1] != 2 {
				t.Fatal("missing snapshot reads did not retain configured cadence")
			}
			if err := engine.CheckNow(context.Background()); err != nil {
				t.Fatal(err)
			}
			state := mustEngineState(t, store)
			if state.Observations["1"].Last == nil || state.Observations["1"].Last.UsedPercent != 42 || len(state.Deliveries) != 0 || len(state.Events) != 0 {
				t.Fatal("first valid saved snapshot sent spurious recovery/reset notification")
			}
		})
	}
}
