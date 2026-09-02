package store

import (
	"os"
	"testing"
	"time"
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
	if len(token) < 10 {
		t.Errorf("token too short: %s", token)
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
		Name:      "test-key",
		Status:    "active",
		RPMLimit:  100,
		TPMLimit:  10000,
		BudgetUSD: 10.0,
		BudgetReset: "daily",
		AllowedModels: `["model1","model2"]`,
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
	vks, err := db.ListVirtualKeys()
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(vks) != 1 {
		t.Errorf("expected 1 key, got %d", len(vks))
	}

	// Update
	vk.Name = "updated-key"
	vk.Status = "disabled"
	if err := db.UpdateVirtualKey(vk); err != nil {
		t.Fatalf("update failed: %v", err)
	}
	vk4, _ := db.GetVirtualKey(vk.ID)
	if vk4.Name != "updated-key" || vk4.Status != "disabled" {
		t.Error("update not applied")
	}

	// Delete
	if err := db.DeleteVirtualKey(vk.ID); err != nil {
		t.Fatalf("delete failed: %v", err)
	}
	_, err = db.GetVirtualKey(vk.ID)
	if err == nil {
		t.Error("key should be deleted")
	}
}

func TestCheckVKAuth(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	vk := &VirtualKey{Name: "test", Status: "active"}
	db.CreateVirtualKey(vk)

	tests := []struct {
		name      string
		keyValue  string
		wantErr   error
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

func TestCheckVKModelAccess(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	tests := []struct {
		name          string
		allowedModels string
		checkModel    string
		wantErr       error
	}{
		{"empty allows all", "", "any-model", nil},
		{"empty array allows all", "[]", "any-model", nil},
		{"model in list", `["model1","model2"]`, "model1", nil},
		{"model not in list", `["model1","model2"]`, "model3", ErrVKModelDenied},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vk := &VirtualKey{
				Name:          "test",
				Status:        "active",
				AllowedModels: tt.allowedModels,
			}
			db.CreateVirtualKey(vk)
			defer db.DeleteVirtualKey(vk.ID)

			err := db.CheckVKModelAccess(vk, tt.checkModel)
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
		name      string
		budgetUSD float64
		usedUSD   float64
		wantErr   error
	}{
		{"no limit", 0, 100, nil},
		{"under budget", 100, 50, nil},
		{"at budget", 100, 100, ErrVKBudgetExceeded},
		{"over budget", 100, 150, ErrVKBudgetExceeded},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vk := &VirtualKey{
				Name:      "test",
				Status:    "active",
				BudgetUSD: tt.budgetUSD,
				UsedUSD:   tt.usedUSD,
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

func TestRecordVKUsage(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	vk := &VirtualKey{
		Name:      "test",
		Status:    "active",
		UsedUSD:   5.0,
		BudgetUSD: 100.0,
	}
	db.CreateVirtualKey(vk)

	// Record usage
	if err := db.RecordVKUsage(vk.ID, 2.5); err != nil {
		t.Fatalf("record usage failed: %v", err)
	}

	// Check updated values
	vk2, _ := db.GetVirtualKey(vk.ID)
	if vk2.UsedUSD != 7.5 {
		t.Errorf("expected used=7.5, got %f", vk2.UsedUSD)
	}
	if vk2.TotalRequests != 1 {
		t.Errorf("expected total_requests=1, got %d", vk2.TotalRequests)
	}
	if vk2.LastUsedAt == 0 {
		t.Error("last_used_at should be set")
	}
}

func TestCheckVKRateLimit(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	vk := &VirtualKey{
		Name:     "test",
		Status:   "active",
		RPMLimit: 10,
		TPMLimit: 1000,
	}
	db.CreateVirtualKey(vk)

	// Record some hits
	for i := 0; i < 5; i++ {
		db.RecordVKRateLimitHit(vk.ID, 100)
	}

	// Should not be limited yet (5 < 10)
	if err := db.CheckVKRateLimit(vk.ID, vk.RPMLimit, vk.TPMLimit); err != nil {
		t.Errorf("should not be limited: %v", err)
	}

	// Add more hits
	for i := 0; i < 6; i++ {
		db.RecordVKRateLimitHit(vk.ID, 100)
	}

	// Should be limited now (11 >= 10)
	if err := db.CheckVKRateLimit(vk.ID, vk.RPMLimit, vk.TPMLimit); err != ErrVKRateLimited {
		t.Errorf("expected rate limited, got %v", err)
	}
}

func TestCheckVKRateLimitTokens(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	vk := &VirtualKey{
		Name:     "test",
		Status:   "active",
		RPMLimit: 100,
		TPMLimit: 500,
	}
	db.CreateVirtualKey(vk)

	// Record hits that exceed TPM
	for i := 0; i < 3; i++ {
		db.RecordVKRateLimitHit(vk.ID, 200)
	}

	// Should be limited by tokens (600 >= 500)
	if err := db.CheckVKRateLimit(vk.ID, vk.RPMLimit, vk.TPMLimit); err != ErrVKRateLimited {
		t.Errorf("expected rate limited by tokens, got %v", err)
	}
}

func TestCleanupVKRateLimit(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	vk := &VirtualKey{Name: "test", Status: "active"}
	db.CreateVirtualKey(vk)

	// Insert old records
	oldWindow := time.Now().Unix() - 7200 // 2 hours ago
	db.DB.Create(&VKRateLimit{
		VKId:        vk.ID,
		WindowStart: oldWindow,
		Requests:    10,
		Tokens:      1000,
	})

	// Insert recent records
	recentWindow := time.Now().Unix() - 60
	db.DB.Create(&VKRateLimit{
		VKId:        vk.ID,
		WindowStart: recentWindow,
		Requests:    5,
		Tokens:      500,
	})

	// Cleanup
	if err := db.CleanupVKRateLimit(); err != nil {
		t.Fatalf("cleanup failed: %v", err)
	}

	// Check old records removed
	var count int64
	db.DB.Model(&VKRateLimit{}).Where("window_start = ?", oldWindow).Count(&count)
	if count != 0 {
		t.Error("old records should be removed")
	}

	// Check recent records kept
	db.DB.Model(&VKRateLimit{}).Where("window_start = ?", recentWindow).Count(&count)
	if count != 1 {
		t.Error("recent records should be kept")
	}
}

func TestBudgetReset(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	now := time.Now().Unix()
	vk := &VirtualKey{
		Name:        "test",
		Status:      "active",
		BudgetUSD:   100.0,
		UsedUSD:     80.0,
		BudgetReset: "daily",
		ResetAt:     now - 1, // 已过期
	}
	db.CreateVirtualKey(vk)

	// Check budget (should trigger reset)
	if err := db.CheckVKBudget(vk); err != nil {
		t.Fatalf("check budget failed: %v", err)
	}

	// Verify reset
	vk2, _ := db.GetVirtualKey(vk.ID)
	if vk2.UsedUSD != 0 {
		t.Errorf("used should be reset to 0, got %f", vk2.UsedUSD)
	}
	if vk2.ResetAt <= now {
		t.Error("reset_at should be updated to future")
	}
}
