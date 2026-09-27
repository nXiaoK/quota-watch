package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func manualFixture(t *testing.T) (*Engine, *memoryEngineStore, *engineFakeAdmin, *engineTestClock) {
	t.Helper()
	engine, store, api, clock := engineFixture(t)
	store.cfg.AutoResetEnabled = false
	store.cfg.Telegram.BotToken, store.cfg.Telegram.ChatID = "123:test_bot_token", "42"
	store.state.Rules[0].AutoReset = false
	store.state.Rules[0].NotifyTelegram = true
	store.state.Rules[0].SubscriptionRefs = map[string]SubscriptionRef{"10": {UserID: 10, GroupID: 2}, "11": {UserID: 11, GroupID: 2}}
	engine.NotifyInteractive = func(context.Context, Config, Delivery) error { return nil }
	return engine, store, api, clock
}

func detectManualRequest(t *testing.T, engine *Engine, store *memoryEngineStore, api *engineFakeAdmin) ManualRequest {
	t.Helper()
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := mustEngineState(t, store)
	if len(state.ManualRequests) != 1 || len(state.Actions) != 0 || len(api.resetCalls) != 0 {
		t.Fatal("detection did not preserve an unapproved request without reset actions")
	}
	request := state.ManualRequests[0]
	if request.Status != "pending" || len(request.ID) != 32 || request.ExpiresAt.Sub(request.CreatedAt) != manualRequestTTL {
		t.Fatal("request identity, initial state, or expiry was not persisted")
	}
	return request
}

func manualCallback(request ManualRequest, choice string) TelegramCallback {
	return TelegramCallback{UpdateID: 1, QueryID: "callback", ChatID: 42, ChatType: "private", FromID: 42, MessageID: 1, Data: "qw:" + choice + ":" + request.ID}
}

func TestManualResetRequiresRecipientDecisionAndExecutesOnce(t *testing.T) {
	engine, store, api, _ := manualFixture(t)
	store.cfg.Telegram.AllowedUserIDs = []int64{99}
	var interactive atomic.Int64
	engine.NotifyInteractive = func(_ context.Context, _ Config, delivery Delivery) error {
		if delivery.ManualRequestID == "" || !strings.Contains(delivery.Message, "#10（周）") {
			t.Error("interactive delivery omitted selected subscription scope")
		}
		interactive.Add(1)
		return nil
	}
	request := detectManualRequest(t, engine, store, api)
	if interactive.Load() != 1 {
		t.Fatal("manual request did not produce exactly one interactive notification")
	}
	unauthorized := manualCallback(request, "r")
	unauthorized.FromID = 99
	decision, err := engine.DecideManualReset(context.Background(), unauthorized)
	if err != nil || decision.Accepted || decision.RequestID != "" || mustEngineState(t, store).ManualRequests[0].Status != "pending" {
		t.Fatal("allowlisted outsider consumed a private recipient's request")
	}
	var accepted atomic.Int64
	var workers sync.WaitGroup
	for i := 0; i < 10; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			decision, err := engine.DecideManualReset(context.Background(), manualCallback(request, "r"))
			if err != nil {
				t.Error(err)
			}
			if decision.Accepted {
				accepted.Add(1)
			}
		}()
	}
	workers.Wait()
	state := mustEngineState(t, store)
	if accepted.Load() != 1 || len(state.Actions) != 1 || state.ManualRequests[0].ApprovedBy != 42 || state.Actions[0].Mode != "manual" || len(api.resetCalls) != 0 {
		t.Fatal("decision was not claimed once, durably, before execution")
	}
	engine.busy.Store(true)
	if err := engine.ProcessManualReset(context.Background(), request.ID); !errors.Is(err, errEngineBusy) {
		t.Fatal("manual reset bypassed engine serialization")
	}
	engine.busy.Store(false)
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := engine.ProcessManualReset(context.Background(), request.ID); err != nil {
		t.Fatal(err)
	}
	state = mustEngineState(t, store)
	if len(api.resetCalls) != 1 || state.ManualRequests[0].Status != "succeeded" || store.cfg.AutoResetEnabled || store.state.Rules[0].AutoReset {
		t.Fatal("manual confirmation did not execute once while keeping automatic reset disabled")
	}
}

func TestManualIgnoreAndStartupNeverResetPendingRequest(t *testing.T) {
	engine, store, api, _ := manualFixture(t)
	request := detectManualRequest(t, engine, store, api)
	restarted := newEngine(store, nil)
	restarted.Now, restarted.NewAdmin, restarted.wait = engine.Now, engine.NewAdmin, engine.wait
	restarted.Notify, restarted.NotifyInteractive = engine.Notify, engine.NotifyInteractive
	if err := restarted.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(api.resetCalls) != 0 || len(mustEngineState(t, store).Actions) != 0 {
		t.Fatal("startup treated an unapproved request as reset authorization")
	}
	decision, err := restarted.DecideManualReset(context.Background(), manualCallback(request, "i"))
	if err != nil || decision.Accepted || decision.RequestID != request.ID || mustEngineState(t, store).ManualRequests[0].Status != "ignored" {
		t.Fatal("ignore did not claim the pending decision without resetting")
	}
	decision, err = restarted.DecideManualReset(context.Background(), manualCallback(request, "r"))
	if err != nil || decision.Accepted {
		t.Fatal("ignored request could be approved later")
	}
	if err := restarted.ProcessManualReset(context.Background(), request.ID); err != nil || len(api.resetCalls) != 0 {
		t.Fatal("ignored request was executed")
	}
}

func TestManualCallbackRejectsChangedScopeAndExpiry(t *testing.T) {
	changes := map[string]func(*memoryEngineStore){
		"targets": func(s *memoryEngineStore) { s.state.Rules[0].SubscriptionIDs = []int64{10} },
		"ownership refs": func(s *memoryEngineStore) {
			s.state.Rules[0].SubscriptionRefs["10"] = SubscriptionRef{UserID: 20, GroupID: 2}
		},
		"mask":                       func(s *memoryEngineStore) { s.state.Rules[0].Daily = true },
		"source accounts":            func(s *memoryEngineStore) { s.state.Rules[0].AccountIDs = []int64{2} },
		"site":                       func(s *memoryEngineStore) { s.cfg.BaseURL = "https://changed.example" },
		"admin credential":           func(s *memoryEngineStore) { s.cfg.AdminAPIKey = "changed-key" },
		"bot":                        func(s *memoryEngineStore) { s.cfg.Telegram.BotToken = "456:another_bot" },
		"chat":                       func(s *memoryEngineStore) { s.cfg.Telegram.ChatID = "43" },
		"permissions":                func(s *memoryEngineStore) { s.cfg.Telegram.AllowedUserIDs = []int64{50} },
		"rule disabled":              func(s *memoryEngineStore) { s.state.Rules[0].Enabled = false },
		"telegram disabled":          func(s *memoryEngineStore) { s.cfg.Telegram.Enabled = false },
		"rule notification disabled": func(s *memoryEngineStore) { s.state.Rules[0].NotifyTelegram = false },
		"snapshot source":            func(s *memoryEngineStore) { s.state.Events[0].Source = "upstream" },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			engine, store, api, _ := manualFixture(t)
			request := detectManualRequest(t, engine, store, api)
			store.mu.Lock()
			change(store)
			store.mu.Unlock()
			decision, err := engine.DecideManualReset(context.Background(), manualCallback(request, "r"))
			if err != nil || decision.Accepted || len(mustEngineState(t, store).Actions) != 0 || len(api.resetCalls) != 0 {
				t.Fatal("stale callback authorized changed reset scope")
			}
		})
	}
	t.Run("expired before approval", func(t *testing.T) {
		engine, store, api, clock := manualFixture(t)
		request := detectManualRequest(t, engine, store, api)
		clock.Add(manualRequestTTL)
		decision, err := engine.DecideManualReset(context.Background(), manualCallback(request, "r"))
		if err != nil || decision.Accepted || mustEngineState(t, store).ManualRequests[0].Status != "expired" || len(api.resetCalls) != 0 {
			t.Fatal("expired request was accepted")
		}
	})
	t.Run("expiry or ownership changes during subscription fetch", func(t *testing.T) {
		for _, expire := range []bool{true, false} {
			engine, store, api, clock := manualFixture(t)
			request := detectManualRequest(t, engine, store, api)
			if _, err := engine.DecideManualReset(context.Background(), manualCallback(request, "r")); err != nil {
				t.Fatal(err)
			}
			api.beforeSubscriptions = func() {
				if expire {
					clock.Add(manualRequestTTL)
				} else {
					api.mu.Lock()
					subscription := api.subscriptions[10]
					subscription.UserID = 100
					api.subscriptions[10] = subscription
					subscription = api.subscriptions[11]
					subscription.GroupID = 100
					api.subscriptions[11] = subscription
					api.mu.Unlock()
				}
			}
			if err := engine.ProcessManualReset(context.Background(), request.ID); err != nil || len(api.resetCalls) != 0 {
				t.Fatal("execution missed final expiry or immutable ownership validation")
			}
		}
	})
}

func TestManualNewResetSupersedesOldRequest(t *testing.T) {
	engine, store, api, clock := manualFixture(t)
	old := detectManualRequest(t, engine, store, api)
	clock.Add(time.Minute)
	api.answers[1] = []engineQuotaAnswer{{percent: 20}}
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Add(time.Minute)
	if err := engine.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := mustEngineState(t, store)
	if len(state.ManualRequests) != 2 || state.ManualRequests[0].Status != "superseded" || state.ManualRequests[1].Status != "pending" {
		t.Fatal("new event did not supersede the previous target decision")
	}
	decision, err := engine.DecideManualReset(context.Background(), manualCallback(old, "r"))
	if err != nil || decision.Accepted || len(api.resetCalls) != 0 {
		t.Fatal("superseded request was approved")
	}
	current := state.ManualRequests[1]
	decision, err = engine.DecideManualReset(context.Background(), manualCallback(current, "r"))
	if err != nil || !decision.Accepted {
		t.Fatal("latest request could not be approved")
	}
	if err := engine.ProcessManualReset(context.Background(), current.ID); err != nil || len(api.resetCalls) != 1 {
		t.Fatal("latest target scope did not execute once")
	}
}

func TestManualSamplingPassMergesSharedTargetsAcrossSources(t *testing.T) {
	engine, store, api, _ := manualFixture(t)
	store.state.Rules[0].SubscriptionIDs = []int64{10}
	store.state.Rules = append(store.state.Rules, Rule{ID: "second", Enabled: true, AccountIDs: []int64{2}, SubscriptionIDs: []int64{10}, SubscriptionRefs: map[string]SubscriptionRef{"10": {UserID: 10, GroupID: 2}}, Daily: true, NotifyTelegram: true})
	observation := store.state.Observations["1"]
	second := *observation.Last
	second.AccountID = 2
	observation.AccountID, observation.Last = 2, &second
	store.state.Observations["2"] = observation
	request := detectManualRequest(t, engine, store, api)
	if len(request.EventIDs) != 2 || len(request.Targets) != 1 || request.Targets[0].Mask != (ResetMask{Daily: true, Weekly: true}) {
		t.Fatal("same-pass target windows were not merged into one request")
	}
	decision, err := engine.DecideManualReset(context.Background(), manualCallback(request, "r"))
	if err != nil || !decision.Accepted {
		t.Fatal("merged scope could not be approved")
	}
	if err := engine.ProcessManualReset(context.Background(), request.ID); err != nil || len(api.resetCalls) != 1 || len(api.resetCalls[0].ids) != 1 || api.resetCalls[0].mask != (ResetMask{Daily: true, Weekly: true}) {
		t.Fatal("shared subscription was cleared more than once or lost a selected window")
	}
}

func TestManualKnownFailureRetryAndUnknownNoReplay(t *testing.T) {
	t.Run("partial retry", func(t *testing.T) {
		engine, store, api, _ := manualFixture(t)
		request := detectManualRequest(t, engine, store, api)
		api.reset = func(context.Context, []int64, ResetMask, string) (ResetResult, error) {
			return ResetResult{SuccessCount: 1, FailedCount: 1, Results: []ResetItem{{SubscriptionID: 10, Success: true}, {SubscriptionID: 11, Error: "clear rejected"}}}, nil
		}
		if _, err := engine.DecideManualReset(context.Background(), manualCallback(request, "r")); err != nil {
			t.Fatal(err)
		}
		if err := engine.ProcessManualReset(context.Background(), request.ID); err != nil {
			t.Fatal(err)
		}
		state := mustEngineState(t, store)
		if state.ManualRequests[0].Status != "partial" {
			t.Fatal("partial manual outcome was not retained")
		}
		original := state.Actions[0]
		api.reset = nil
		if err := engine.RetryAction(context.Background(), original.ID); err != nil {
			t.Fatal(err)
		}
		state = mustEngineState(t, store)
		if len(api.resetCalls) != 2 || len(api.resetCalls[1].ids) != 1 || api.resetCalls[1].ids[0] != 11 || state.Actions[1].Mode != "manual" || state.Actions[1].ManualRequestID != request.ID || state.ManualRequests[0].Status != "succeeded" {
			t.Fatal("manual retry lost approval scope or included a successful target")
		}
		if err := engine.RetryAction(context.Background(), original.ID); err == nil || len(api.resetCalls) != 2 {
			t.Fatal("repeated parent retry repeated a reset")
		}
	})
	t.Run("unknown no replay", func(t *testing.T) {
		engine, store, api, _ := manualFixture(t)
		request := detectManualRequest(t, engine, store, api)
		api.reset = func(context.Context, []int64, ResetMask, string) (ResetResult, error) {
			return ResetResult{}, &APIError{Status: 500, Unknown: true, Message: "response lost"}
		}
		if _, err := engine.DecideManualReset(context.Background(), manualCallback(request, "r")); err != nil {
			t.Fatal(err)
		}
		if err := engine.ProcessManualReset(context.Background(), request.ID); err == nil {
			t.Fatal("unknown outcome was hidden")
		}
		state := mustEngineState(t, store)
		if state.Actions[0].Status != "unknown" || state.ManualRequests[0].Status != "unknown" {
			t.Fatal("unknown manual state was not persisted")
		}
		if err := engine.RetryAction(context.Background(), state.Actions[0].ID); err == nil {
			t.Fatal("unknown manual action allowed a blind retry")
		}
		if err := engine.CheckNow(context.Background()); err != nil || len(api.resetCalls) != 1 {
			t.Fatal("unknown outcome was replayed by a later check")
		}
	})
}

func TestManualDecisionPersistenceAndDeliveryScope(t *testing.T) {
	t.Run("claim failure", func(t *testing.T) {
		engine, store, api, _ := manualFixture(t)
		request := detectManualRequest(t, engine, store, api)
		store.reject = func(state State) bool { return state.ManualRequests[0].Status == "processing" }
		decision, err := engine.DecideManualReset(context.Background(), manualCallback(request, "r"))
		if err == nil || decision.Accepted || decision.RequestID != "" || len(api.resetCalls) != 0 || len(mustEngineState(t, store).Actions) != 0 {
			t.Fatal("failed durable claim was reported as accepted or started an action")
		}
	})
	t.Run("stale delivery", func(t *testing.T) {
		engine, store, api, clock := manualFixture(t)
		var sent atomic.Int64
		engine.NotifyInteractive = func(context.Context, Config, Delivery) error {
			sent.Add(1)
			return errors.New("delivery unavailable")
		}
		if err := engine.CheckNow(context.Background()); err == nil {
			t.Fatal("expected first notification delivery to fail")
		}
		store.mu.Lock()
		store.state.Rules[0].Weekly, store.state.Rules[0].Monthly = false, true
		store.mu.Unlock()
		clock.Add(time.Minute)
		if err := engine.processDeliveries(context.Background()); err != nil {
			t.Fatal(err)
		}
		state := mustEngineState(t, store)
		if sent.Load() != 1 || state.Deliveries[0].Status != "skipped" || state.ManualRequests[0].Status != "invalid" || len(api.resetCalls) != 0 {
			t.Fatal("stale authorization buttons were resent after the configured target windows changed")
		}
	})
	t.Run("unbound targets and group without permissions", func(t *testing.T) {
		for _, group := range []bool{false, true} {
			engine, store, api, _ := manualFixture(t)
			if group {
				store.cfg.Telegram.ChatID = "-42"
			} else {
				store.state.Rules[0].SubscriptionRefs = nil
			}
			var ordinary atomic.Int64
			engine.Notify = func(context.Context, Config, string, string) error { ordinary.Add(1); return nil }
			if err := engine.CheckNow(context.Background()); err != nil {
				t.Fatal(err)
			}
			state := mustEngineState(t, store)
			if len(state.ManualRequests) != 0 || len(state.Actions) != 0 || len(api.resetCalls) != 0 || ordinary.Load() != 1 {
				t.Fatal("unsafe interactive scope was generated instead of an ordinary notification")
			}
		}
	})
}

func TestManualLargeSelectionRetainsAllTargetsAndReviewableNotice(t *testing.T) {
	engine, store, api, clock := manualFixture(t)
	rule := &store.state.Rules[0]
	rule.SubscriptionIDs = nil
	rule.SubscriptionRefs = make(map[string]SubscriptionRef)
	for id := int64(10); id <= 210; id++ {
		rule.SubscriptionIDs = append(rule.SubscriptionIDs, id)
		rule.SubscriptionRefs[fmt.Sprint(id)] = SubscriptionRef{UserID: id, GroupID: 2}
		api.subscriptions[id] = Subscription{ID: id, UserID: id, GroupID: 2, Status: "active", StartsAt: clock.Now().Add(-time.Hour), ExpiresAt: clock.Now().Add(24 * time.Hour)}
	}
	var notice string
	engine.NotifyInteractive = func(_ context.Context, _ Config, delivery Delivery) error { notice = delivery.Message; return nil }
	request := detectManualRequest(t, engine, store, api)
	if len(request.Targets) != 201 || !strings.Contains(notice, "选定订阅共 201 个") || !strings.Contains(notice, "其余 181 条") || !strings.Contains(notice, request.ExpiresAt.Format(time.RFC3339)) || !strings.Contains(notice, "不延长有效期") || !strings.Contains(notice, request.ID) || utf8.RuneCountInString(notice) > 4096 {
		t.Fatal("large-selection notice lost its complete count, expiry, or review scope")
	}
	decision, err := engine.DecideManualReset(context.Background(), manualCallback(request, "r"))
	if err != nil || !decision.Accepted {
		t.Fatal("large selection was subject to a new business limit")
	}
	if err := engine.ProcessManualReset(context.Background(), request.ID); err != nil {
		t.Fatal(err)
	}
	seen := make(map[int64]bool)
	for _, call := range api.resetCalls {
		if len(call.ids) > 100 {
			t.Fatal("upstream batch size was exceeded")
		}
		for _, id := range call.ids {
			if seen[id] {
				t.Fatal("large selection repeated a target")
			}
			seen[id] = true
		}
	}
	if len(api.resetCalls) != 3 || len(seen) != 201 {
		t.Fatal("large selection did not retain every target across transport batches")
	}
}
