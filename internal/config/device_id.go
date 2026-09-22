package config

import (
	"crypto/rand"
	"encoding/base64"
)

// NewDeviceID 生成 DeepSeek Web 客户端形态的设备指纹："B" + base64(64 随机字节)。
//
// 设备指纹会随登录请求一起提交，参与上游的登录风控判定；一旦某个指纹被标记，
// 复用该指纹的账号会持续登录失败（RISK_DEVICE_DETECTED）。登录链路与 Admin
// 重置接口共用本函数，保证指纹格式唯一且不可预测。
func NewDeviceID() (string, error) {
	buf := make([]byte, 64)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "B" + base64.StdEncoding.EncodeToString(buf), nil
}
