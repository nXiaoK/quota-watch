package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"
)

// SystemUpdateInfo describes the latest release reported by Sub2API.
type SystemUpdateInfo struct {
	CurrentVersion string `json:"current_version"`
	LatestVersion  string `json:"latest_version"`
	HasUpdate      bool   `json:"has_update"`
	Warning        string `json:"warning"`
	BuildType      string `json:"build_type"`
	Cached         bool   `json:"cached"`
}

// SystemUpdateResult describes an in-place update, which requires a separate restart.
type SystemUpdateResult struct {
	Message         string `json:"message"`
	NeedRestart     bool   `json:"need_restart"`
	AlreadyUpToDate bool   `json:"already_up_to_date"`
	CurrentVersion  string `json:"current_version"`
	LatestVersion   string `json:"latest_version"`
	OperationID     string `json:"operation_id"`
}

func validSystemOperationKey(key string) bool {
	if key == "" || len(key) > 128 {
		return false
	}
	for _, ch := range key {
		if ch < 33 || ch > 126 {
			return false
		}
	}
	return true
}

func invalidSystemResponse(mutation bool) error {
	return &APIError{
		Status:  http.StatusOK,
		Message: "administrator API returned an invalid system response",
		Unknown: mutation,
	}
}

// LatestUsage returns the timestamp of the newest stored usage log, if any.
func (c *AdminClient) LatestUsage(ctx context.Context) (time.Time, bool, error) {
	query := url.Values{
		"page":       {"1"},
		"page_size":  {"1"},
		"sort_by":    {"created_at"},
		"sort_order": {"desc"},
	}
	var page Page[struct {
		CreatedAt time.Time `json:"created_at"`
	}]
	if err := c.request(ctx, http.MethodGet, "/admin/usage", query, nil, "", &page, false); err != nil {
		return time.Time{}, false, err
	}
	if page.Page != 1 || page.PageSize != 1 || len(page.Items) > 1 || page.Total < 0 || (len(page.Items) == 0 && page.Total != 0) {
		return time.Time{}, false, invalidSystemResponse(false)
	}
	if len(page.Items) == 0 {
		return time.Time{}, false, nil
	}
	if page.Items[0].CreatedAt.IsZero() {
		return time.Time{}, false, invalidSystemResponse(false)
	}
	return page.Items[0].CreatedAt, true, nil
}

// CheckUpdates bypasses Sub2API's update cache and asks for the latest release.
func (c *AdminClient) CheckUpdates(ctx context.Context) (SystemUpdateInfo, error) {
	var wire struct {
		SystemUpdateInfo
		HasUpdate *bool `json:"has_update"`
	}
	if err := c.request(ctx, http.MethodGet, "/admin/system/check-updates", url.Values{"force": {"true"}}, nil, "", &wire, false); err != nil {
		return SystemUpdateInfo{}, err
	}
	if wire.CurrentVersion == "" || wire.LatestVersion == "" || wire.HasUpdate == nil {
		return SystemUpdateInfo{}, invalidSystemResponse(false)
	}
	info := wire.SystemUpdateInfo
	info.HasUpdate = *wire.HasUpdate
	return info, nil
}

// PerformSystemUpdate replaces Sub2API's binary and reports whether it needs a restart.
func (c *AdminClient) PerformSystemUpdate(ctx context.Context, key string) (SystemUpdateResult, error) {
	if !validSystemOperationKey(key) {
		return SystemUpdateResult{}, errors.New("a valid system update idempotency key is required")
	}
	var wire struct {
		SystemUpdateResult
		NeedRestart     *bool `json:"need_restart"`
		AlreadyUpToDate *bool `json:"already_up_to_date"`
	}
	if err := c.request(ctx, http.MethodPost, "/admin/system/update", nil, nil, key, &wire, true); err != nil {
		return SystemUpdateResult{}, err
	}
	result := wire.SystemUpdateResult
	if wire.NeedRestart != nil {
		result.NeedRestart = *wire.NeedRestart
	}
	if wire.AlreadyUpToDate != nil {
		result.AlreadyUpToDate = *wire.AlreadyUpToDate
	}
	if result.Message == "" || result.NeedRestart == result.AlreadyUpToDate {
		return SystemUpdateResult{}, invalidSystemResponse(true)
	}
	return result, nil
}

// RestartSystem asks Sub2API to exit; its supervisor must start the new process.
func (c *AdminClient) RestartSystem(ctx context.Context, key string) error {
	if !validSystemOperationKey(key) {
		return errors.New("a valid system restart idempotency key is required")
	}
	var result struct {
		Message string `json:"message"`
	}
	if err := c.request(ctx, http.MethodPost, "/admin/system/restart", nil, nil, key, &result, true); err != nil {
		return err
	}
	if result.Message == "" {
		return invalidSystemResponse(true)
	}
	return nil
}

// RunningVersion returns the version compiled into the currently running process.
func (c *AdminClient) RunningVersion(ctx context.Context) (string, error) {
	var result struct {
		Version string `json:"version"`
	}
	if err := c.request(ctx, http.MethodGet, "/admin/system/version", nil, nil, "", &result, false); err != nil {
		return "", err
	}
	if result.Version == "" {
		return "", invalidSystemResponse(false)
	}
	return result.Version, nil
}
