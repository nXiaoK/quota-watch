package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var adminRequestSequence atomic.Uint64

type adminRequestLog struct {
	client     *AdminClient
	logger     *slog.Logger
	ctx        context.Context
	started    time.Time
	attrs      []slog.Attr
	httpStatus int
	errorClass string
}

func (c *AdminClient) startRequestLog(ctx context.Context, method, route string, query url.Values, allowed bool) *adminRequestLog {
	if c.logEnabled == nil || !c.logEnabled() {
		return nil
	}
	logger := c.logger
	if logger == nil {
		logger = slog.Default()
	}
	if method != http.MethodGet && method != http.MethodPost {
		method = "[blocked]"
	}
	path := "[blocked]"
	if allowed {
		u := *c.baseURL
		u.Path += route
		path = u.EscapedPath()
	}
	attrs := []slog.Attr{
		slog.String("request_id", strconv.FormatUint(adminRequestSequence.Add(1), 10)),
		slog.String("method", method),
		slog.String("host", c.baseURL.Host),
		slog.String("path", path),
	}
	if c.logSource != "" {
		attrs = append(attrs, slog.String("source", c.logSource))
	}
	if allowed {
		if safeQuery := safeRequestLogQuery(query); safeQuery != "" {
			attrs = append(attrs, slog.String("query", safeQuery))
		}
	}
	entry := &adminRequestLog{client: c, logger: logger, ctx: ctx, started: time.Now(), attrs: attrs}
	logger.LogAttrs(ctx, slog.LevelInfo, "sub2api 请求", append(attrs, slog.String("phase", "start"))...)
	return entry
}

func (entry *adminRequestLog) failure(class string) {
	if entry != nil {
		entry.errorClass = class
	}
}

func (entry *adminRequestLog) response(status int) {
	if entry != nil {
		entry.httpStatus = status
	}
}

func (entry *adminRequestLog) end() {
	if entry == nil || !entry.client.logEnabled() {
		return
	}
	outcome, message := "success", ""
	switch entry.errorClass {
	case "route_not_permitted":
		outcome, message = "rejected", "administrator API route or action is not permitted by stored-data monitoring"
	case "request_encode":
		outcome, message = "protocol", "failed to encode administrator API request"
	case "request_build":
		outcome, message = "protocol", "failed to create administrator API request"
	case "connection":
		outcome, message = "transport", "administrator API connection failed"
	case "context_canceled", "deadline_exceeded":
		outcome, message = "canceled", "administrator API request was canceled or timed out"
	case "http_status":
		outcome, message = "http", fmt.Sprintf("administrator API rejected request (HTTP %d)", entry.httpStatus)
	case "response_read":
		outcome, message = "protocol", "administrator API response could not be read"
	case "response_envelope":
		outcome, message = "protocol", "administrator API returned an invalid or unsuccessful response"
	case "response_payload":
		outcome, message = "protocol", "administrator API returned an invalid response payload"
	}
	attrs := append(entry.attrs, slog.String("phase", "end"), slog.Int("http_status", entry.httpStatus),
		slog.Int64("duration_ms", time.Since(entry.started).Milliseconds()), slog.String("outcome", outcome))
	if entry.errorClass != "" {
		attrs = append(attrs, slog.String("error_class", entry.errorClass), slog.String("error", message))
	}
	entry.logger.LogAttrs(entry.ctx, slog.LevelInfo, "sub2api 请求", attrs...)
}

func safeRequestLogQuery(query url.Values) string {
	safe := make(url.Values)
	for _, name := range []string{"page", "page_size", "user_id", "group_id", "group", "platform", "type", "status", "sort_by", "sort_order", "lite", "include_scheduler_score", "force", "search"} {
		values, exists := query[name]
		if !exists {
			continue
		}
		value := "[redacted]"
		if len(values) == 1 {
			raw := values[0]
			switch name {
			case "page", "page_size", "user_id", "group_id", "group":
				if id, err := strconv.ParseInt(raw, 10, 64); err == nil && id > 0 && raw == strconv.FormatInt(id, 10) {
					value = raw
				} else if name == "group" && raw == "ungrouped" {
					value = raw
				}
			case "platform":
				if raw == "openai" || raw == "anthropic" || raw == "gemini" || raw == "antigravity" {
					value = raw
				}
			case "type":
				if raw == "oauth" || raw == "apikey" || raw == "upstream" {
					value = raw
				}
			case "status":
				if raw == "active" || raw == "inactive" || raw == "error" || raw == "expired" || raw == "revoked" {
					value = raw
				}
			case "sort_by":
				if raw == "id" || raw == "name" || raw == "status" || raw == "schedulable" || raw == "priority" || raw == "rate_multiplier" || raw == "last_used_at" || raw == "expires_at" || raw == "created_at" || raw == "starts_at" || raw == "upstream_billing_rate" {
					value = raw
				}
			case "sort_order":
				if raw == "asc" || raw == "desc" {
					value = raw
				}
			case "force":
				if raw == "true" {
					value = raw
				}
			case "lite", "include_scheduler_score":
				if raw == "true" || raw == "false" || raw == "1" || raw == "0" {
					value = raw
				}
			}
		}
		safe.Set(name, value)
	}
	return strings.ReplaceAll(safe.Encode(), "%5Bredacted%5D", "[redacted]")
}
