package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"gorm.io/gorm"
)

var (
	ErrVKNotFound       = errors.New("virtual key not found")
	ErrVKDisabled       = errors.New("virtual key disabled")
	ErrVKBudgetExceeded = errors.New("budget exceeded")
	ErrVKAccessDenied   = errors.New("route not allowed")
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

// normalizeVKAllowedRoutes 历史版本经 API 层把 allowed_routes 写成路由名数组，
// 而校验层按 ID 数组解析，配置了限制的 VK 必然在请求时报 invalid allowed_routes。
// 本迁移把名称统一改写为 ID 数组（数字字符串按 ID 保留，未知名剔除），
// 非数组内容保持原样交由请求时校验报错。幂等：能解析为 []int64 的行直接跳过。
func normalizeVKAllowedRoutes(db *gorm.DB) error {
	var vks []VirtualKey
	if err := db.Find(&vks).Error; err != nil {
		return err
	}
	if len(vks) == 0 {
		return nil
	}
	var routes []Route
	if err := db.Find(&routes).Error; err != nil {
		return err
	}
	byName := make(map[string]int64, len(routes))
	for _, rt := range routes {
		byName[rt.Name] = rt.ID
	}
	for i := range vks {
		vk := &vks[i]
		if vk.AllowedRoutes == "" || vk.AllowedRoutes == "[]" {
			continue
		}
		var ids []int64
		if json.Unmarshal([]byte(vk.AllowedRoutes), &ids) == nil {
			continue
		}
		var names []string
		if json.Unmarshal([]byte(vk.AllowedRoutes), &names) != nil {
			continue
		}
		out := make([]int64, 0, len(names))
		for _, n := range names {
			if id, ok := byName[n]; ok {
				out = append(out, id)
				continue
			}
			// 旧数据可能以数字字符串形式存 ID
			if id, err := strconv.ParseInt(n, 10, 64); err == nil {
				out = append(out, id)
			}
		}
		b, err := json.Marshal(out)
		if err != nil {
			return err
		}
		if err := db.Model(&VirtualKey{}).Where("id = ?", vk.ID).Update("allowed_routes", string(b)).Error; err != nil {
			return err
		}
	}
	return nil
}
