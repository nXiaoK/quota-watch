package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed all:web/dist
var webFiles embed.FS

type Server struct {
	store  *Store
	engine *Engine
	logger *slog.Logger
	ctx    context.Context
	auth   *panelAuth
	jobs   sync.WaitGroup
}

func NewServer(ctx context.Context, store *Store, engine *Engine, logger *slog.Logger, username, password string) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{store: store, engine: engine, logger: logger, ctx: ctx, auth: newPanelAuth(username, password)}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.Handle("/", s.guard(http.HandlerFunc(s.route)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api" && !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get("X-Quota-Watch") != "1" {
				writeError(w, 403, "缺少请求校验头")
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" {
				if _, ok := sameHostOrigin(r); !ok {
					writeError(w, 403, "不接受跨站修改请求")
					return
				}
			}
		}
		if r.URL.Path != "/api/auth/login" || r.Method != http.MethodPost {
			if _, ok := s.auth.session(r); !ok {
				writeError(w, 401, "请先登录监控服务")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		s.serveWeb(w, r)
		return
	}
	switch {
	case r.URL.Path == "/api/auth/login" && r.Method == http.MethodPost:
		s.auth.login(w, r)
	case r.URL.Path == "/api/auth/session" && r.Method == http.MethodGet:
		info, ok := s.auth.session(r)
		if !ok {
			writeError(w, 401, "请先登录监控服务")
			return
		}
		writeJSON(w, 200, info)
	case r.URL.Path == "/api/auth/logout" && r.Method == http.MethodPost:
		s.auth.invalidate(r)
		clearSessionCookie(w, r)
		writeJSON(w, 200, map[string]bool{"authenticated": false})
	case r.URL.Path == "/api/config" && r.Method == "GET":
		cfg, err := s.store.Config()
		if err != nil {
			writeError(w, 500, "读取配置失败")
			return
		}
		writeJSON(w, 200, publicConfig(cfg))
	case r.URL.Path == "/api/config" && r.Method == "PUT":
		previous, err := s.store.Config()
		if err != nil {
			writeError(w, 500, "读取配置失败")
			return
		}
		var input ConfigInput
		if err := readJSON(w, r, &input); err != nil {
			writeError(w, 400, "配置格式无效")
			return
		}
		cfg, err := mergeConfig(input, previous)
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if err := s.store.SaveConfig(cfg); err != nil {
			writeError(w, 500, "保存配置失败")
			return
		}
		if previous.VerboseLogging != cfg.VerboseLogging {
			s.logger.Info("运行日志设置已更新", "verbose_logging", cfg.VerboseLogging)
		}
		writeJSON(w, 200, publicConfig(cfg))
	case r.URL.Path == "/api/state" && r.Method == "GET":
		state, err := s.store.Snapshot()
		if err != nil {
			writeError(w, 500, "读取状态失败")
			return
		}
		writeJSON(w, 200, struct {
			State
			Busy bool `json:"busy"`
		}{state, s.engine.IsBusy()})
	case r.URL.Path == "/api/update/acknowledge" && r.Method == http.MethodPost:
		cfg, err := s.store.Config()
		if err != nil {
			writeError(w, 500, "读取配置失败")
			return
		}
		if err := s.store.UpdateForConfig(cfg, func(state *State) error {
			if state.Update.Status != "unknown" {
				return errors.New("当前没有需要人工核对的更新任务")
			}
			state.Update.Status = "available"
			state.Update.LastError = ""
			state.Update.LastCheckAt = time.Time{}
			state.Update.HasUpdate = false
			state.Update.LastAttemptDate = ""
			state.Update.LastAttemptVersion = ""
			state.Update.OperationID = ""
			return nil
		}); err != nil {
			writeError(w, 409, err.Error())
			return
		}
		s.logger.Info("管理员已核对并解除自动更新阻塞")
		writeJSON(w, 200, map[string]string{"message": "已解除阻塞，下次检查将重新确认版本"})
	case r.URL.Path == "/api/accounts" && r.Method == "GET":
		client, err := s.client()
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		query := allowedQuery(r, "page", "page_size", "search", "group")
		query.Set("platform", "openai")
		query.Set("type", "oauth")
		query.Set("lite", "true")
		page, err := client.ListAccounts(r.Context(), query)
		if err != nil {
			writeError(w, 502, err.Error())
			return
		}
		writeJSON(w, 200, page)
	case r.URL.Path == "/api/subscriptions" && r.Method == "GET":
		client, err := s.client()
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		query := allowedQuery(r, "page", "page_size", "user_id", "group_id")
		query.Set("status", "active")
		page, err := client.ListSubscriptions(r.Context(), query)
		if err != nil {
			writeError(w, 502, err.Error())
			return
		}
		writeJSON(w, 200, page)
	case r.URL.Path == "/api/connection/test" && r.Method == "POST":
		cfg, err := s.requestConfig(w, r)
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		client, err := s.newAdminClient(cfg)
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		result, err := client.Connection(r.Context())
		if err != nil {
			writeError(w, 502, err.Error())
			return
		}
		stored, storedErr := s.store.Config()
		if storedErr == nil && stored.BaseURL == cfg.BaseURL && stored.AdminAPIKey == cfg.AdminAPIKey {
			_ = s.store.Update(func(state *State) error { state.Health.Version = result.Version; return nil })
		}
		writeJSON(w, 200, result)
	case r.URL.Path == "/api/notifications/test" && r.Method == "POST":
		var input struct {
			Channel string       `json:"channel"`
			Config  *ConfigInput `json:"config"`
		}
		if err := readJSON(w, r, &input); err != nil {
			writeError(w, 400, "请求格式无效")
			return
		}
		cfg, err := s.store.Config()
		if err == nil && input.Config != nil {
			cfg, err = mergeConfig(*input.Config, cfg)
		}
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if input.Channel != "telegram" && input.Channel != "email" {
			writeError(w, 400, "请选择 Telegram 或邮件渠道")
			return
		}
		if err := SendNotification(r.Context(), cfg, input.Channel, "Quota Watch 测试通知：通知渠道连接成功。"); err != nil {
			writeError(w, 502, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"message": "测试通知已发送"})
	case r.URL.Path == "/api/rules" && r.Method == "PUT":
		var input struct {
			Rules []Rule `json:"rules"`
		}
		if err := readJSON(w, r, &input); err != nil {
			writeError(w, 400, "规则格式无效")
			return
		}
		if err := validateRules(input.Rules); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		if input.Rules == nil {
			input.Rules = []Rule{}
		}
		expectedConfig, err := s.store.Config()
		if err != nil {
			writeError(w, 500, "读取配置失败")
			return
		}
		ids := []int64{}
		seen := map[int64]bool{}
		for i := range input.Rules {
			input.Rules[i].SubscriptionRefs = nil
			for _, id := range input.Rules[i].SubscriptionIDs {
				if !seen[id] {
					ids = append(ids, id)
					seen[id] = true
				}
			}
		}
		if len(ids) > 0 {
			client, err := s.newAdminClient(expectedConfig)
			if err != nil {
				writeError(w, 400, err.Error())
				return
			}
			previousState, stateErr := s.store.Snapshot()
			if stateErr != nil {
				writeError(w, 500, "读取规则失败")
				return
			}
			for _, rule := range previousState.Rules {
				client.PrimeSubscriptionHints(rule.SubscriptionRefs)
			}
			subscriptions, err := client.GetSubscriptions(r.Context(), ids)
			if err != nil {
				writeError(w, 502, err.Error())
				return
			}
			for i := range input.Rules {
				input.Rules[i].SubscriptionRefs = map[string]SubscriptionRef{}
				for _, id := range input.Rules[i].SubscriptionIDs {
					if sub, ok := subscriptions[id]; ok {
						input.Rules[i].SubscriptionRefs[strconv.FormatInt(id, 10)] = SubscriptionRef{UserID: sub.UserID, GroupID: sub.GroupID}
					}
				}
			}
		}
		if err := s.store.SaveRulesForConfig(expectedConfig, input.Rules); err != nil {
			writeError(w, 409, "保存规则失败，连接配置可能已改变，请刷新后重试")
			return
		}
		writeJSON(w, 200, input)
	case r.URL.Path == "/api/rules/simulate" && r.Method == "POST":
		var input struct {
			Rule Rule `json:"rule"`
		}
		if err := readJSON(w, r, &input); err != nil {
			writeError(w, 400, "规则格式无效")
			return
		}
		if err := validateRules([]Rule{input.Rule}); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		result, err := s.engine.Simulate(r.Context(), input.Rule)
		if err != nil {
			writeError(w, 502, err.Error())
			return
		}
		writeJSON(w, 200, result)
	case r.URL.Path == "/api/check" && r.Method == "POST":
		if s.engine.IsBusy() {
			writeError(w, 409, "上一轮检测仍在执行")
			return
		}
		s.jobs.Add(1)
		go func() {
			defer s.jobs.Done()
			if err := s.engine.CheckNow(s.ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.logger.Warn("手动检测未完成", "error", err)
			}
		}()
		writeJSON(w, 202, map[string]string{"message": "已安排检测，请查看概览中的最新状态"})
	case strings.HasPrefix(r.URL.Path, "/api/actions/") && strings.HasSuffix(r.URL.Path, "/retry") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/actions/"), "/retry")
		if id == "" || strings.Contains(id, "/") {
			writeError(w, 400, "动作 ID 无效")
			return
		}
		if err := s.engine.RetryAction(r.Context(), id); err != nil {
			writeError(w, 409, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"message": "已处理失败项重试"})
	default:
		writeError(w, 404, "接口不存在")
	}
}

func (s *Server) client() (*AdminClient, error) {
	cfg, err := s.store.Config()
	if err != nil {
		return nil, errors.New("读取配置失败")
	}
	return s.newAdminClient(cfg)
}

func (s *Server) newAdminClient(cfg Config) (*AdminClient, error) {
	client, err := NewAdminClient(cfg)
	if err == nil {
		client.logger = s.logger
		client.logEnabled = s.store.VerboseLoggingEnabled
	}
	return client, err
}

func (s *Server) requestConfig(w http.ResponseWriter, r *http.Request) (Config, error) {
	previous, err := s.store.Config()
	if err != nil {
		return Config{}, errors.New("读取配置失败")
	}
	if r.ContentLength == 0 {
		return previous, nil
	}
	var raw json.RawMessage
	if err := readJSON(w, r, &raw); err != nil {
		return Config{}, errors.New("配置格式无效")
	}
	var wrapped struct {
		Config *ConfigInput `json:"config"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return Config{}, errors.New("配置格式无效")
	}
	if wrapped.Config != nil {
		return mergeConfig(*wrapped.Config, previous)
	}
	var input ConfigInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return Config{}, errors.New("配置格式无效")
	}
	if input.PollIntervalSeconds == 0 && input.BaseURL == "" {
		return previous, nil
	}
	return mergeConfig(input, previous)
}

func (s *Server) serveWeb(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, 405, "不支持此请求方法")
		return
	}
	assets, err := fs.Sub(webFiles, "web/dist")
	if err != nil {
		writeError(w, 500, "管理页不可用")
		return
	}
	if _, err := fs.Stat(assets, "index.html"); err != nil {
		writeError(w, 503, "管理页尚未构建，请先在 web 目录运行 pnpm install && pnpm build，再编译服务")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "index.html"
	}
	if _, err := fs.Stat(assets, path); err != nil {
		if strings.HasPrefix(path, "assets/") || strings.Contains(path, ".") {
			http.NotFound(w, r)
			return
		}
		copy := r.Clone(r.Context())
		copy.URL.Path = "/"
		r = copy
	}
	http.FileServer(http.FS(assets)).ServeHTTP(w, r)
}

func allowedQuery(r *http.Request, keys ...string) url.Values {
	query := url.Values{}
	for _, key := range keys {
		if value := r.URL.Query().Get(key); value != "" {
			query.Set(key, value)
		}
	}
	return query
}

func readJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1024*1024)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("请求只能包含一个 JSON 值")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
