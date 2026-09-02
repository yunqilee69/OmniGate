package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	ErrVKNotFound      = errors.New("virtual key not found")
	ErrVKDisabled      = errors.New("virtual key disabled")
	ErrVKRateLimited   = errors.New("rate limit exceeded")
	ErrVKBudgetExceeded = errors.New("budget exceeded")
	ErrVKModelDenied   = errors.New("model not allowed")
)

// GenerateVKToken 生成 vk- 前缀的随机 token。
func GenerateVKToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return "vk-" + hex.EncodeToString(b)
}

// CreateVirtualKey 创建虚拟 key。
func (s *Store) CreateVirtualKey(vk *VirtualKey) error {
	if vk.KeyValue == "" {
		vk.KeyValue = GenerateVKToken()
	}
	return s.DB.Create(vk).Error
}

// GetVirtualKeyByValue 根据 key_value 查询虚拟 key。
func (s *Store) GetVirtualKeyByValue(keyValue string) (*VirtualKey, error) {
	var vk VirtualKey
	if err := s.DB.Where("key_value = ?", keyValue).First(&vk).Error; err != nil {
		return nil, err
	}
	return &vk, nil
}

// GetVirtualKey 根据 ID 查询虚拟 key。
func (s *Store) GetVirtualKey(id int64) (*VirtualKey, error) {
	var vk VirtualKey
	if err := s.DB.First(&vk, id).Error; err != nil {
		return nil, err
	}
	return &vk, nil
}

// ListVirtualKeys 列出所有虚拟 key。
func (s *Store) ListVirtualKeys() ([]VirtualKey, error) {
	var vks []VirtualKey
	if err := s.DB.Order("created_at DESC").Find(&vks).Error; err != nil {
		return nil, err
	}
	return vks, nil
}

// UpdateVirtualKey 更新虚拟 key。
func (s *Store) UpdateVirtualKey(vk *VirtualKey) error {
	return s.DB.Save(vk).Error
}

// DeleteVirtualKey 删除虚拟 key。
func (s *Store) DeleteVirtualKey(id int64) error {
	return s.DB.Delete(&VirtualKey{}, id).Error
}

// CheckVKAuth 检查虚拟 key 是否有效、是否禁用。
func (s *Store) CheckVKAuth(keyValue string) (*VirtualKey, error) {
	vk, err := s.GetVirtualKeyByValue(keyValue)
	if err != nil {
		return nil, ErrVKNotFound
	}
	if vk.Status != "active" {
		return nil, ErrVKDisabled
	}
	return vk, nil
}

// CheckVKModelAccess 检查虚拟 key 是否允许访问指定模型。
func (s *Store) CheckVKModelAccess(vk *VirtualKey, model string) error {
	if vk.AllowedModels == "" || vk.AllowedModels == "[]" {
		return nil // 空=全部允许
	}
	var allowed []string
	if err := json.Unmarshal([]byte(vk.AllowedModels), &allowed); err != nil {
		return fmt.Errorf("parse allowed_models: %w", err)
	}
	for _, m := range allowed {
		if m == model {
			return nil
		}
	}
	return ErrVKModelDenied
}

// CheckVKBudget 检查虚拟 key 配额是否足够（预检查，不扣费）。
func (s *Store) CheckVKBudget(vk *VirtualKey) error {
	if vk.BudgetUSD <= 0 {
		return nil // 0=不限制
	}
	// 检查是否需要重置
	now := time.Now().Unix()
	if vk.ResetAt > 0 && now >= vk.ResetAt {
		if err := s.resetVKBudget(vk); err != nil {
			return err
		}
	}
	if vk.UsedUSD >= vk.BudgetUSD {
		return ErrVKBudgetExceeded
	}
	return nil
}

// resetVKBudget 重置虚拟 key 配额（内部调用）。
func (s *Store) resetVKBudget(vk *VirtualKey) error {
	vk.UsedUSD = 0
	now := time.Now()
	switch vk.BudgetReset {
	case "daily":
		vk.ResetAt = now.AddDate(0, 0, 1).Unix()
	case "monthly":
		vk.ResetAt = now.AddDate(0, 1, 0).Unix()
	default:
		vk.ResetAt = 0
	}
	return s.DB.Save(vk).Error
}

// RecordVKUsage 记录虚拟 key 使用量（请求成功后调用，扣除费用）。
func (s *Store) RecordVKUsage(vkID int64, costUSD float64) error {
	return s.DB.Model(&VirtualKey{}).Where("id = ?", vkID).Updates(map[string]interface{}{
		"used_usd":       s.DB.Raw("used_usd + ?", costUSD),
		"total_requests": s.DB.Raw("total_requests + 1"),
		"last_used_at":   time.Now().Unix(),
	}).Error
}

// CheckVKRateLimit 检查虚拟 key 是否超过限流（滑动窗口，过去 1 分钟）。
func (s *Store) CheckVKRateLimit(vkID int64, rpmLimit, tpmLimit int64) error {
	if rpmLimit <= 0 && tpmLimit <= 0 {
		return nil // 都不限制
	}

	now := time.Now().Unix()
	windowStart := now - 60 // 过去 60 秒

	// 查询过去 1 分钟的累计
	var result struct {
		Requests int64
		Tokens   int64
	}
	if err := s.DB.Model(&VKRateLimit{}).
		Select("COALESCE(SUM(requests), 0) as requests, COALESCE(SUM(tokens), 0) as tokens").
		Where("vk_id = ? AND window_start >= ?", vkID, windowStart).
		Scan(&result).Error; err != nil {
		return err
	}

	if rpmLimit > 0 && result.Requests >= rpmLimit {
		return ErrVKRateLimited
	}
	if tpmLimit > 0 && result.Tokens >= tpmLimit {
		return ErrVKRateLimited
	}
	return nil
}

// RecordVKRateLimitHit 记录虚拟 key 限流命中（请求发出前调用）。
func (s *Store) RecordVKRateLimitHit(vkID int64, tokens int64) error {
	now := time.Now().Unix()
	// 按分钟聚合，窗口对齐到分钟起点
	windowStart := now - (now % 60)

	// UPSERT: 存在则累加，不存在则插入
	return s.DB.Exec(`
		INSERT INTO vk_rate_limit (vk_id, window_start, requests, tokens)
		VALUES (?, ?, 1, ?)
		ON CONFLICT(vk_id, window_start) DO UPDATE SET
			requests = requests + 1,
			tokens = tokens + ?
	`, vkID, windowStart, tokens, tokens).Error
}

// CleanupVKRateLimit 清理过期的限流窗口（建议定期调用，如每小时）。
func (s *Store) CleanupVKRateLimit() error {
	cutoff := time.Now().Unix() - 3600 // 保留 1 小时
	return s.DB.Where("window_start < ?", cutoff).Delete(&VKRateLimit{}).Error
}
