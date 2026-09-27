package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

type receiverFakeEngine struct {
	store     *memoryEngineStore
	decide    func(TelegramCallback) (ManualDecision, error)
	process   func(string) error
	decisions int
	processed int
}

func (e *receiverFakeEngine) DecideManualReset(_ context.Context, callback TelegramCallback) (ManualDecision, error) {
	e.decisions++
	if e.decide != nil {
		return e.decide(callback)
	}
	decision := ManualDecision{Text: "已处理", RequestID: receiverTestRequestID}
	err := e.store.Update(func(state *State) error {
		if state.ManualRequests[0].Status == "pending" {
			state.ManualRequests[0].Status = "processing"
			decision.Text, decision.Accepted = "已批准重置", true
		}
		return nil
	})
	return decision, err
}

func (e *receiverFakeEngine) ProcessManualReset(_ context.Context, id string) error {
	e.processed++
	if e.process != nil {
		return e.process(id)
	}
	return nil
}

const receiverTestRequestID = "0123456789abcdef0123456789abcdef"

func receiverTestFixture() (*TelegramReceiver, *memoryEngineStore, *receiverFakeEngine) {
	cfg := DefaultConfig()
	cfg.Telegram = TelegramConfig{Enabled: true, BotToken: "123:secret-token", ChatID: "42"}
	store := &memoryEngineStore{cfg: cfg, state: State{
		ManualRequests:   []ManualRequest{{ID: receiverTestRequestID, Status: "pending", ExpiresAt: time.Now().Add(time.Hour)}},
		TelegramReceiver: TelegramReceiverState{BotFingerprint: telegramBotFingerprint(cfg.Telegram.BotToken)},
	}}
	engine := &receiverFakeEngine{store: store}
	receiver := newTelegramReceiver(store, engine, nil)
	return receiver, store, engine
}

func receiverCallbackJSON(updateID int, extra string) string {
	return `{"update_id":` + strconv.Itoa(updateID) + `,"callback_query":{"id":"query-` + strconv.Itoa(updateID) + `","from":{"id":42},"message":{"message_id":9,"chat":{"id":42,"type":"private"}},"data":"qw:r:` + receiverTestRequestID + `"` + extra + `}}`
}

func receiverUseTransport(r *TelegramReceiver, transport notificationRoundTripper) {
	r.newHTTP = func(TelegramConfig, time.Duration) (*http.Client, func(), error) {
		return &http.Client{Transport: transport}, func() {}, nil
	}
}

func TestTelegramReceiverClaimsBeforeAnswerAndDoesNotReplay(t *testing.T) {
	r, store, engine := receiverTestFixture()
	order := []string{}
	engine.process = func(id string) error {
		if id != receiverTestRequestID {
			t.Fatal("receiver used callback data as subscription ID")
		}
		order = append(order, "process")
		return nil
	}
	receiverUseTransport(r, func(req *http.Request) (*http.Response, error) {
		method := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
		order = append(order, method)
		if method == "getUpdates" {
			var payload struct {
				Offset         int64    `json:"offset"`
				Timeout        int      `json:"timeout"`
				AllowedUpdates []string `json:"allowed_updates"`
			}
			if json.NewDecoder(req.Body).Decode(&payload) != nil || payload.Timeout != 30 || !reflect.DeepEqual(payload.AllowedUpdates, []string{"callback_query"}) {
				t.Fatal("receiver enabled updates other than callback queries")
			}
			return telegramTestResponse(200, `{"ok":true,"result":[`+receiverCallbackJSON(3, "")+`,`+receiverCallbackJSON(4, "")+`]}`), nil
		}
		state, _ := store.Snapshot()
		if state.ManualRequests[0].Status != "processing" {
			t.Fatal("receiver answered before the reset decision was durable")
		}
		if method == "editMessageReplyMarkup" {
			var payload struct {
				ChatID      int64          `json:"chat_id"`
				ReplyMarkup telegramMarkup `json:"reply_markup"`
			}
			if json.NewDecoder(req.Body).Decode(&payload) != nil || payload.ChatID != 42 || len(payload.ReplyMarkup.InlineKeyboard) != 0 {
				t.Fatal("receiver did not remove only the approved message buttons")
			}
		}
		return telegramTestResponse(200, `{"ok":true,"result":true}`), nil
	})
	if _, err := r.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"getUpdates", "answerCallbackQuery", "editMessageReplyMarkup", "process", "answerCallbackQuery", "editMessageReplyMarkup"}) || engine.processed != 1 {
		t.Fatalf("approval order or duplicate handling: %#v, %d resets", order, engine.processed)
	}
	state, _ := store.Snapshot()
	if state.TelegramReceiver.Offset != 5 {
		t.Fatal("receiver did not persist its update position")
	}
}

func TestTelegramReceiverIgnoreRemovesButtonsWithoutReset(t *testing.T) {
	r, store, engine := receiverTestFixture()
	engine.decide = func(callback TelegramCallback) (ManualDecision, error) {
		if callback.Data != "qw:i:"+receiverTestRequestID || callback.ChatID != 42 || callback.ChatType != "private" || callback.FromID != 42 {
			t.Fatalf("receiver changed decision scope: %#v", callback)
		}
		return ManualDecision{Text: "已忽略", RequestID: receiverTestRequestID}, store.Update(func(state *State) error {
			state.ManualRequests[0].Status = "ignored"
			return nil
		})
	}
	methods := []string{}
	receiverUseTransport(r, func(req *http.Request) (*http.Response, error) {
		method := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
		methods = append(methods, method)
		if method == "getUpdates" {
			return telegramTestResponse(200, `{"ok":true,"result":[`+strings.Replace(receiverCallbackJSON(0, ""), "qw:r:", "qw:i:", 1)+`]}`), nil
		}
		return telegramTestResponse(200, `{"ok":true,"result":true}`), nil
	})
	if _, err := r.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if engine.processed != 0 || !reflect.DeepEqual(methods, []string{"getUpdates", "answerCallbackQuery", "editMessageReplyMarkup"}) {
		t.Fatalf("ignore caused a reset or left buttons: %#v", methods)
	}
}

func TestTelegramReceiverRejectsInlineMissingActorAndMissingChat(t *testing.T) {
	for _, query := range []string{
		`{"id":"inline","from":{"id":42},"inline_message_id":"inline-private","data":"qw:r:` + receiverTestRequestID + `"}`,
		`{"id":"missing-actor","message":{"message_id":9,"chat":{"id":42,"type":"private"}},"data":"qw:r:` + receiverTestRequestID + `"}`,
		`{"id":"missing-chat","from":{"id":42},"message":{"message_id":9},"data":"qw:r:` + receiverTestRequestID + `"}`,
	} {
		r, _, engine := receiverTestFixture()
		methods := []string{}
		receiverUseTransport(r, func(req *http.Request) (*http.Response, error) {
			method := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
			methods = append(methods, method)
			if method == "getUpdates" {
				return telegramTestResponse(200, `{"ok":true,"result":[{"update_id":0,"callback_query":`+query+`}]}`), nil
			}
			return telegramTestResponse(200, `{"ok":true,"result":true}`), nil
		})
		if _, err := r.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
		if engine.decisions != 0 || engine.processed != 0 || !reflect.DeepEqual(methods, []string{"getUpdates", "answerCallbackQuery"}) {
			t.Fatal("receiver accepted a callback without a concrete actor and message chat")
		}
	}
}

func TestTelegramReceiverKeepsUpdateWhenDecisionStorageFails(t *testing.T) {
	r, store, engine := receiverTestFixture()
	engine.decide = func(TelegramCallback) (ManualDecision, error) {
		return ManualDecision{}, errors.New("private-sql-password storage failed")
	}
	calls := 0
	receiverUseTransport(r, func(req *http.Request) (*http.Response, error) {
		calls++
		return telegramTestResponse(200, `{"ok":true,"result":[`+receiverCallbackJSON(0, "")+`]}`), nil
	})
	_, err := r.poll(context.Background())
	state, _ := store.Snapshot()
	if err == nil || strings.Contains(err.Error(), "private-sql-password") || calls != 1 || state.TelegramReceiver.Offset != 0 || engine.processed != 0 {
		t.Fatalf("receiver swallowed or exposed failed storage: %v, calls %d, offset %d", err, calls, state.TelegramReceiver.Offset)
	}
}

func TestTelegramReceiverOffsetFailureRetainsDecisionAndAvoidsResetReplay(t *testing.T) {
	r, store, engine := receiverTestFixture()
	store.reject = func(state State) bool { return state.TelegramReceiver.Offset == 1 }
	receiverUseTransport(r, func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/getUpdates") {
			return telegramTestResponse(200, `{"ok":true,"result":[`+receiverCallbackJSON(0, "")+`]}`), nil
		}
		return telegramTestResponse(200, `{"ok":true,"result":true}`), nil
	})
	if _, err := r.poll(context.Background()); err == nil {
		t.Fatal("update storage failure was ignored")
	}
	state, _ := store.Snapshot()
	if state.TelegramReceiver.Offset != 0 || state.ManualRequests[0].Status != "processing" || engine.processed != 0 {
		t.Fatal("receiver advanced an undurable update or lost its decision")
	}
	store.reject = nil
	// Another pending notification keeps the long poll active while the already
	// approved action awaits the engine's regular queue processing.
	store.state.ManualRequests = append(store.state.ManualRequests, ManualRequest{Status: "pending", ExpiresAt: time.Now().Add(time.Hour)})
	if _, err := r.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, _ = store.Snapshot()
	if state.TelegramReceiver.Offset != 1 || engine.processed != 0 {
		t.Fatal("replayed callback executed an already claimed reset")
	}
}

func TestTelegramReceiverPollGatingAndBotReplacement(t *testing.T) {
	for _, change := range []func(*memoryEngineStore){
		func(store *memoryEngineStore) { store.cfg.AutoResetEnabled = true },
		func(store *memoryEngineStore) { store.cfg.Telegram.Enabled = false },
		func(store *memoryEngineStore) { store.cfg.Telegram.BotToken = "invalid" },
		func(store *memoryEngineStore) { store.state.ManualRequests[0].Status = "ignored" },
		func(store *memoryEngineStore) { store.state.ManualRequests[0].ExpiresAt = time.Now().Add(-time.Minute) },
	} {
		r, store, _ := receiverTestFixture()
		change(store)
		receiverUseTransport(r, func(*http.Request) (*http.Response, error) {
			t.Fatal("inactive Telegram confirmation must not poll")
			return nil, nil
		})
		if delay, err := r.poll(context.Background()); err != nil || delay != 2*time.Second {
			t.Fatalf("inactive Telegram receiver did not wait locally: %v, %v", delay, err)
		}
	}
	r, store, _ := receiverTestFixture()
	store.state.TelegramReceiver.Offset = 999
	store.cfg.Telegram.BotToken = "456:replacement-token"
	receiverUseTransport(r, func(req *http.Request) (*http.Response, error) {
		var payload struct {
			Offset int64 `json:"offset"`
		}
		if json.NewDecoder(req.Body).Decode(&payload) != nil || payload.Offset != 0 || !strings.Contains(req.URL.Path, "456:replacement-token") {
			t.Fatal("receiver reused another bot's update offset")
		}
		return telegramTestResponse(200, `{"ok":true,"result":[]}`), nil
	})
	if _, err := r.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestTelegramReceiverConflictIsVisibleWithoutChangingWebhook(t *testing.T) {
	r, store, _ := receiverTestFixture()
	methods := []string{}
	receiverUseTransport(r, func(req *http.Request) (*http.Response, error) {
		methods = append(methods, req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:])
		return telegramTestResponse(409, `{"ok":false,"error_code":409,"description":"https://private-webhook secret-token proxy-password"}`), nil
	})
	_, err := r.poll(context.Background())
	state, _ := store.Snapshot()
	if err == nil || state.TelegramReceiver.Status != "conflict" || !strings.Contains(state.TelegramReceiver.LastError, "Webhook") || !reflect.DeepEqual(methods, []string{"getUpdates"}) {
		t.Fatal("receiver hid the polling conflict or changed the webhook")
	}
	for _, secret := range []string{"secret-token", "proxy-password", "https://private-webhook"} {
		if strings.Contains(err.Error(), secret) || strings.Contains(state.TelegramReceiver.LastError, secret) {
			t.Fatal("receiver disclosed Telegram's error description")
		}
	}
}

func TestTelegramReceiverHonorsRetryAfterAndDeduplicatesLogs(t *testing.T) {
	r, _, _ := receiverTestFixture()
	var logs strings.Builder
	r.logger = slog.New(slog.NewTextHandler(&logs, nil))
	receiverUseTransport(r, func(req *http.Request) (*http.Response, error) {
		return telegramTestResponse(429, `{"ok":false,"error_code":429,"parameters":{"retry_after":77},"description":"secret-token"}`), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waits := 0
	r.wait = func(_ context.Context, delay time.Duration) error {
		if delay != 77*time.Second {
			t.Fatalf("Telegram Retry-After ignored: %v", delay)
		}
		waits++
		if waits == 2 {
			cancel()
			return context.Canceled
		}
		return nil
	}
	r.Run(ctx)
	if waits != 2 || strings.Count(logs.String(), "Telegram 回调接收暂停") != 1 || strings.Contains(logs.String(), "secret-token") {
		t.Fatalf("repeated Telegram failures spammed or disclosed logs: %q", logs.String())
	}
}

func TestTelegramReceiverLongPollStopsOnCancellation(t *testing.T) {
	r, _, _ := receiverTestFixture()
	started := make(chan struct{})
	receiverUseTransport(r, func(req *http.Request) (*http.Response, error) {
		close(started)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Run(ctx) }()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Telegram long poll ignored shutdown cancellation")
	}
}

func TestTelegramReceiverCallbackAnswerFailureDoesNotLoseApprovedReset(t *testing.T) {
	r, store, engine := receiverTestFixture()
	receiverUseTransport(r, func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/getUpdates") {
			return telegramTestResponse(200, `{"ok":true,"result":[`+receiverCallbackJSON(0, "")+`]}`), nil
		}
		return &http.Response{StatusCode: 500, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":false}`))}, nil
	})
	engine.process = func(string) error { return errEngineBusy }
	if _, err := r.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, _ := store.Snapshot()
	if engine.processed != 1 || state.TelegramReceiver.Offset != 1 || state.ManualRequests[0].Status != "processing" {
		t.Fatal("callback response failure or busy engine lost the approved reset")
	}
}
