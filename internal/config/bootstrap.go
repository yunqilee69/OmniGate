package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// Bootstrap 是启动层配置：启动时确定、运行期不变（监听地址、管理鉴权、网关调用密钥）。
type Bootstrap struct {
	Server struct {
		Listen string `yaml:"listen"`
	} `yaml:"server"`
	Admin struct {
		Username string `yaml:"username"`
		Password string `yaml:"password"`
		ApiKey   string `yaml:"api_key"`
	} `yaml:"admin"`
}

const defaultListen = "127.0.0.1:17777"

const bootstrapTemplate = `# OmniGate 启动层配置（熔断/限流/内容捕获等运行层配置在 Web 管理界面中热生效）
server:
  # 监听地址（默认 127.0.0.1:17777，管理界面与代理共用）。仅本机访问保持 127.0.0.1；局域网访问改为 0.0.0.0
  listen: %s

admin:
  # 账号密码（Web 管理台登录）：设置后进入管理台需登录；同时也可作为 /v1 调用凭据。
  # 用户名不可包含冒号；API 调用遵循 HTTP Basic 规则（RFC 7617）：
  #   Authorization: Basic base64(用户名:密码)
  # OpenAI SDK 场景 api_key 可直接填 base64(用户名:密码) 或 "用户名:密码" 原文（Bearer 传递）。
  username: ""
  password: ""
  # 网关 API 密钥（调用 /v1 专用，不用于 Web 登录）：设置后 /v1 需携带
  #   Authorization: Bearer <api_key>
  # 与账号密码凭据任选其一；账号密码留空而仅设此项 = 本地免登录 + 远程调用带密钥。
  api_key: ""
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
	if err := boot.validateAdmin(); err != nil {
		return boot, fmt.Errorf("config %s: %w", path, err)
	}
	return boot, nil
}

// validateAdmin 校验账号密码组合的完整性：设了用户名就必须设密码，
// 用户名禁含冒号（Basic 凭据以首个冒号分隔用户名与密码，用户名带冒号将无法正确解析）。
func (b Bootstrap) validateAdmin() error {
	if b.Admin.Username == "" {
		return nil
	}
	if strings.Contains(b.Admin.Username, ":") {
		return fmt.Errorf("admin.username 不能包含冒号（Basic 凭据分隔符）")
	}
	if b.Admin.Password == "" {
		return fmt.Errorf("admin.username 已设置但 admin.password 为空")
	}
	return nil
}
