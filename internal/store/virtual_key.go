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
	ErrVKNotFound           = errors.New("virtual key not found")
	ErrVKDisabled           = errors.New("virtual key disabled")
	ErrVKRateLimitExceeded  = errors.New("rate limit exceeded")
	ErrVKBudgetExceeded     = errors.New("budget exceeded")
	ErrVKAccessDenied       = errors.New("route not allowed")
)

// GenerateVKToken 生成 vk- 前缀的随机 token（16 字节 hex，128 bit 熵，总长 35 字符）。
func GenerateVKToken() string {
	b := make([]byte, 16)
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

// CheckVKRouteAccess 检查虚拟 key 是否允许访问指定路由。
func (s *Store) CheckVKRouteAccess(vk *VirtualKey, routeID int64) error {
	if vk.AllowedRoutes == "" {
		return nil // 空=全部允许
	}
	var allowed []int64
	if err := json.Unmarshal([]byte(vk.AllowedRoutes), &allowed); err != nil {
		return fmt.Errorf("invalid allowed_routes: %w", err)
	}
	if len(allowed) == 0 {
		return nil // 空数组=全部允许
	}
	for _, id := range allowed {
		if id == routeID {
			return nil
		}
	}
	return ErrVKAccessDenied
}

// CheckVKBudget 检查虚拟 key 配额是否足够（预检查，不扣费）。
func (s *Store) CheckVKBudget(vk *VirtualKey) error {
	if vk.TotalBudgetUSD == 0 {
		return nil // 0=不限制
	}
	if vk.UsedUSD >= vk.TotalBudgetUSD {
		return ErrVKBudgetExceeded
	}
	return nil
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
func (s *Store) CheckVKRateLimit(vkID int64, rpmLimit int64) error {
	if rpmLimit == 0 {
		return nil // 0=不限制
	}
	now := time.Now().Unix()
	windowStart := now - 60
	var count int64
	err := s.DB.Model(&VKRateLimit{}).
		Where("vk_id = ? AND minute_ts >= ?", vkID, windowStart).
		Select("COALESCE(SUM(request_count), 0)").
		Scan(&count).Error
	if err != nil {
		return fmt.Errorf("query rate limit: %w", err)
	}
	if count >= rpmLimit {
		return ErrVKRateLimitExceeded
	}
	return nil
}

// RecordVKRateLimitHit 记录虚拟 key 限流命中（handler 返回后调用）。
func (s *Store) RecordVKRateLimitHit(vkID int64) error {
	now := time.Now()
	minuteTs := now.Unix() / 60 * 60
	return s.DB.Exec(`
		INSERT INTO vk_rate_limits (vk_id, minute_ts, request_count)
		VALUES (?, ?, 1)
		ON CONFLICT(vk_id, minute_ts) DO UPDATE SET request_count = request_count + 1
	`, vkID, minuteTs).Error
}

// CleanupVKRateLimit 清理过期的限流窗口（建议定期调用，如每小时）。
func (s *Store) CleanupVKRateLimit() error {
	cutoff := time.Now().Unix() - 3600 // 保留 1 小时
	return s.DB.Where("minute_ts < ?", cutoff).Delete(&VKRateLimit{}).Error
}
