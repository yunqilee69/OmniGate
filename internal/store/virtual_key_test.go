package store

import (
	"os"
	"testing"
)

func setupTestDB(t *testing.T) *Store {
	t.Helper()
	f, err := os.CreateTemp("", "omnigate-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	db, err := Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestGenerateVKToken(t *testing.T) {
	token := GenerateVKToken()
	if len(token) != 35 {
		t.Errorf("token length = %d, want 35 (vk- + 16-byte hex): %s", len(token), token)
	}
	if token[:3] != "vk-" {
		t.Errorf("token should start with vk-: %s", token)
	}

	// 生成两个 token 应该不同
	token2 := GenerateVKToken()
	if token == token2 {
		t.Errorf("tokens should be unique")
	}
}

func TestVirtualKeyCRUD(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	// Create
	vk := &VirtualKey{
		Name:           "test-key",
		Status:         "active",
		RPMLimit:       100,
		TotalBudgetUSD: 10.0,
		AllowedRoutes:  `[1,2]`,
	}
	if err := db.CreateVirtualKey(vk); err != nil {
		t.Fatalf("create failed: %v", err)
	}
	if vk.ID == 0 {
		t.Error("id should be set")
	}
	if vk.KeyValue[:3] != "vk-" {
		t.Errorf("key_value should start with vk-: %s", vk.KeyValue)
	}

	// Get by value
	vk2, err := db.GetVirtualKeyByValue(vk.KeyValue)
	if err != nil {
		t.Fatalf("get by value failed: %v", err)
	}
	if vk2.Name != "test-key" {
		t.Errorf("name mismatch: %s", vk2.Name)
	}

	// Get by ID
	vk3, err := db.GetVirtualKey(vk.ID)
	if err != nil {
		t.Fatalf("get by id failed: %v", err)
	}
	if vk3.KeyValue != vk.KeyValue {
		t.Error("key_value mismatch")
	}

	// List
	all, err := db.ListVirtualKeys()
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("expected 1 key, got %d", len(all))
	}

	// Update
	vk.Name = "updated"
	vk.RPMLimit = 200
	if err := db.UpdateVirtualKey(vk); err != nil {
		t.Fatalf("update failed: %v", err)
	}

	vk4, _ := db.GetVirtualKey(vk.ID)
	if vk4.Name != "updated" || vk4.RPMLimit != 200 {
		t.Error("update not reflected")
	}

	// Delete
	if err := db.DeleteVirtualKey(vk.ID); err != nil {
		t.Fatalf("delete failed: %v", err)
	}

	_, err = db.GetVirtualKey(vk.ID)
	if err == nil {
		t.Error("should not find deleted key")
	}
}

func TestUpdateVirtualKeyPreservesUsedUSD(t *testing.T) {
	db := setupTestDB(t)
	vk := &VirtualKey{Name: "orig", Status: "active", RPMLimit: 10, TotalBudgetUSD: 5, UsedUSD: 1.25, TotalRequests: 7, LastUsedAt: 100}
	if err := db.CreateVirtualKey(vk); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.Model(&VirtualKey{}).Where("id = ?", vk.ID).Updates(map[string]any{
		"used_usd": 3.5, "total_requests": 9, "last_used_at": 200,
	}).Error; err != nil {
		t.Fatal(err)
	}
	stale := *vk
	stale.Name = "renamed"
	stale.UsedUSD = 1.25
	stale.TotalRequests = 7
	stale.LastUsedAt = 100
	if err := db.UpdateVirtualKey(&stale); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetVirtualKey(vk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "renamed" {
		t.Fatalf("name not updated: %+v", got)
	}
	if got.UsedUSD != 3.5 || got.TotalRequests != 9 || got.LastUsedAt != 200 {
		t.Fatalf("usage columns must survive config update: %+v", got)
	}
}

func TestResetVirtualKeyBudget(t *testing.T) {
	db := setupTestDB(t)
	vk := &VirtualKey{Name: "b", Status: "active", UsedUSD: 4.2, TotalRequests: 3}
	if err := db.CreateVirtualKey(vk); err != nil {
		t.Fatal(err)
	}
	if err := db.ResetVirtualKeyBudget(vk.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetVirtualKey(vk.ID)
	if got.UsedUSD != 0 {
		t.Fatalf("used_usd want 0, got %v", got.UsedUSD)
	}
	if got.TotalRequests != 3 {
		t.Fatalf("reset-budget must not clear total_requests: %+v", got)
	}
}

func TestCheckVKAuth(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	vk := &VirtualKey{Name: "test", Status: "active"}
	db.CreateVirtualKey(vk)

	tests := []struct {
		name     string
		keyValue string
		wantErr  error
	}{
		{"valid", vk.KeyValue, nil},
		{"not found", "vk-invalid", ErrVKNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := db.CheckVKAuth(tt.keyValue)
			if err != tt.wantErr {
				t.Errorf("expected %v, got %v", tt.wantErr, err)
			}
		})
	}

	// Test disabled key
	vk.Status = "disabled"
	db.UpdateVirtualKey(vk)
	_, err := db.CheckVKAuth(vk.KeyValue)
	if err != ErrVKDisabled {
		t.Errorf("expected ErrVKDisabled, got %v", err)
	}
}

func TestCheckVKRouteAccess(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	tests := []struct {
		name          string
		allowedRoutes string
		checkRouteID  int64
		wantErr       error
	}{
		{"empty allows all", "", 1, nil},
		{"empty array allows all", "[]", 1, nil},
		{"route in list", `[1,2]`, 1, nil},
		{"route not in list", `[1,2]`, 3, ErrVKAccessDenied},
		{"direct model denied when allowlist set", `[1,2]`, 0, ErrVKAccessDenied},
		{"direct model allowed when unrestricted", "[]", 0, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vk := &VirtualKey{
				Name:          "test",
				Status:        "active",
				AllowedRoutes: tt.allowedRoutes,
			}
			db.CreateVirtualKey(vk)
			defer db.DeleteVirtualKey(vk.ID)

			err := db.CheckVKRouteAccess(vk, tt.checkRouteID)
			if err != tt.wantErr {
				t.Errorf("expected %v, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestCheckVKBudget(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	tests := []struct {
		name           string
		totalBudgetUSD float64
		usedUSD        float64
		wantErr        error
	}{
		{"no limit", 0, 100, nil},
		{"under budget", 100, 50, nil},
		{"at budget", 100, 100, ErrVKBudgetExceeded},
		{"over budget", 100, 150, ErrVKBudgetExceeded},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vk := &VirtualKey{
				Name:           "test",
				Status:         "active",
				TotalBudgetUSD: tt.totalBudgetUSD,
				UsedUSD:        tt.usedUSD,
			}
			db.CreateVirtualKey(vk)
			defer db.DeleteVirtualKey(vk.ID)

			err := db.CheckVKBudget(vk)
			if err != tt.wantErr {
				t.Errorf("expected %v, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestCheckVKBudgetReloadsUsedUSD(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	vk := &VirtualKey{Name: "stale", Status: "active", TotalBudgetUSD: 10, UsedUSD: 1}
	if err := db.CreateVirtualKey(vk); err != nil {
		t.Fatal(err)
	}
	if err := db.DB.Model(&VirtualKey{}).Where("id = ?", vk.ID).Update("used_usd", 10).Error; err != nil {
		t.Fatal(err)
	}
	stale := &VirtualKey{ID: vk.ID, TotalBudgetUSD: 10, UsedUSD: 1}
	if err := db.CheckVKBudget(stale); err != ErrVKBudgetExceeded {
		t.Fatalf("stale snapshot must reload used_usd, err=%v", err)
	}
	if stale.UsedUSD != 10 {
		t.Fatalf("used_usd should be copied onto snapshot, got %v", stale.UsedUSD)
	}
}

func TestNormalizeVKAllowedRoutes(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	db.DB.Create(&Route{Name: "alpha"})
	db.DB.Create(&Route{Name: "beta"})

	rows := []VirtualKey{
		{Name: "legacy", AllowedRoutes: `["alpha","ghost"]`},
		{Name: "numeric", AllowedRoutes: `["1","2"]`},
		{Name: "idfmt", AllowedRoutes: `[1]`},
		{Name: "junk", AllowedRoutes: `"weird"`},
	}
	for i := range rows {
		if err := db.CreateVirtualKey(&rows[i]); err != nil {
			t.Fatalf("seed %s: %v", rows[i].Name, err)
		}
	}

	if err := normalizeVKAllowedRoutes(db.DB); err != nil {
		t.Fatalf("normalize: %v", err)
	}

	get := func(name string) string {
		t.Helper()
		var vk VirtualKey
		if err := db.DB.Where("name = ?", name).First(&vk).Error; err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		return vk.AllowedRoutes
	}
	if got := get("legacy"); got != `[1]` {
		t.Errorf("legacy = %s, want [1] (未知名 ghost 剔除)", got)
	}
	if got := get("numeric"); got != `[1,2]` {
		t.Errorf("numeric = %s, want [1,2]", got)
	}
	if got := get("idfmt"); got != `[1]` {
		t.Errorf("idfmt = %s, want unchanged [1]", got)
	}
	if got := get("junk"); got != `"weird"` {
		t.Errorf("junk = %s, want unchanged", got)
	}

	// 幂等：再跑一次结果不变
	if err := normalizeVKAllowedRoutes(db.DB); err != nil {
		t.Fatalf("normalize 2nd: %v", err)
	}
	if got := get("legacy"); got != `[1]` {
		t.Errorf("legacy after 2nd pass = %s, want [1]", got)
	}
}
