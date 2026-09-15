package store

import (
	"testing"
	"time"
)

// TestSettleRequestPendingUpdate 覆盖收尾落库主路径：pending 行就地更新为终态，
// 尝试明细、日聚合、VK 用量结算在同一事务内完成。
func TestSettleRequestPendingUpdate(t *testing.T) {
	db := setupTestDB(t)

	vk := &VirtualKey{Name: "test", Status: "active", TotalBudgetUSD: 100}
	if err := db.CreateVirtualKey(vk); err != nil {
		t.Fatal(err)
	}

	// 入口 pending 行
	pending := RequestLog{
		RequestID: "req-1", Route: "r", Endpoint: "completions", Status: "pending",
		VKID: vk.ID, CreatedAt: time.Now().Unix(),
	}
	if err := db.DB.Create(&pending).Error; err != nil {
		t.Fatal(err)
	}

	log := RequestLog{
		ID: pending.ID, RequestID: "req-1", Route: "r", Model: "m", Provider: "p", KeyID: 7,
		Status: "success", IsStream: false,
		PromptTokens: 10, CompletionTokens: 5, Cost: 0.25, Retries: 1, VKID: vk.ID,
		CreatedAt: pending.CreatedAt,
	}
	attempts := []RequestAttempt{
		{RequestID: "req-1", Attempt: 0, Route: "r", Model: "m", Provider: "p", Status: "error", ErrorCode: "500"},
		{RequestID: "req-1", Attempt: 1, Route: "r", Model: "m", Provider: "p", Status: "success"},
	}

	if err := db.SettleRequest(&log, attempts); err != nil {
		t.Fatalf("settle: %v", err)
	}

	// 日志终态：pending 行就地更新，endpoint/created_at 不被零值覆盖
	var got RequestLog
	if err := db.DB.First(&got, pending.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != "success" || got.Retries != 1 || got.Cost != 0.25 {
		t.Fatalf("log not settled: %+v", got)
	}
	if got.Endpoint != "completions" {
		t.Fatalf("endpoint must be preserved from pending row: %q", got.Endpoint)
	}
	if got.CreatedAt != pending.CreatedAt {
		t.Fatalf("created_at must be preserved: %d != %d", got.CreatedAt, pending.CreatedAt)
	}

	// 尝试明细按序落库
	var rows []RequestAttempt
	if err := db.DB.Where("request_id = ?", "req-1").Order("attempt").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Status != "error" || rows[1].Status != "success" {
		t.Fatalf("attempts wrong: %+v", rows)
	}

	// 日聚合只有终态 success 一条
	var daily []RequestLogDaily
	if err := db.DB.Find(&daily).Error; err != nil {
		t.Fatal(err)
	}
	if len(daily) != 1 || daily[0].Status != "success" || daily[0].Total != 1 || daily[0].PromptTokens != 10 {
		t.Fatalf("daily wrong: %+v", daily)
	}

	// VK 用量结算
	updated, err := db.GetVirtualKey(vk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.UsedUSD != 0.25 || updated.TotalRequests != 1 {
		t.Fatalf("vk usage not settled: %+v", updated)
	}
}

// TestSettleRequestCreatePath pendingID=0（pending 行创建失败兜底）时走插入路径。
func TestSettleRequestCreatePath(t *testing.T) {
	db := setupTestDB(t)
	log := RequestLog{RequestID: "req-2", Route: "r", Model: "m", Provider: "p", Status: "client_error", ErrorCode: "400"}
	if err := db.SettleRequest(&log, nil); err != nil {
		t.Fatalf("settle: %v", err)
	}
	var got RequestLog
	if err := db.DB.First(&got, log.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.ID == 0 || got.Status != "client_error" {
		t.Fatalf("log not created: %+v", got)
	}
}

// TestSettleRequestNoVKCreditOnFailure 失败请求不结算 VK 用量。
func TestSettleRequestNoVKCreditOnFailure(t *testing.T) {
	db := setupTestDB(t)
	vk := &VirtualKey{Name: "test", Status: "active"}
	if err := db.CreateVirtualKey(vk); err != nil {
		t.Fatal(err)
	}
	log := RequestLog{RequestID: "req-3", Route: "r", Status: "error", ErrorCode: "timeout", VKID: vk.ID, Cost: 1.0}
	if err := db.SettleRequest(&log, nil); err != nil {
		t.Fatal(err)
	}
	updated, err := db.GetVirtualKey(vk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.UsedUSD != 0 || updated.TotalRequests != 0 {
		t.Fatalf("failure must not credit vk: %+v", updated)
	}
}

// TestSettleRequestNoVKCreditOnEstimated 估算 token 不计费、不结算 VK。
func TestSettleRequestNoVKCreditOnEstimated(t *testing.T) {
	db := setupTestDB(t)
	vk := &VirtualKey{Name: "test", Status: "active"}
	if err := db.CreateVirtualKey(vk); err != nil {
		t.Fatal(err)
	}
	log := RequestLog{RequestID: "req-est", Route: "r", Status: "success", VKID: vk.ID, Cost: 1.0, TokensEstimated: true}
	if err := db.SettleRequest(&log, nil); err != nil {
		t.Fatal(err)
	}
	updated, err := db.GetVirtualKey(vk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.UsedUSD != 0 || updated.TotalRequests != 0 {
		t.Fatalf("estimated usage must not credit vk: %+v", updated)
	}
}

// TestLegacyVKRateLimitsTableDropped 旧版 DB 落库限流表在 Open 时被删除。
func TestLegacyVKRateLimitsTableDropped(t *testing.T) {
	db := setupTestDB(t)
	if db.DB.Migrator().HasTable("vk_rate_limits") {
		t.Fatal("legacy vk_rate_limits table should be dropped on Open")
	}
}

// TestSettleRequestVKBudgetCap 有额度时 used_usd + cost 不得超过总额，超额不入账。
func TestSettleRequestVKBudgetCap(t *testing.T) {
	db := setupTestDB(t)
	vk := &VirtualKey{Name: "capped", Status: "active", TotalBudgetUSD: 1.0, UsedUSD: 0.8}
	if err := db.CreateVirtualKey(vk); err != nil {
		t.Fatal(err)
	}
	log := RequestLog{RequestID: "req-cap", Route: "r", Status: "success", VKID: vk.ID, Cost: 0.5}
	if err := db.SettleRequest(&log, nil); err != nil {
		t.Fatal(err)
	}
	updated, err := db.GetVirtualKey(vk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.UsedUSD != 0.8 || updated.TotalRequests != 0 {
		t.Fatalf("over-budget settle must not credit: %+v", updated)
	}
}

// TestSettleRequestVKUnlimitedStillCredits 总额度 0=不限制，仍累加 used_usd。
func TestSettleRequestVKUnlimitedStillCredits(t *testing.T) {
	db := setupTestDB(t)
	vk := &VirtualKey{Name: "open", Status: "active", TotalBudgetUSD: 0}
	if err := db.CreateVirtualKey(vk); err != nil {
		t.Fatal(err)
	}
	log := RequestLog{RequestID: "req-open", Route: "r", Status: "success", VKID: vk.ID, Cost: 12.5}
	if err := db.SettleRequest(&log, nil); err != nil {
		t.Fatal(err)
	}
	updated, err := db.GetVirtualKey(vk.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.UsedUSD != 12.5 || updated.TotalRequests != 1 {
		t.Fatalf("unlimited vk must still credit: %+v", updated)
	}
}

// TestReclaimPendingOnOpen 崩溃遗留的 pending 行在 Open 时改成 error(interrupted)。
func TestReclaimPendingOnOpen(t *testing.T) {
	db := setupTestDB(t)
	now := time.Now().Unix()
	if err := db.DB.Create(&RequestLog{
		RequestID: "stale-pending", Route: "r", Status: "pending", CreatedAt: now - 30,
	}).Error; err != nil {
		t.Fatal(err)
	}
	db.reclaimPending()
	var got RequestLog
	if err := db.DB.Where("request_id = ?", "stale-pending").First(&got).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != "error" || got.ErrorCode != "interrupted" {
		t.Fatalf("pending must be reclaimed: %+v", got)
	}
}
