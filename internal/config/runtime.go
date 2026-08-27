package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/cloudomni/omnigate/internal/store"
)

// Runtime 是运行层配置快照。构建后只读，读者不得修改。
type Runtime struct {
	BreakerCooldownLadder   []time.Duration
	BreakerDisableThreshold int
	BreakerMaxHops          int
	KeyCooldownS            int
	StreamIdleTimeoutS      int
	StreamInjectUsage       bool
	CaptureEnabled          bool
	CaptureRoutes           []string
	CaptureRetentionDays    int
	LogRetentionDays        int
}

type settingSpec struct {
	key      string
	def      string // JSON 编码的默认值
	validate func(v any) error
}

func durArr(v any) error {
	arr, ok := v.([]any)
	if !ok || len(arr) == 0 {
		return errors.New(`must be a non-empty array like ["30s","1m"]`)
	}
	for _, e := range arr {
		s, ok := e.(string)
		if !ok {
			return errors.New(`elements must be duration strings like "30s"`)
		}
		if d, err := time.ParseDuration(s); err != nil || d <= 0 {
			return fmt.Errorf("invalid duration %q", s)
		}
	}
	return nil
}

func intRange(lo, hi int) func(any) error {
	return func(v any) error {
		n, ok := v.(float64)
		if !ok || n != float64(int(n)) || n < float64(lo) || n > float64(hi) {
			return fmt.Errorf("must be an integer in [%d, %d]", lo, hi)
		}
		return nil
	}
}

func boolVal(v any) error {
	if _, ok := v.(bool); !ok {
		return errors.New("must be true or false")
	}
	return nil
}

func strArr(v any) error {
	arr, ok := v.([]any)
	if !ok {
		return errors.New("must be an array of route names")
	}
	for _, e := range arr {
		s, ok := e.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return errors.New("elements must be non-empty route names")
		}
	}
	return nil
}

var settingSpecs = []settingSpec{
	{key: "breaker.cooldown_ladder", def: `["30s","1m","3m"]`, validate: durArr},
	{key: "breaker.disable_threshold", def: `3`, validate: intRange(1, 100)},
	{key: "breaker.max_hops", def: `3`, validate: intRange(1, 10)},
	{key: "ratelimit.key_cooldown_s", def: `60`, validate: intRange(1, 86400)},
	{key: "stream.idle_timeout_s", def: `300`, validate: intRange(1, 86400)},
	{key: "stream.inject_usage", def: `true`, validate: boolVal},
	{key: "capture.enabled", def: `false`, validate: boolVal},
	{key: "capture.routes", def: `[]`, validate: strArr},
	{key: "capture.retention_days", def: `3`, validate: intRange(1, 365)},
	{key: "log.retention_days", def: `0`, validate: intRange(0, 3650)},
}

// RuntimeManager 管理运行层配置：DB 为事实来源，内存快照 atomic 替换（保存即热生效）。
type RuntimeManager struct {
	db   *store.Store
	snap atomic.Pointer[Runtime]
}

// NewRuntimeManager 打开（并按需播种默认值）运行层配置。
func NewRuntimeManager(db *store.Store) (*RuntimeManager, error) {
	m := &RuntimeManager{db: db}
	if err := m.seedDefaults(); err != nil {
		return nil, err
	}
	if err := m.rebuild(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *RuntimeManager) seedDefaults() error {
	var existing []store.AppConfig
	if err := m.db.DB.Find(&existing).Error; err != nil {
		return fmt.Errorf("load app_config: %w", err)
	}
	have := map[string]bool{}
	for _, r := range existing {
		have[r.Key] = true
	}
	for _, sp := range settingSpecs {
		if have[sp.key] {
			continue
		}
		if err := m.db.DB.Create(&store.AppConfig{Key: sp.key, Value: sp.def}).Error; err != nil {
			return fmt.Errorf("seed %s: %w", sp.key, err)
		}
	}
	return nil
}

// rawMap 返回「默认值 + DB 行覆盖」合并后的原始 JSON 串。
func (m *RuntimeManager) rawMap() (map[string]string, error) {
	raw := map[string]string{}
	for _, sp := range settingSpecs {
		raw[sp.key] = sp.def
	}
	var rows []store.AppConfig
	if err := m.db.DB.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("load app_config: %w", err)
	}
	for _, r := range rows {
		if _, isKnown := raw[r.Key]; isKnown {
			raw[r.Key] = r.Value
		}
	}
	return raw, nil
}

func (m *RuntimeManager) rebuild() error {
	raw, err := m.rawMap()
	if err != nil {
		return err
	}
	rt := &Runtime{}

	var ladder []string
	if err := json.Unmarshal([]byte(raw["breaker.cooldown_ladder"]), &ladder); err != nil {
		return fmt.Errorf("parse breaker.cooldown_ladder: %w", err)
	}
	for _, s := range ladder {
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("parse breaker.cooldown_ladder: %w", err)
		}
		rt.BreakerCooldownLadder = append(rt.BreakerCooldownLadder, d)
	}

	getInt := func(key string) int {
		var n int
		if err := json.Unmarshal([]byte(raw[key]), &n); err != nil {
			return 0 // seed + validate 已保证不会发生
		}
		return n
	}
	getBool := func(key string) bool {
		var b bool
		_ = json.Unmarshal([]byte(raw[key]), &b)
		return b
	}

	rt.BreakerDisableThreshold = getInt("breaker.disable_threshold")
	rt.BreakerMaxHops = getInt("breaker.max_hops")
	rt.KeyCooldownS = getInt("ratelimit.key_cooldown_s")
	rt.StreamIdleTimeoutS = getInt("stream.idle_timeout_s")
	rt.StreamInjectUsage = getBool("stream.inject_usage")
	rt.CaptureEnabled = getBool("capture.enabled")
	_ = json.Unmarshal([]byte(raw["capture.routes"]), &rt.CaptureRoutes)
	rt.CaptureRetentionDays = getInt("capture.retention_days")
	rt.LogRetentionDays = getInt("log.retention_days")

	m.snap.Store(rt)
	return nil
}

// Snapshot 返回当前运行层配置（只读，调用方不得修改）。
func (m *RuntimeManager) Snapshot() *Runtime { return m.snap.Load() }

// All 返回合并默认值后的已解码配置 map，用于 GET /api/settings。
func (m *RuntimeManager) All() (map[string]any, error) {
	raw, err := m.rawMap()
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for k, v := range raw {
		var dec any
		if err := json.Unmarshal([]byte(v), &dec); err != nil {
			out[k] = v
			continue
		}
		out[k] = dec
	}
	return out, nil
}

// Update 校验并写入部分配置，然后原子重建快照（热生效）。
func (m *RuntimeManager) Update(payload map[string]json.RawMessage) error {
	if len(payload) == 0 {
		return errors.New("empty settings payload")
	}
	specByKey := map[string]settingSpec{}
	for _, sp := range settingSpecs {
		specByKey[sp.key] = sp
	}
	type kv struct{ key, value string }
	updates := make([]kv, 0, len(payload))
	for k, rawMsg := range payload {
		sp, ok := specByKey[k]
		if !ok {
			return fmt.Errorf("unknown setting %q", k)
		}
		var v any
		if err := json.Unmarshal(rawMsg, &v); err != nil {
			return fmt.Errorf("setting %q: invalid JSON value", k)
		}
		if sp.validate != nil {
			if err := sp.validate(v); err != nil {
				return fmt.Errorf("setting %q: %w", k, err)
			}
		}
		updates = append(updates, kv{k, string(rawMsg)})
	}
	err := m.db.DB.Transaction(func(tx *gorm.DB) error {
		for _, u := range updates {
			row := store.AppConfig{Key: u.key, Value: u.value}
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "key"}},
				DoUpdates: clause.AssignmentColumns([]string{"value"}),
			}).Create(&row).Error; err != nil {
				return fmt.Errorf("save %s: %w", u.key, err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return m.rebuild()
}
