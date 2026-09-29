package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	verboseLogging atomic.Bool
	mu             sync.Mutex
	db             *sql.DB
	aead           cipher.AEAD
	lock           *os.File
}

func OpenStore(directory, encodedKey string) (_ *Store, err error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	lock, err := lockDataDirectory(directory)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = lock.Close()
		}
	}()
	key, err := loadMasterKey(directory, encodedKey)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(directory, "quota-watch.db")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = file.Chmod(0600); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err = file.Close(); err != nil {
		return nil, err
	}
	u := filepath.ToSlash(path)
	db, err := sql.Open("sqlite", u)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	defer func() {
		if err != nil {
			_ = db.Close()
		}
	}()
	if _, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS documents (name TEXT PRIMARY KEY, payload BLOB NOT NULL);`); err != nil {
		return nil, err
	}
	s := &Store{db: db, aead: aead, lock: lock}
	var cfg Config
	if cfg, err = s.Config(); err != nil {
		return nil, err
	}
	s.verboseLogging.Store(cfg.VerboseLogging)
	if _, err = s.Snapshot(); err != nil {
		return nil, err
	}
	return s, nil
}

func loadMasterKey(directory, encoded string) ([]byte, error) {
	if encoded = strings.TrimSpace(encoded); encoded != "" {
		key, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(key) != 32 {
			return nil, errors.New("QUOTA_WATCH_MASTER_KEY 必须是 32 字节密钥的 Base64 编码")
		}
		return key, nil
	}
	path := filepath.Join(directory, "master.key")
	key, err := os.ReadFile(path)
	if err == nil {
		if len(key) != 32 {
			return nil, errors.New("master.key 长度无效，请恢复原密钥")
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key = make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	if _, err := file.Write(key); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return nil, err
	}
	return key, file.Close()
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.db.Close()
	lockErr := s.lock.Close()
	return errors.Join(err, lockErr)
}

func (s *Store) Config() (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.configLocked()
}

func (s *Store) VerboseLoggingEnabled() bool { return s.verboseLogging.Load() }

func (s *Store) configLocked() (Config, error) {
	var payload []byte
	err := s.db.QueryRow("SELECT payload FROM documents WHERE name = ?", "config").Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultConfig(), nil
	}
	if err != nil {
		return Config{}, err
	}
	if len(payload) < s.aead.NonceSize() {
		return Config{}, errors.New("配置密文损坏")
	}
	plain, err := s.aead.Open(nil, payload[:s.aead.NonceSize()], payload[s.aead.NonceSize():], []byte("quota-watch/config/v1"))
	if err != nil {
		return Config{}, errors.New("无法解密配置，请使用原 master.key 或 QUOTA_WATCH_MASTER_KEY")
	}
	var cfg Config
	if err := json.Unmarshal(plain, &cfg); err != nil {
		return Config{}, fmt.Errorf("读取配置: %w", err)
	}
	cfg.Update = normalizeUpdateConfig(cfg.Update)
	return cfg, nil
}

func (s *Store) Snapshot() (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateLocked()
}

func (s *Store) stateLocked() (State, error) {
	state := State{Rules: []Rule{}, Observations: map[string]Observation{}, Events: []Event{}, Actions: []Action{}, Deliveries: []Delivery{}}
	var payload []byte
	err := s.db.QueryRow("SELECT payload FROM documents WHERE name = ?", "state").Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return State{}, err
	}
	if err := json.Unmarshal(payload, &state); err != nil {
		return State{}, fmt.Errorf("读取状态: %w", err)
	}
	normalizeState(&state)
	return state, nil
}

func normalizeState(state *State) {
	if state.ManualRequests == nil {
		state.ManualRequests = []ManualRequest{}
	}
	if state.Rules == nil {
		state.Rules = []Rule{}
	}
	if state.Observations == nil {
		state.Observations = map[string]Observation{}
	}
	if state.Events == nil {
		state.Events = []Event{}
	}
	if state.Actions == nil {
		state.Actions = []Action{}
	}
	if state.Deliveries == nil {
		state.Deliveries = []Delivery{}
	}
}

func (s *Store) UpdateForManualConfig(expected Config, update func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.configLocked()
	if err != nil {
		return err
	}
	if current.BaseURL != expected.BaseURL || current.AdminAPIKey != expected.AdminAPIKey ||
		current.AutoResetEnabled != expected.AutoResetEnabled || current.Telegram.Enabled != expected.Telegram.Enabled ||
		current.Telegram.BotToken != expected.Telegram.BotToken || current.Telegram.ChatID != expected.Telegram.ChatID ||
		!reflect.DeepEqual(current.Telegram.AllowedUserIDs, expected.Telegram.AllowedUserIDs) {
		return errors.New("手动重置的连接、开关或 Telegram 权限已改变，请重新核对")
	}
	return s.updateLocked(update)
}

func (s *Store) Update(update func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateLocked(update)
}

func (s *Store) UpdateForConfig(expected Config, update func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, err := s.configLocked()
	if err != nil {
		return err
	}
	if current.BaseURL != expected.BaseURL || current.AdminAPIKey != expected.AdminAPIKey {
		return errors.New("连接配置已改变，请刷新后重新操作")
	}
	return s.updateLocked(update)
}

func (s *Store) SaveRulesForConfig(expected Config, rules []Rule) error {
	return s.UpdateForConfig(expected, func(state *State) error {
		previous := map[int64]bool{}
		for _, id := range monitoredAccounts(state.Rules) {
			previous[id] = true
		}
		for _, id := range monitoredAccounts(rules) {
			if !previous[id] {
				delete(state.Observations, fmt.Sprint(id))
			}
		}
		state.Rules = rules
		return nil
	})
}

func (s *Store) updateLocked(update func(*State) error) error {
	state, err := s.stateLocked()
	if err != nil {
		return err
	}
	if err := update(&state); err != nil {
		return err
	}
	payload, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = s.db.Exec("INSERT INTO documents(name,payload) VALUES(?,?) ON CONFLICT(name) DO UPDATE SET payload=excluded.payload", "state", payload)
	return err
}

func (s *Store) SaveConfig(cfg Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, err := s.configLocked()
	if err != nil {
		return err
	}
	plain, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	payload := s.aead.Seal(nonce, nonce, plain, []byte("quota-watch/config/v1"))
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("INSERT INTO documents(name,payload) VALUES(?,?) ON CONFLICT(name) DO UPDATE SET payload=excluded.payload", "config", payload); err != nil {
		return err
	}
	if previous.BaseURL != cfg.BaseURL {
		// IDs belong to one installation. A new URL requires fresh selection.
		var state State
		var raw []byte
		err := tx.QueryRow("SELECT payload FROM documents WHERE name = ?", "state").Scan(&raw)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &state); err != nil {
				return err
			}
		}
		state.Observations = map[string]Observation{}
		state.Health = Health{}
		state.Update = UpdateState{}
		normalizeState(&state)
		for i := range state.Rules {
			state.Rules[i].Enabled = false
			state.Rules[i].SubscriptionRefs = nil
		}
		for i := range state.Actions {
			if state.Actions[i].Status == "pending" {
				state.Actions[i].Status = "skipped"
				state.Actions[i].LastError = "主站地址已变更，请重新核对规则"
				state.Actions[i].UpdatedAt = time.Now().UTC()
			}
		}
		for i := range state.ManualRequests {
			if state.ManualRequests[i].Status == "pending" || state.ManualRequests[i].Status == "processing" {
				state.ManualRequests[i].Status = "invalidated"
				state.ManualRequests[i].LastError = "主站地址已变更，请重新核对规则"
			}
		}
		for i := range state.Deliveries {
			if state.Deliveries[i].Status == "pending" || state.Deliveries[i].Status == "failed" {
				state.Deliveries[i].Status = "cancelled"
			}
		}
		raw, err = json.Marshal(state)
		if err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO documents(name,payload) VALUES(?,?) ON CONFLICT(name) DO UPDATE SET payload=excluded.payload", "state", raw); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.verboseLogging.Store(cfg.VerboseLogging)
	return nil
}
