package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/cloudomni/omnigate/internal/store"
)

func setupTestDB(t *testing.T) *store.Store {
	t.Helper()
	f, err := os.CreateTemp("", "omnigate-api-test-*.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })
	
	db, err := store.Open(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func withURLParam(r *http.Request, key, value string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, value)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}


func TestVirtualKeyCreate(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	handler := NewVirtualKeyHandler(db)

	req := CreateVirtualKeyRequest{
		Name:          "test-key",
		RPMLimit:      100,
		TPMLimit:      10000,
		BudgetUSD:     50.0,
		BudgetReset:   "daily",
		AllowedModels: []string{"model1", "model2"},
	}
	body, _ := json.Marshal(req)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/virtual-keys", bytes.NewReader(body))

	handler.Create(w, r)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp VirtualKeyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response failed: %v", err)
	}

	if resp.Name != "test-key" {
		t.Errorf("name mismatch: %s", resp.Name)
	}
	if resp.KeyValue[:3] != "vk-" {
		t.Errorf("key_value should start with vk-: %s", resp.KeyValue)
	}
	if len(resp.KeyValue) < 20 {
		t.Error("key_value should be full length on create")
	}
	if resp.RPMLimit != 100 {
		t.Errorf("rpm_limit mismatch: %d", resp.RPMLimit)
	}
	if len(resp.AllowedModels) != 2 {
		t.Errorf("allowed_models mismatch: %v", resp.AllowedModels)
	}
}

func TestVirtualKeyCreateMissingName(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	handler := NewVirtualKeyHandler(db)

	req := CreateVirtualKeyRequest{RPMLimit: 100}
	body, _ := json.Marshal(req)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/virtual-keys", bytes.NewReader(body))

	handler.Create(w, r)

	if w.Code != 400 {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestVirtualKeyList(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	handler := NewVirtualKeyHandler(db)

	// Create some keys
	for i := 0; i < 3; i++ {
		vk := &store.VirtualKey{Name: "test", Status: "active"}
		db.CreateVirtualKey(vk)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/virtual-keys", nil)

	handler.List(w, r)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp []VirtualKeyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if len(resp) != 3 {
		t.Errorf("expected 3 keys, got %d", len(resp))
	}

	// Check keys are masked
	for _, vk := range resp {
		if len(vk.KeyValue) > 20 {
			t.Error("key_value should be masked in list")
		}
	}
}

func TestVirtualKeyGet(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	handler := NewVirtualKeyHandler(db)

	vk := &store.VirtualKey{Name: "test", Status: "active"}
	db.CreateVirtualKey(vk)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/virtual-keys/1", nil)
	r = withURLParam(r, "id", "1")

	handler.Get(w, r)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp VirtualKeyResponse
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp.Name != "test" {
		t.Errorf("name mismatch: %s", resp.Name)
	}
}

func TestVirtualKeyUpdate(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	handler := NewVirtualKeyHandler(db)

	vk := &store.VirtualKey{
		Name:      "test",
		Status:    "active",
		RPMLimit:  100,
		BudgetUSD: 50.0,
	}
	db.CreateVirtualKey(vk)

	newName := "updated"
	newStatus := "disabled"
	newRPM := int64(200)
	req := UpdateVirtualKeyRequest{
		Name:     &newName,
		Status:   &newStatus,
		RPMLimit: &newRPM,
	}
	body, _ := json.Marshal(req)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("PUT", "/api/virtual-keys/1", bytes.NewReader(body))
	r = withURLParam(r, "id", "1")

	handler.Update(w, r)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}

	// Verify update
	vk2, _ := db.GetVirtualKey(1)
	if vk2.Name != "updated" {
		t.Errorf("name not updated: %s", vk2.Name)
	}
	if vk2.Status != "disabled" {
		t.Errorf("status not updated: %s", vk2.Status)
	}
	if vk2.RPMLimit != 200 {
		t.Errorf("rpm_limit not updated: %d", vk2.RPMLimit)
	}
}

func TestVirtualKeyDelete(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	handler := NewVirtualKeyHandler(db)

	vk := &store.VirtualKey{Name: "test", Status: "active"}
	db.CreateVirtualKey(vk)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("DELETE", "/api/virtual-keys/1", nil)
	r = withURLParam(r, "id", "1")

	handler.Delete(w, r)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}

	// Verify deleted
	_, err := db.GetVirtualKey(1)
	if err == nil {
		t.Error("key should be deleted")
	}
}

func TestVirtualKeyResetBudget(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	handler := NewVirtualKeyHandler(db)

	vk := &store.VirtualKey{
		Name:        "test",
		Status:      "active",
		BudgetUSD:   100.0,
		UsedUSD:     80.0,
		BudgetReset: "daily",
	}
	db.CreateVirtualKey(vk)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/virtual-keys/1/reset-budget", nil)
	r = withURLParam(r, "id", "1")

	handler.ResetBudget(w, r)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}

	// Verify reset
	vk2, _ := db.GetVirtualKey(1)
	if vk2.UsedUSD != 0 {
		t.Errorf("used should be 0, got %f", vk2.UsedUSD)
	}
}

func TestVirtualKeyRevealKey(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	handler := NewVirtualKeyHandler(db)

	vk := &store.VirtualKey{Name: "test", Status: "active"}
	db.CreateVirtualKey(vk)

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/virtual-keys/1/reveal-key", nil)
	r = withURLParam(r, "id", "1")

	handler.RevealKey(w, r)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}

	var resp map[string]string
	json.Unmarshal(w.Body.Bytes(), &resp)

	if resp["key_value"] != vk.KeyValue {
		t.Error("full key_value should be revealed")
	}
}
