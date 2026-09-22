package accounts

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"ds2api/internal/config"
)

// deviceIDPersistenceWarning 描述设备指纹无法落盘时的风险提示。
const deviceIDPersistenceWarning = "当前为环境变量配置模式且未开启 DS2API_ENV_WRITEBACK，设备指纹仅在内存生效，重启后会丢失。"

// resetAccountDeviceID 为单个账号生成全新的设备指纹。
//
// 设备指纹参与上游登录风控判定：某个指纹一旦被标记，继续复用它的账号会持续返回
// RISK_DEVICE_DETECTED。重置后必须重新登录才会生效，因此这里同时清空内存中的
// token，强制下一次请求走一次全新登录。
func (h *Handler) resetAccountDeviceID(w http.ResponseWriter, r *http.Request) {
	identifier := chi.URLParam(r, "identifier")
	if decoded, err := url.PathUnescape(identifier); err == nil {
		identifier = decoded
	}
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "需要账号标识（identifier / email / mobile）"})
		return
	}

	deviceID, err := config.NewDeviceID()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": "生成设备指纹失败: " + err.Error()})
		return
	}

	err = h.Store.Update(func(c *config.Config) error {
		for i := range c.Accounts {
			if !accountMatchesIdentifier(c.Accounts[i], identifier) {
				continue
			}
			c.Accounts[i].DeviceID = deviceID
			c.Accounts[i].Token = ""
			return nil
		}
		return newRequestError("账号不存在")
	})
	if err != nil {
		if detail, ok := requestErrorDetail(err); ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"detail": detail})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	h.Pool.Reset()

	result := map[string]any{
		"success":           true,
		"identifier":        identifier,
		"has_device_id":     true,
		"device_id_preview": maskSecretPreview(deviceID),
	}
	if h.Store.IsEnvBacked() && !h.Store.IsEnvWritebackEnabled() {
		result["config_warning"] = deviceIDPersistenceWarning
	}
	writeJSON(w, http.StatusOK, result)
}

// resetAllAccountsDeviceIDs 批量重置设备指纹。
//
// 请求体可传 {"identifiers": ["a@b.com", "13800000000"]} 只重置指定账号；
// 不传或传空数组时重置全部账号。每个账号都会拿到独立的随机指纹。
func (h *Handler) resetAllAccountsDeviceIDs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Identifiers []string `json:"identifiers"`
	}
	if r.Body != nil {
		// 请求体可选；解析失败时按“全部账号”处理，避免误伤调用方。
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	targets := make(map[string]struct{}, len(req.Identifiers))
	for _, raw := range req.Identifiers {
		id := strings.TrimSpace(raw)
		if id != "" {
			targets[id] = struct{}{}
		}
	}
	onlySelected := len(targets) > 0

	reset := make([]string, 0, len(targets))
	err := h.Store.Update(func(c *config.Config) error {
		reset = make([]string, 0, len(c.Accounts))
		issued := make(map[string]struct{}, len(c.Accounts))
		for i := range c.Accounts {
			identifier := c.Accounts[i].Identifier()
			if identifier == "" {
				continue
			}
			if onlySelected {
				if _, ok := targets[identifier]; !ok {
					continue
				}
			}
			deviceID, genErr := generateUniqueDeviceID(issued)
			if genErr != nil {
				return genErr
			}
			c.Accounts[i].DeviceID = deviceID
			c.Accounts[i].Token = ""
			reset = append(reset, identifier)
		}
		return nil
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	h.Pool.Reset()

	result := map[string]any{
		"success": true,
		"total":   len(reset),
		"reset":   reset,
	}
	if h.Store.IsEnvBacked() && !h.Store.IsEnvWritebackEnabled() {
		result["config_warning"] = deviceIDPersistenceWarning
	}
	writeJSON(w, http.StatusOK, result)
}

// generateUniqueDeviceID 在本次批量重置内保证指纹互不重复。
func generateUniqueDeviceID(issued map[string]struct{}) (string, error) {
	for attempt := 0; attempt < 8; attempt++ {
		deviceID, err := config.NewDeviceID()
		if err != nil {
			return "", fmt.Errorf("生成设备指纹失败: %w", err)
		}
		if _, dup := issued[deviceID]; dup {
			continue
		}
		issued[deviceID] = struct{}{}
		return deviceID, nil
	}
	return "", fmt.Errorf("生成设备指纹失败: 连续重复")
}
