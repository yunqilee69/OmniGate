package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Bootstrap 是启动层配置：启动时确定、运行期不变（监听地址、管理鉴权、数据库路径、日志路径）。
type Bootstrap struct {
	Server struct {
		Host string `yaml:"host"`
		Port int    `yaml:"port"`
	} `yaml:"server"`
	Admin struct {
		Username string `yaml:"username"`
		Password string `yaml:"password"`
	} `yaml:"admin"`
	Database struct {
		Path string `yaml:"path"`
	} `yaml:"database"`
	Log struct {
		Path string `yaml:"path"`
	} `yaml:"log"`
}

const defaultHost = "127.0.0.1"
const defaultPort = 17777

const bootstrapTemplate = `# OmniGate 启动层配置（熔断/限流/内容捕获等运行层配置在 Web 管理界面中热生效）
server:
  # 监听地址（默认 127.0.0.1）。仅本机访问保持 127.0.0.1；局域网访问改为 0.0.0.0
  # 可通过环境变量覆盖：SERVER_HOST
  host: %s
  # 监听端口（默认 17777，管理界面与代理共用）
  # 可通过环境变量覆盖：SERVER_PORT
  port: %d

admin:
  # 账号密码（Web 管理台登录）：设置后进入管理台需登录。
  # 用户名不可包含冒号；
  # 可通过环境变量覆盖：ADMIN_USERNAME, ADMIN_PASSWORD
  # 留空 = 本地免登录（适合单机使用）
  username: ""
  password: ""

database:
  # 数据库文件路径（默认 ~/.omnigate/omnigate.db）。
  # 可通过环境变量覆盖：DATABASE_PATH
  path: ""

log:
  # 日志输出路径（默认 ~/.omnigate/omnigate.log）。
  # 支持特殊值：stdout | stderr | off
  # 可通过环境变量覆盖：LOG_PATH
  path: ""

# /v1 网关调用：现已使用虚拟密钥鉴权，在 Web 管理界面的"虚拟密钥"页面创建和管理。
`

// LoadBootstrap 读取启动层配置；文件不存在时生成默认模板并返回默认值。
func LoadBootstrap(path string) (Bootstrap, error) {
	var boot Bootstrap
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		if mkErr := os.MkdirAll(filepath.Dir(path), 0o755); mkErr != nil {
			return boot, fmt.Errorf("create config dir: %w", mkErr)
		}
		tpl := fmt.Sprintf(bootstrapTemplate, defaultHost, defaultPort)
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
	// 应用环境变量覆盖
	if host := os.Getenv("SERVER_HOST"); host != "" {
		boot.Server.Host = host
	}
	if portStr := os.Getenv("SERVER_PORT"); portStr != "" {
		if port, err := strconv.Atoi(portStr); err == nil && port > 0 && port <= 65535 {
			boot.Server.Port = port
		}
	}
	if boot.Server.Host == "" {
		boot.Server.Host = defaultHost
	}
	if boot.Server.Port == 0 {
		boot.Server.Port = defaultPort
	}
	if username := os.Getenv("ADMIN_USERNAME"); username != "" {
		boot.Admin.Username = username
	}
	if password := os.Getenv("ADMIN_PASSWORD"); password != "" {
		boot.Admin.Password = password
	}
	if dbPath := os.Getenv("DATABASE_PATH"); dbPath != "" {
		boot.Database.Path = dbPath
	}
	if logPath := os.Getenv("LOG_PATH"); logPath != "" {
		boot.Log.Path = logPath
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

// Listen 返回 host:port 格式的监听地址。
func (b Bootstrap) Listen() string {
	return net.JoinHostPort(b.Server.Host, strconv.Itoa(b.Server.Port))
}