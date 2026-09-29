package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const manualRequestTTL = 24 * time.Hour

func manualConnectionFingerprint(cfg Config) string {
	return hashValue([]any{cfg.BaseURL, cfg.AdminAPIKey})
}

func manualTelegramFingerprint(cfg TelegramConfig) string {
	return hashValue([]any{cfg.Enabled, cfg.BotToken, cfg.ChatID, uniqueSortedIDs(cfg.AllowedUserIDs)})
}

func manualRulesFingerprint(rules []Rule, ids []string) string {
	ordered := append([]string(nil), ids...)
	sort.Strings(ordered)
	byID := make(map[string]Rule, len(rules))
	for _, rule := range rules {
		byID[rule.ID] = rule
	}
	payload := make([]any, 0, len(ordered))
	for _, id := range ordered {
		rule, found := byID[id]
		if !found {
			return ""
		}
		refs := make(map[string]SubscriptionRef)
		for _, subID := range uniqueSortedIDs(rule.SubscriptionIDs) {
			key := strconv.FormatInt(subID, 10)
			if ref, found := rule.SubscriptionRefs[key]; found {
				refs[key] = ref
			}
		}
		payload = append(payload, []any{id, rule.Enabled, uniqueSortedIDs(rule.AccountIDs), uniqueSortedIDs(rule.SubscriptionIDs), refs, ResetMask{Daily: rule.Daily, Weekly: rule.Weekly, Monthly: rule.Monthly}, rule.NotifyTelegram})
	}
	return hashValue(payload)
}

func manualTelegramReady(cfg TelegramConfig) bool {
	if !cfg.Enabled || !validTelegramToken(cfg.BotToken) {
		return false
	}
	chatID, err := strconv.ParseInt(cfg.ChatID, 10, 64)
	if err == nil && chatID > 0 {
		return true
	}
	return (err == nil && chatID < 0 || strings.HasPrefix(cfg.ChatID, "@")) && len(uniqueSortedIDs(cfg.AllowedUserIDs)) > 0
}

// A single request covers all selected subscriptions affected by this sampling
// pass. Overlapping rules contribute one union of windows for each target.
func buildManualRequest(cfg Config, events []Event, eventRules map[string][]Rule, now time.Time) (*ManualRequest, error) {
	if cfg.AutoResetEnabled || !manualTelegramReady(cfg.Telegram) {
		return nil, nil
	}
	type target struct {
		value    ManualTarget
		rules    []string
		events   []string
		conflict bool
	}
	targets := make(map[int64]*target)
	for _, event := range events {
		if event.Source != "stored" {
			continue
		}
		for _, rule := range eventRules[event.ID] {
			if !rule.Enabled || !rule.NotifyTelegram || (!rule.Daily && !rule.Weekly && !rule.Monthly) {
				continue
			}
			for _, id := range uniqueSortedIDs(rule.SubscriptionIDs) {
				ref, found := rule.SubscriptionRefs[strconv.FormatInt(id, 10)]
				if !found || ref.UserID <= 0 || ref.GroupID <= 0 {
					continue
				}
				if targets[id] == nil {
					targets[id] = &target{value: ManualTarget{SubscriptionID: id, Ref: ref}}
				}
				item := targets[id]
				item.conflict = item.conflict || item.value.Ref != ref
				item.value.Mask.Daily = item.value.Mask.Daily || rule.Daily
				item.value.Mask.Weekly = item.value.Mask.Weekly || rule.Weekly
				item.value.Mask.Monthly = item.value.Mask.Monthly || rule.Monthly
				if !containsString(item.rules, rule.ID) {
					item.rules = append(item.rules, rule.ID)
				}
				if !containsString(item.events, event.ID) {
					item.events = append(item.events, event.ID)
				}
			}
		}
	}
	request := &ManualRequest{Status: "pending", ChatID: cfg.Telegram.ChatID, CreatedAt: now, ExpiresAt: now.Add(manualRequestTTL), Targets: []ManualTarget{}, RuleIDs: []string{}, EventIDs: []string{}, ActionIDs: []string{}}
	for _, item := range targets {
		if item.conflict {
			continue
		}
		request.Targets = append(request.Targets, item.value)
		for _, id := range item.rules {
			if !containsString(request.RuleIDs, id) {
				request.RuleIDs = append(request.RuleIDs, id)
			}
		}
		for _, id := range item.events {
			if !containsString(request.EventIDs, id) {
				request.EventIDs = append(request.EventIDs, id)
			}
		}
	}
	if len(request.Targets) == 0 {
		return nil, nil
	}
	sort.Slice(request.Targets, func(i, j int) bool { return request.Targets[i].SubscriptionID < request.Targets[j].SubscriptionID })
	sort.Strings(request.RuleIDs)
	sort.Strings(request.EventIDs)
	rules := make([]Rule, 0)
	for _, sourceRules := range eventRules {
		for _, rule := range sourceRules {
			rules = append(rules, rule)
		}
	}
	request.ConnectionFingerprint = manualConnectionFingerprint(cfg)
	request.RulesFingerprint = manualRulesFingerprint(rules, request.RuleIDs)
	request.TelegramFingerprint = manualTelegramFingerprint(cfg.Telegram)
	entropy := make([]byte, 16)
	if _, err := rand.Read(entropy); err != nil {
		return nil, err
	}
	request.ID = hex.EncodeToString(entropy)
	return request, nil
}

func manualRequestIndex(state *State, id string) int {
	for i := range state.ManualRequests {
		if state.ManualRequests[i].ID == id {
			return i
		}
	}
	return -1
}

func manualRequestScopeValid(cfg Config, state *State, request ManualRequest) bool {
	if len(request.EventIDs) == 0 || len(request.RuleIDs) == 0 || len(request.Targets) == 0 || request.ChatID != cfg.Telegram.ChatID || request.ConnectionFingerprint != manualConnectionFingerprint(cfg) || request.TelegramFingerprint != manualTelegramFingerprint(cfg.Telegram) || request.RulesFingerprint != manualRulesFingerprint(state.Rules, request.RuleIDs) {
		return false
	}
	events := make(map[string]Event)
	for _, event := range state.Events {
		events[event.ID] = event
	}
	rules := make(map[string]Rule)
	for _, rule := range state.Rules {
		rules[rule.ID] = rule
	}
	for _, id := range request.EventIDs {
		event, found := events[id]
		if !found || event.Source != "stored" {
			return false
		}
		matched := false
		for _, ruleID := range request.RuleIDs {
			matched = matched || containsID(rules[ruleID].AccountIDs, event.AccountID)
		}
		if !matched {
			return false
		}
	}
	seen := make(map[int64]bool)
	for _, target := range request.Targets {
		if target.SubscriptionID <= 0 || target.Ref.UserID <= 0 || target.Ref.GroupID <= 0 || seen[target.SubscriptionID] || (!target.Mask.Daily && !target.Mask.Weekly && !target.Mask.Monthly) {
			return false
		}
		seen[target.SubscriptionID] = true
		mask := ResetMask{}
		for _, ruleID := range request.RuleIDs {
			rule := rules[ruleID]
			if !rule.Enabled || !rule.NotifyTelegram {
				return false
			}
			if !containsID(rule.SubscriptionIDs, target.SubscriptionID) {
				continue
			}
			ref, found := rule.SubscriptionRefs[strconv.FormatInt(target.SubscriptionID, 10)]
			if !found || ref.UserID <= 0 || ref.GroupID <= 0 {
				continue
			}
			if ref != target.Ref {
				return false
			}
			mask.Daily = mask.Daily || rule.Daily
			mask.Weekly = mask.Weekly || rule.Weekly
			mask.Monthly = mask.Monthly || rule.Monthly
		}
		if mask != target.Mask {
			return false
		}
	}
	return true
}

func supersedeManualRequests(state *State, events []Event, eventRules map[string][]Rule, now time.Time) {
	targets := make(map[int64]bool)
	for _, event := range events {
		for _, rule := range eventRules[event.ID] {
			if !rule.Daily && !rule.Weekly && !rule.Monthly {
				continue
			}
			for _, id := range rule.SubscriptionIDs {
				targets[id] = true
			}
		}
	}
	for i := range state.ManualRequests {
		request := &state.ManualRequests[i]
		if request.Status != "pending" && request.Status != "processing" && request.Status != "partial" && request.Status != "failed" {
			continue
		}
		for _, target := range request.Targets {
			if targets[target.SubscriptionID] {
				invalidateManualRequest(state, i, "superseded", "目标订阅已有更新的归零事件，请使用最新通知", now)
				break
			}
		}
	}
}

func invalidateManualRequest(state *State, index int, status, message string, now time.Time) {
	request := &state.ManualRequests[index]
	request.Status, request.LastError = status, message
	for i := range state.Actions {
		action := &state.Actions[i]
		if action.ManualRequestID == request.ID && action.Status == "pending" {
			action.Status, action.LastError, action.UpdatedAt = "skipped", message, now
			action.Results = failedItems(action.SubscriptionIDs, "skipped: "+message)
		}
	}
}

func parseManualCallbackData(data string) (string, string, bool) {
	if len(data) != len("qw:r:")+32 || (!strings.HasPrefix(data, "qw:r:") && !strings.HasPrefix(data, "qw:i:")) {
		return "", "", false
	}
	id := data[5:]
	decoded, err := hex.DecodeString(id)
	if err != nil || len(decoded) != 16 || strings.ToLower(id) != id {
		return "", "", false
	}
	return data[3:4], id, true
}

func manualCallbackAuthorized(cfg TelegramConfig, request ManualRequest, callback TelegramCallback) bool {
	if callback.FromID <= 0 || request.ChatID != cfg.ChatID {
		return false
	}
	if strings.HasPrefix(cfg.ChatID, "@") {
		if !strings.EqualFold(strings.TrimPrefix(cfg.ChatID, "@"), callback.ChatUsername) || (callback.ChatType != "group" && callback.ChatType != "supergroup") {
			return false
		}
		return containsID(cfg.AllowedUserIDs, callback.FromID)
	}
	chatID, err := strconv.ParseInt(cfg.ChatID, 10, 64)
	if err != nil || chatID != callback.ChatID {
		return false
	}
	if chatID > 0 {
		return callback.ChatType == "private" && callback.FromID == chatID
	}
	return chatID < 0 && (callback.ChatType == "group" || callback.ChatType == "supergroup") && containsID(cfg.AllowedUserIDs, callback.FromID)
}

func (e *Engine) manualConfigUpdate(cfg Config, update func(*State) error) error {
	if scoped, ok := e.store.(interface {
		UpdateForManualConfig(Config, func(*State) error) error
	}); ok {
		return scoped.UpdateForManualConfig(cfg, update)
	}
	if scoped, ok := e.store.(interface {
		UpdateForConfig(Config, func(*State) error) error
	}); ok {
		return scoped.UpdateForConfig(cfg, update)
	}
	return e.store.Update(update)
}

// Claim the decision durably before any reset request is sent. An unauthorized
// click leaves a pending request available to its intended recipient.
func (e *Engine) DecideManualReset(ctx context.Context, callback TelegramCallback) (ManualDecision, error) {
	decision := ManualDecision{Text: "此重置请求无效"}
	if err := ctx.Err(); err != nil {
		return decision, err
	}
	choice, id, valid := parseManualCallbackData(callback.Data)
	if !valid {
		return decision, nil
	}
	cfg, err := e.store.Config()
	if err != nil {
		return decision, err
	}
	err = e.manualConfigUpdate(cfg, func(state *State) error {
		if cfg.AutoResetEnabled {
			decision.Text = "全局自动重置已开启，手动重置按钮不可用"
			return nil
		}
		index := manualRequestIndex(state, id)
		if index < 0 {
			return nil
		}
		request := &state.ManualRequests[index]
		if !manualCallbackAuthorized(cfg.Telegram, *request, callback) {
			decision.Text = "你无权处理此重置请求"
			return nil
		}
		if request.Status != "pending" {
			decision.Text, decision.RequestID = "此重置请求已处理或已失效，不会重复重置", id
			return nil
		}
		now := e.Now()
		if !request.ExpiresAt.After(now) {
			invalidateManualRequest(state, index, "expired", "手动重置请求已超过 24 小时有效期", now)
			decision.Text, decision.RequestID = "此重置请求已过期", id
			return nil
		}
		if !manualRequestScopeValid(cfg, state, *request) {
			invalidateManualRequest(state, index, "invalid", "连接、规则目标/周期或 Telegram 权限配置已改变", now)
			decision.Text, decision.RequestID = "配置已改变，请使用后续归零通知", id
			return nil
		}
		request.DecisionAt = now
		decision.RequestID = id
		if choice == "i" {
			request.Status = "ignored"
			decision.Text = "已忽略此次订阅重置"
			return nil
		}
		request.Status, request.ApprovedBy = "processing", callback.FromID
		actions := manualRequestActions(*request, now)
		for _, action := range actions {
			request.ActionIDs = append(request.ActionIDs, action.ID)
		}
		state.Actions = append(state.Actions, actions...)
		decision.Accepted, decision.Text = true, fmt.Sprintf("已确认，正在重置选定的 %d 个订阅", len(request.Targets))
		return nil
	})
	if err != nil {
		return ManualDecision{Text: "暂时无法保存选择，请稍后重试"}, err
	}
	return decision, err
}

func manualActionKey(request ManualRequest, actionID string) string {
	return "quota-watch.manual." + hashValue([]any{request.ID, request.ConnectionFingerprint, request.RulesFingerprint, request.TelegramFingerprint})[:24] + "." + hashValue(actionID)
}

func manualRequestActions(request ManualRequest, now time.Time) []Action {
	groups := make(map[string][]int64)
	masks := make(map[string]ResetMask)
	for _, target := range request.Targets {
		key := hashValue(target.Mask)
		groups[key] = append(groups[key], target.SubscriptionID)
		masks[key] = target.Mask
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	actions := make([]Action, 0)
	for _, key := range keys {
		ids := uniqueSortedIDs(groups[key])
		for start := 0; start < len(ids); start += 100 {
			subscriptions := append([]int64(nil), ids[start:min(start+100, len(ids))]...)
			id := "manual-" + hashValue([]any{request.ID, masks[key], subscriptions})
			actions = append(actions, Action{Mode: "manual", ManualRequestID: request.ID, ID: id, EventID: request.EventIDs[0], EventIDs: append([]string(nil), request.EventIDs...), RuleIDs: append([]string(nil), request.RuleIDs...), SubscriptionIDs: subscriptions, Mask: masks[key], IdempotencyKey: manualActionKey(request, id), NotificationChannels: []string{"telegram"}, Status: "pending", Results: []ResetItem{}, CreatedAt: now, UpdatedAt: now})
		}
	}
	return actions
}

func manualActionAllowed(cfg Config, state *State, action Action) bool {
	index := manualRequestIndex(state, action.ManualRequestID)
	if index < 0 || action.Mode != "manual" || !actionUsesStoredEvents(state, action) {
		return false
	}
	request := state.ManualRequests[index]
	if request.ApprovedBy <= 0 || request.DecisionAt.IsZero() || !containsString(request.ActionIDs, action.ID) || (request.Status != "processing" && request.Status != "succeeded" && request.Status != "partial" && request.Status != "failed" && request.Status != "unknown" && request.Status != "skipped") || !manualRequestScopeValid(cfg, state, request) || action.IdempotencyKey != manualActionKey(request, action.ID) {
		return false
	}
	if hashValue(uniqueStrings(action.EventIDs)) != hashValue(uniqueStrings(request.EventIDs)) || hashValue(uniqueStrings(action.RuleIDs)) != hashValue(uniqueStrings(request.RuleIDs)) || action.EventID != request.EventIDs[0] || len(action.SubscriptionIDs) == 0 {
		return false
	}
	targets := make(map[int64]ManualTarget, len(request.Targets))
	for _, target := range request.Targets {
		targets[target.SubscriptionID] = target
	}
	seen := make(map[int64]bool)
	for _, id := range action.SubscriptionIDs {
		target, found := targets[id]
		if !found || seen[id] || target.Mask != action.Mask {
			return false
		}
		seen[id] = true
	}
	return true
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		seen[value] = true
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func actionAllowedAt(cfg Config, state *State, action Action, now time.Time) bool {
	if !actionAllowed(cfg, state, action) {
		return false
	}
	if action.Mode == "manual" {
		index := manualRequestIndex(state, action.ManualRequestID)
		return index >= 0 && state.ManualRequests[index].ExpiresAt.After(now)
	}
	return true
}

func manualSubscriptionHints(api AdminAPI, request ManualRequest, ids []int64) map[string]SubscriptionRef {
	hints := make(map[string]SubscriptionRef)
	for _, target := range request.Targets {
		if containsID(ids, target.SubscriptionID) {
			hints[strconv.FormatInt(target.SubscriptionID, 10)] = target.Ref
		}
	}
	if primer, ok := api.(interface {
		PrimeSubscriptionHints(map[string]SubscriptionRef)
	}); ok {
		primer.PrimeSubscriptionHints(hints)
	}
	return hints
}

func updateManualRequestStatus(state *State, id string) {
	index := manualRequestIndex(state, id)
	if index < 0 {
		return
	}
	request := &state.ManualRequests[index]
	if request.Status != "processing" && request.Status != "succeeded" && request.Status != "partial" && request.Status != "failed" && request.Status != "unknown" && request.Status != "skipped" {
		return
	}
	latest := make(map[int64]ResetItem)
	unknown := false
	for _, action := range state.Actions {
		if action.ManualRequestID != id {
			continue
		}
		if action.Status == "pending" || action.Status == "running" {
			request.Status = "processing"
			return
		}
		if action.Status == "unknown" {
			unknown = true
		}
		for _, result := range action.Results {
			latest[result.SubscriptionID] = result
		}
	}
	if unknown {
		request.Status, request.LastError = "unknown", "存在结果未知的目标；不会自动重放，请人工核对"
		return
	}
	success, skipped := 0, 0
	for _, item := range latest {
		if item.Success {
			success++
		} else if strings.HasPrefix(item.Error, "skipped:") {
			skipped++
		}
	}
	request.Status, request.LastError = "succeeded", ""
	if success != len(request.Targets) {
		request.Status = "partial"
	}
	if success == 0 {
		request.Status = "failed"
		if skipped == len(request.Targets) {
			request.Status = "skipped"
		}
	}
}

// Process uses the same serialization as scheduled checks. A busy engine keeps
// the durably approved actions queued for the next CheckNow call.
func (e *Engine) ProcessManualReset(ctx context.Context, id string) error {
	if !e.busy.CompareAndSwap(false, true) {
		return errEngineBusy
	}
	defer e.busy.Store(false)
	if err := e.recoverInFlight(); err != nil {
		return err
	}
	state, err := e.store.Snapshot()
	if err != nil {
		return err
	}
	index := manualRequestIndex(&state, id)
	if index < 0 {
		return errors.New("手动重置请求不存在")
	}
	if state.ManualRequests[index].Status != "processing" {
		return nil
	}
	var executionErr error
	for _, action := range state.Actions {
		if action.ManualRequestID != id || action.Status != "pending" {
			continue
		}
		if ctx.Err() != nil {
			return errors.Join(executionErr, ctx.Err())
		}
		executionErr = errors.Join(executionErr, e.executeAction(ctx, action.ID))
	}
	if err := e.queueEventSummaries(); err != nil {
		return errors.Join(executionErr, err)
	}
	return errors.Join(executionErr, e.processDeliveries(ctx))
}

func manualRequestMessage(state *State, request ManualRequest) string {
	labels := make([]string, 0, min(20, len(request.Targets)))
	for _, target := range request.Targets[:min(20, len(request.Targets))] {
		windows := make([]string, 0, 3)
		if target.Mask.Daily {
			windows = append(windows, "日")
		}
		if target.Mask.Weekly {
			windows = append(windows, "周")
		}
		if target.Mask.Monthly {
			windows = append(windows, "月")
		}
		labels = append(labels, fmt.Sprintf("#%d（%s）", target.SubscriptionID, strings.Join(windows, "/")))
	}
	message := fmt.Sprintf("订阅手动重置待确认\n全局自动重置已关闭，尚未重置订阅。\n请求：%s\n选定订阅共 %d 个，关联归零事件 %d 个。\n选择“重置选定订阅”将只清各订阅已选的日/周/月周期用量，不延长有效期；选择“忽略”不执行重置。\n按钮有效至：%s（24 小时）\n选定订阅：%s", request.ID, len(request.Targets), len(request.EventIDs), request.ExpiresAt.Format(time.RFC3339), strings.Join(labels, "、"))
	if len(request.Targets) > len(labels) {
		message += fmt.Sprintf("\n其余 %d 条，完整清单与各订阅周期见工作台手动确认记录（请求 %s）。", len(request.Targets)-len(labels), request.ID)
	}
	shown := 0
	for _, event := range state.Events {
		if containsString(request.EventIDs, event.ID) && shown < 3 {
			details := []rune(eventMessage(event))
			if len(details) > 600 {
				message += "\n\n" + string(details[:600]) + "…\n完整事件详情见工作台。"
			} else {
				message += "\n\n" + string(details)
			}
			shown++
		}
	}
	if len(request.EventIDs) > shown {
		message += fmt.Sprintf("\n其余 %d 个归零事件详情见工作台。", len(request.EventIDs)-shown)
	}
	return message
}
