package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"time"
	"unicode/utf8"
)

type manualResetHandler interface {
	DecideManualReset(context.Context, TelegramCallback) (ManualDecision, error)
	ProcessManualReset(context.Context, string) error
}

type TelegramReceiver struct {
	store   engineStore
	engine  manualResetHandler
	logger  *slog.Logger
	newHTTP func(TelegramConfig, time.Duration) (*http.Client, func(), error)
	now     func() time.Time
	wait    func(context.Context, time.Duration) error
}

type telegramUpdate struct {
	UpdateID int64 `json:"update_id"`
	Callback *struct {
		ID   string `json:"id"`
		From *struct {
			ID int64 `json:"id"`
		} `json:"from"`
		Message *struct {
			MessageID int64 `json:"message_id"`
			Chat      *struct {
				ID       int64  `json:"id"`
				Username string `json:"username"`
				Type     string `json:"type"`
			} `json:"chat"`
		} `json:"message"`
		InlineMessageID string `json:"inline_message_id"`
		Data            string `json:"data"`
	} `json:"callback_query"`
}

func NewTelegramReceiver(store *Store, engine *Engine, logger *slog.Logger) *TelegramReceiver {
	return newTelegramReceiver(store, engine, logger)
}

func newTelegramReceiver(store engineStore, engine manualResetHandler, logger *slog.Logger) *TelegramReceiver {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &TelegramReceiver{store: store, engine: engine, logger: logger, newHTTP: newTelegramHTTPClient, now: time.Now,
		wait: func(ctx context.Context, delay time.Duration) error {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		}}
}

func (r *TelegramReceiver) Run(ctx context.Context) {
	attempts := 0
	previousError := ""
	for ctx.Err() == nil {
		delay, err := r.poll(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			attempts++
			delay = notificationBackoff(err, attempts)
			if err.Error() != previousError {
				r.logger.Warn("Telegram 回调接收暂停", "error", err)
				previousError = err.Error()
			}
		} else {
			attempts, previousError = 0, ""
		}
		if r.wait(ctx, delay) != nil {
			return
		}
	}
}

func telegramBotFingerprint(token string) string {
	if token == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (r *TelegramReceiver) poll(ctx context.Context) (time.Duration, error) {
	cfg, err := r.store.Config()
	if err != nil {
		return 0, errors.New("无法读取 Telegram 接收配置")
	}
	state, err := r.store.Snapshot()
	if err != nil {
		return 0, errors.New("无法读取 Telegram 接收状态")
	}
	fingerprint := telegramBotFingerprint(cfg.Telegram.BotToken)
	if state.TelegramReceiver.BotFingerprint != fingerprint {
		receiverState := TelegramReceiverState{BotFingerprint: fingerprint, Status: "idle", CheckedAt: r.now()}
		if err := r.store.Update(func(current *State) error {
			current.TelegramReceiver = receiverState
			return nil
		}); err != nil {
			return 0, errors.New("无法保存 Telegram 接收状态")
		}
		state.TelegramReceiver = receiverState
	}
	pending := false
	for _, request := range state.ManualRequests {
		if request.Status == "pending" && request.ExpiresAt.After(r.now()) {
			pending = true
			break
		}
	}
	if !cfg.Telegram.Enabled || !validTelegramToken(cfg.Telegram.BotToken) || cfg.AutoResetEnabled || !pending {
		status := "idle"
		if !cfg.Telegram.Enabled {
			status = "disabled"
		}
		if state.TelegramReceiver.Status == status && state.TelegramReceiver.LastError == "" {
			return 2 * time.Second, nil
		}
		return 2 * time.Second, r.setStatus(fingerprint, status, "")
	}
	client, closeClient, err := r.newHTTP(cfg.Telegram, telegramPollTimeout)
	if err != nil {
		if saveErr := r.setStatus(fingerprint, "error", err.Error()); saveErr != nil {
			return 0, saveErr
		}
		return 0, err
	}
	defer closeClient()
	var updates []telegramUpdate
	err = telegramRequest(ctx, cfg.Telegram, client, "getUpdates", struct {
		Offset         int64    `json:"offset"`
		Timeout        int      `json:"timeout"`
		Limit          int      `json:"limit"`
		AllowedUpdates []string `json:"allowed_updates"`
	}{state.TelegramReceiver.Offset, 30, 100, []string{"callback_query"}}, telegramPollTimeout, telegramUpdatesLimit, &updates)
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if err != nil {
		status, message := "error", err.Error()
		var apiError *APIError
		if errors.As(err, &apiError) && apiError.Status == http.StatusConflict {
			status = "conflict"
			message = "该 Telegram Bot 已设置 Webhook 或有其他程序接收更新，请使用独立 Bot 或自行调整原接收程序；本服务不会修改 Webhook"
		}
		if saveErr := r.setStatus(fingerprint, status, message); saveErr != nil {
			return 0, saveErr
		}
		return 0, err
	}
	if err := r.setStatus(fingerprint, "polling", ""); err != nil {
		return 0, err
	}
	sort.SliceStable(updates, func(i, j int) bool { return updates[i].UpdateID < updates[j].UpdateID })
	for _, update := range updates {
		if update.UpdateID < state.TelegramReceiver.Offset {
			continue
		}
		if update.UpdateID < 0 || update.UpdateID == math.MaxInt64 {
			return 0, errors.New("Telegram 返回的更新标识无效")
		}
		if err := r.handleUpdate(ctx, cfg, client, fingerprint, update); err != nil {
			return 0, err
		}
		state.TelegramReceiver.Offset = update.UpdateID + 1
	}
	// A real long poll normally waits on Telegram. A small local delay also
	// prevents a malformed server or empty backlog from causing a tight loop.
	return time.Second, nil
}

func (r *TelegramReceiver) setStatus(fingerprint, status, message string) error {
	if err := r.store.Update(func(state *State) error {
		if state.TelegramReceiver.BotFingerprint != fingerprint {
			return errors.New("Telegram Bot 配置已改变")
		}
		state.TelegramReceiver.Status, state.TelegramReceiver.LastError = status, message
		state.TelegramReceiver.CheckedAt = r.now()
		return nil
	}); err != nil {
		return errors.New("无法保存 Telegram 接收状态")
	}
	return nil
}

func (r *TelegramReceiver) handleUpdate(ctx context.Context, cfg Config, client *http.Client, fingerprint string, update telegramUpdate) error {
	decision := ManualDecision{}
	var callback TelegramCallback
	if query := update.Callback; query != nil {
		callback.UpdateID, callback.QueryID, callback.Data = update.UpdateID, query.ID, query.Data
		decision.Text = "此按钮无法处理，请使用指定聊天中的通知"
		if query.ID != "" && query.InlineMessageID == "" && query.From != nil && query.From.ID > 0 && query.Message != nil && query.Message.Chat != nil && query.Message.Chat.ID != 0 && query.Message.MessageID > 0 && utf8.ValidString(query.Data) && len(query.Data) <= 64 {
			callback.FromID, callback.MessageID = query.From.ID, query.Message.MessageID
			callback.ChatID, callback.ChatUsername, callback.ChatType = query.Message.Chat.ID, query.Message.Chat.Username, query.Message.Chat.Type
			var err error
			decision, err = r.engine.DecideManualReset(ctx, callback)
			if err != nil {
				return errors.New("无法保存 Telegram 手动重置决策，本次更新将重试")
			}
		}
		if query.ID != "" {
			if utf8.RuneCountInString(decision.Text) > 200 {
				decision.Text = string([]rune(decision.Text)[:200])
			}
			if err := telegramRequest(ctx, cfg.Telegram, client, "answerCallbackQuery", struct {
				ID   string `json:"callback_query_id"`
				Text string `json:"text"`
			}{query.ID, decision.Text}, notificationTimeout, telegramResponseLimit, nil); err != nil && ctx.Err() == nil {
				r.logger.Warn("Telegram 按钮响应发送失败；已保存的决策会继续处理")
			}
		}
		if decision.RequestID != "" {
			if err := telegramRequest(ctx, cfg.Telegram, client, "editMessageReplyMarkup", struct {
				ChatID      int64          `json:"chat_id"`
				MessageID   int64          `json:"message_id"`
				ReplyMarkup telegramMarkup `json:"reply_markup"`
			}{callback.ChatID, callback.MessageID, telegramMarkup{InlineKeyboard: [][]telegramButton{}}}, notificationTimeout, telegramResponseLimit, nil); err != nil && ctx.Err() == nil {
				r.logger.Warn("Telegram 通知按钮清理失败；重复点击不会重复重置")
			}
		}
	}
	if err := r.store.Update(func(state *State) error {
		if state.TelegramReceiver.BotFingerprint != fingerprint {
			return errors.New("Telegram Bot 配置已改变")
		}
		state.TelegramReceiver.Offset = max(state.TelegramReceiver.Offset, update.UpdateID+1)
		return nil
	}); err != nil {
		return errors.New("无法保存 Telegram 更新位置，本次更新将重试")
	}
	if decision.Accepted {
		if err := r.engine.ProcessManualReset(ctx, decision.RequestID); err != nil && !errors.Is(err, errEngineBusy) && ctx.Err() == nil {
			r.logger.Warn("Telegram 手动重置执行未全部完成，请在管理页面核对结果")
		}
	}
	return nil
}
