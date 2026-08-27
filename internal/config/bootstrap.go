package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Bootstrap 是启动层配置：启动时确定、运行期不变（监听地址、管理令牌）。
type Bootstrap struct {
	Server struct {
		Listen string `yaml:"listen"`
	} `yaml:"server"`
	Admin struct {
		Token string `yaml:"token"`
	} `yaml:"admin"`
}

const defaultListen = "127.0.0.1:17777"

const bootstrapTemplate = `# OmniGate 启动层配置（熔断/限流/内容捕获等运行层配置在 Web 管理界面中热生效）
server:
  # 监听地址（默认 127.0.0.1:17777，管理界面与代理共用）。仅本机访问保持 127.0.0.1；局域网访问改为 0.0.0.0
  listen: %s

admin:
  # 管理面访问令牌；留空 = 管理面无鉴权（纯本地使用）。
  # 设置后所有 /api/* 请求需携带 X-Admin-Token 头。/v1/* 代理面永不鉴权。
  token: ""
`

// LoadBootstrap 读取启动层配置；文件不存在时生成默认模板并返回默认值。
func LoadBootstrap(path string) (Bootstrap, error) {
	var boot Bootstrap
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr != nil {
			return boot, fmt.Errorf("create config dir: %w", mkErr)
		}
		tpl := fmt.Sprintf(bootstrapTemplate, defaultListen)
		if wErr := os.WriteFile(path, []byte(tpl), 0o600); wErr != nil {
			return boot, fmt.Errorf("write default config: %w", wErr)
		}
		data = []byte(tpl)
	} else if err != nil {
		return boot, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(data, &boot); err != nil {
		return boot, fmt.Errorf("parse config %s: %w", path, err)
	}
	if boot.Server.Listen == "" {
		boot.Server.Listen = defaultListen
	}
	return boot, nil
}
