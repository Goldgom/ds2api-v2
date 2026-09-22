package accounts

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"ds2api/internal/config"
)

func deviceIDByEmail(t *testing.T, h *Handler, email string) string {
	t.Helper()
	acc, ok := h.Store.FindAccount(email)
	if !ok {
		t.Fatalf("account %s not found", email)
	}
	return acc.DeviceID
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return payload
}

func TestResetAccountDeviceIDIssuesFreshIDAndClearsToken(t *testing.T) {
	h := newAdminTestHandler(t, `{
		"accounts":[{"email":"u@example.com","password":"pwd","device_id":"Bold-device"}]
	}`)
	if err := h.Store.UpdateAccountToken("u@example.com", "old-token"); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	r := chi.NewRouter()
	r.Post("/admin/accounts/{identifier}/device-id/reset", h.resetAccountDeviceID)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, adminReq(http.MethodPost, "/admin/accounts/u@example.com/device-id/reset", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}

	payload := decodeJSON(t, rec)
	if ok, _ := payload["success"].(bool); !ok {
		t.Fatalf("expected success=true, got %#v", payload)
	}
	preview, _ := payload["device_id_preview"].(string)
	if preview == "" || !strings.Contains(preview, "****") {
		t.Fatalf("expected masked device_id_preview, got %q", preview)
	}

	acc, _ := h.Store.FindAccount("u@example.com")
	if acc.DeviceID == "" || acc.DeviceID == "Bold-device" {
		t.Fatalf("expected a fresh device_id, got %q", acc.DeviceID)
	}
	if !strings.HasPrefix(acc.DeviceID, "B") {
		t.Fatalf("device_id must keep the B prefix, got %q", acc.DeviceID)
	}
	if acc.Token != "" {
		t.Fatalf("expected token to be cleared so the next request re-logs in, got %q", acc.Token)
	}
	if acc.Password != "pwd" {
		t.Fatalf("reset must not touch the password, got %q", acc.Password)
	}
}

func TestResetAccountDeviceIDIsolatesOtherAccounts(t *testing.T) {
	h := newAdminTestHandler(t, `{
		"accounts":[
			{"email":"a@example.com","password":"pwd","device_id":"Bshared"},
			{"email":"b@example.com","password":"pwd","device_id":"Bshared"}
		]
	}`)

	r := chi.NewRouter()
	r.Post("/admin/accounts/{identifier}/device-id/reset", h.resetAccountDeviceID)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, adminReq(http.MethodPost, "/admin/accounts/a@example.com/device-id/reset", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}

	if got := deviceIDByEmail(t, h, "a@example.com"); got == "Bshared" {
		t.Fatal("target account device_id was not reset")
	}
	if got := deviceIDByEmail(t, h, "b@example.com"); got != "Bshared" {
		t.Fatalf("unrelated account device_id changed: %q", got)
	}
}

func TestResetAccountDeviceIDUnknownAccountReturns404(t *testing.T) {
	h := newAdminTestHandler(t, `{"accounts":[{"email":"u@example.com","password":"pwd"}]}`)

	r := chi.NewRouter()
	r.Post("/admin/accounts/{identifier}/device-id/reset", h.resetAccountDeviceID)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, adminReq(http.MethodPost, "/admin/accounts/missing@example.com/device-id/reset", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestResetAllAccountsDeviceIDsIssuesDistinctIDs(t *testing.T) {
	h := newAdminTestHandler(t, `{
		"accounts":[
			{"email":"a@example.com","password":"pwd","device_id":"Bshared"},
			{"email":"b@example.com","password":"pwd","device_id":"Bshared"},
			{"email":"c@example.com","password":"pwd","device_id":"Bshared"}
		]
	}`)

	r := chi.NewRouter()
	r.Post("/admin/accounts/device-id/reset-all", h.resetAllAccountsDeviceIDs)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, adminReq(http.MethodPost, "/admin/accounts/device-id/reset-all", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}

	payload := decodeJSON(t, rec)
	if total, _ := payload["total"].(float64); total != 3 {
		t.Fatalf("expected total=3, got %#v", payload["total"])
	}

	seen := map[string]string{}
	for _, email := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		got := deviceIDByEmail(t, h, email)
		if got == "" || got == "Bshared" {
			t.Fatalf("account %s device_id was not reset: %q", email, got)
		}
		if prev, dup := seen[got]; dup {
			t.Fatalf("device_id collision between %s and %s: %q", prev, email, got)
		}
		seen[got] = email
	}
}

func TestResetAllAccountsDeviceIDsHonorsIdentifierFilter(t *testing.T) {
	h := newAdminTestHandler(t, `{
		"accounts":[
			{"email":"a@example.com","password":"pwd","device_id":"Bshared"},
			{"email":"b@example.com","password":"pwd","device_id":"Bshared"}
		]
	}`)

	r := chi.NewRouter()
	r.Post("/admin/accounts/device-id/reset-all", h.resetAllAccountsDeviceIDs)

	body := []byte(`{"identifiers":["b@example.com"]}`)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, adminReq(http.MethodPost, "/admin/accounts/device-id/reset-all", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}

	if got := deviceIDByEmail(t, h, "a@example.com"); got != "Bshared" {
		t.Fatalf("non-selected account should be untouched, got %q", got)
	}
	if got := deviceIDByEmail(t, h, "b@example.com"); got == "Bshared" {
		t.Fatal("selected account device_id was not reset")
	}
}

func TestListAccountsExposesMaskedDeviceID(t *testing.T) {
	h := newAdminTestHandler(t, `{
		"accounts":[{"email":"u@example.com","password":"pwd","device_id":"Bfull-device-value"}]
	}`)

	req := httptest.NewRequest(http.MethodGet, "/admin/accounts?page=1&page_size=10", nil)
	rec := httptest.NewRecorder()
	h.listAccounts(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}

	payload := decodeJSON(t, rec)
	items, _ := payload["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	item, _ := items[0].(map[string]any)
	if has, _ := item["has_device_id"].(bool); !has {
		t.Fatalf("expected has_device_id=true, got %#v", item["has_device_id"])
	}
	preview, _ := item["device_id_preview"].(string)
	if preview == "" || strings.Contains(preview, "full-device-value") {
		t.Fatalf("expected masked preview, got %q", preview)
	}
}

func TestResetAccountDeviceIDReportsEnvBackedPersistenceWarning(t *testing.T) {
	t.Setenv("DS2API_ENV_WRITEBACK", "0")
	h := newAdminTestHandler(t, `{"accounts":[{"email":"u@example.com","password":"pwd"}]}`)
	if !h.Store.IsEnvBacked() {
		t.Fatal("expected env-backed store for this test")
	}

	r := chi.NewRouter()
	r.Post("/admin/accounts/{identifier}/device-id/reset", h.resetAccountDeviceID)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, adminReq(http.MethodPost, "/admin/accounts/u@example.com/device-id/reset", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if warning, _ := decodeJSON(t, rec)["config_warning"].(string); warning == "" {
		t.Fatal("expected config_warning when the store cannot persist to disk")
	}
}

// 路由级测试：确认 /accounts/device-id/reset-all 不会被 /accounts/{identifier}/... 抢匹配。
func TestDeviceIDResetRoutesAreRegistered(t *testing.T) {
	router := newHTTPAdminHarness(t, `{
		"accounts":[{"email":"a@example.com","password":"pwd"},{"email":"b@example.com","password":"pwd"}]
	}`, &testingDSMock{})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, adminReq(http.MethodPost, "/accounts/a@example.com/device-id/reset", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("single reset route: %d body=%s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, adminReq(http.MethodPost, "/accounts/device-id/reset-all", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("batch reset route: %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestNewDeviceIDIsUniqueAndPrefixed(t *testing.T) {
	seen := map[string]struct{}{}
	for range 16 {
		id, err := config.NewDeviceID()
		if err != nil {
			t.Fatalf("NewDeviceID: %v", err)
		}
		if !strings.HasPrefix(id, "B") {
			t.Fatalf("expected B prefix, got %q", id)
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate device id generated: %q", id)
		}
		seen[id] = struct{}{}
	}
}
