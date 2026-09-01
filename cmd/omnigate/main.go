// Command omnigate 是 OpenAI 兼容的 AI 网关入口。
//
// 默认把所有运行时数据(数据库、启动层配置、运行日志)集中到 ~/.omnigate/ 下,
// 首次运行无配置时自动初始化该目录与默认 config.yaml,用户无需手工准备。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cloudomni/omnigate/internal/api"
	"github.com/cloudomni/omnigate/internal/config"
	"github.com/cloudomni/omnigate/internal/proxy"
	"github.com/cloudomni/omnigate/internal/store"
)

// 以下变量由 goreleaser 在构建时通过 -ldflags -X 注入。
// 本地 `go build` 时为占位值。
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// appDirName 是用户主目录下集中管理 OmniGate 数据/配置/日志的目录名。
const appDirName = ".omnigate"

// expandHome 把路径开头的 ~ 展开为用户主目录(~user/... 不支持,原样返回)。
func expandHome(p string) string {
	if p == "" || p[0] != '~' {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	if p == "~" {
		return home
	}
	if p[1] == '/' || p[1] == filepath.Separator {
		return filepath.Join(home, p[2:])
	}
	return p
}

// appHomeDir 返回 ~/.omnigate 绝对路径;获取不到主目录时回落到当前目录下 .omnigate。
func appHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", appDirName)
	}
	return filepath.Join(home, appDirName)
}

// isLogToken 判定 --log 参数是否走流/关闭语义(而非文件路径)。
func isLogToken(s string) bool {
	switch s {
	case "stdout", "stderr", "off", "-":
		return true
	}
	return false
}

// setupLogger 配置全局 slog 输出。
//
//   - "stdout"        → 仅 stdout(给开发/前台)
//   - "stderr" / ""   → 仅 stderr
//   - "off" / "-"     → 全部丢弃
//   - 其他路径        → stderr + 文件(终端可见且持久化)
//
// 返回的 io.Closer 是文件句柄(若有),由 main 在退出时 Close。
func setupLogger(logPath string) (io.Closer, error) {
	var w io.Writer
	var file io.Closer

	switch logPath {
	case "stdout":
		w = os.Stdout
	case "", "stderr":
		w = os.Stderr
	case "off", "-":
		w = io.Discard
	default:
		if dir := filepath.Dir(logPath); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("create log dir %s: %w", dir, err)
			}
		}
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, fmt.Errorf("open log file %s: %w", logPath, err)
		}
		file = f
		w = io.MultiWriter(os.Stderr, f)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(w, nil)))
	return file, nil
}

func main() {
	// 解析子命令
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "start":
			startCommand()
			return
		case "stop":
			stopCommand()
			return
		case "status":
			statusCommand()
			return
		case "help", "-h", "--help":
			printHelp()
			return
		case "-version", "--version":
			fmt.Printf("omnigate %s (commit %s, built %s)\n", version, commit, date)
			return
		}
	}

	// 无子命令时默认为 start
	startCommand()
}

func printHelp() {
	fmt.Println("OmniGate - OpenAI 兼容的 AI 网关")
	fmt.Printf("版本: %s\n\n", version)
	fmt.Println("用法:")
	fmt.Println("  omnigate [command] [flags]")
	fmt.Println()
	fmt.Println("可用命令:")
	fmt.Println("  start        启动服务（默认后台模式）")
	fmt.Println("  stop         停止运行中的服务")
	fmt.Println("  status       查看服务状态")
	fmt.Println("  help         显示帮助信息")
	fmt.Println()
	fmt.Println("start 命令标志:")
	fmt.Println("  --db <path>         数据库文件路径 (默认: ~/.omnigate/omnigate.db)")
	fmt.Println("  --config <path>     配置文件路径 (默认: ~/.omnigate/config.yaml)")
	fmt.Println("  --log <path>        日志输出 (默认: ~/.omnigate/omnigate.log)")
	fmt.Println("  --listen <addr>     监听地址 (覆盖配置文件)")
	fmt.Println("  --foreground        前台运行模式")
	fmt.Println("  --version           显示版本信息")
	fmt.Println()
	fmt.Println("示例:")
	fmt.Println("  omnigate              # 后台启动服务")
	fmt.Println("  omnigate start        # 后台启动服务")
	fmt.Println("  omnigate start --foreground  # 前台启动服务")
	fmt.Println("  omnigate stop         # 停止服务")
	fmt.Println("  omnigate status       # 查看状态")
}

func pidFilePath() string {
	return filepath.Join(appHomeDir(), "omnigate.pid")
}

func writePidFile(pid int) error {
	pidFile := pidFilePath()
	return os.WriteFile(pidFile, []byte(strconv.Itoa(pid)), 0o644)
}

func readPidFile() (int, error) {
	pidFile := pidFilePath()
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

func removePidFile() error {
	pidFile := pidFilePath()
	return os.Remove(pidFile)
}

func isProcessRunning(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// 在Unix上，发送信号0检查进程是否存在
	// 在Windows上，FindProcess总是成功，需要尝试其他方法
	err = process.Signal(syscall.Signal(0))
	return err == nil
}

func stopCommand() {
	pid, err := readPidFile()
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("✗ 服务未运行（找不到PID文件）")
		} else {
			fmt.Printf("✗ 读取PID文件失败: %v\n", err)
		}
		os.Exit(1)
	}

	if !isProcessRunning(pid) {
		fmt.Printf("✗ 进程 %d 不存在，清理PID文件\n", pid)
		removePidFile()
		os.Exit(1)
	}

	process, err := os.FindProcess(pid)
	if err != nil {
		fmt.Printf("✗ 查找进程失败: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("正在停止服务 (PID: %d)...\n", pid)
	if err := process.Signal(syscall.SIGTERM); err != nil {
		fmt.Printf("✗ 发送停止信号失败: %v\n", err)
		os.Exit(1)
	}

	// 等待进程退出（最多5秒）
	for range 50 {
		time.Sleep(100 * time.Millisecond)
		if !isProcessRunning(pid) {
			removePidFile()
			fmt.Println("✓ 服务已停止")
			return
		}
	}
}

func statusCommand() {
	pid, err := readPidFile()
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("状态: 未运行")
		} else {
			fmt.Printf("状态: 未知 (读取PID失败: %v)\n", err)
		}
		os.Exit(1)
	}

	if isProcessRunning(pid) {
		fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		fmt.Printf("  OmniGate %s\n", version)
		fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		fmt.Println("  状态: 运行中 ✓")
		fmt.Printf("  PID: %d\n", pid)
		fmt.Printf("  数据目录: %s\n", appHomeDir())
		fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	} else {
		fmt.Println("状态: 未运行 (PID文件存在但进程不存在)")
		removePidFile()
		os.Exit(1)
	}
}

func startCommand() {
	appHome := appHomeDir()

	var (
		dbPath         string
		cfgPath        string
		logPath        string
		listenOverride string
		showVersion    bool
		foreground     bool
		isChild        bool
	)
	
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	fs.StringVar(&dbPath, "db", filepath.Join(appHome, "omnigate.db"),
		"SQLite database file path (default: ~/.omnigate/omnigate.db)")
	fs.StringVar(&cfgPath, "config", filepath.Join(appHome, "config.yaml"),
		"bootstrap config file path (auto-initialized with defaults on first run)")
	fs.StringVar(&logPath, "log", filepath.Join(appHome, "omnigate.log"),
		"log output: file path (default ~/.omnigate/omnigate.log) | stdout | stderr | off")
	fs.StringVar(&listenOverride, "listen", "",
		"override listen address from config.yaml (debug, highest priority)")
	fs.BoolVar(&showVersion, "version", false,
		"print version and exit")
	fs.BoolVar(&foreground, "foreground", false,
		"run in foreground mode with verbose logging (default: false, background mode)")
	fs.BoolVar(&isChild, "child", false,
		"internal flag: marks this process as background child (do not use manually)")
	
	// 跳过 "start" 子命令解析标志
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "start" {
		args = args[1:]
	}
	fs.Parse(args)

	if showVersion {
		fmt.Printf("omnigate %s (commit %s, built %s)\n", version, commit, date)
		return
	}

	// 检查是否已有实例运行
	if !isChild {
		if pid, err := readPidFile(); err == nil && isProcessRunning(pid) {
			fmt.Printf("✗ 服务已在运行 (PID: %d)\n", pid)
			fmt.Println("  使用 'omnigate status' 查看状态")
			fmt.Println("  使用 'omnigate stop' 停止服务")
			os.Exit(1)
		}
	}

	// 如果不是前台模式且不是子进程，重启自己为后台进程
	if !foreground && !isChild {
		args := []string{"start"}
		args = append(args, os.Args[1:]...)
		// 跳过重复的 "start"
		if len(args) > 1 && args[1] == "start" {
			args = append(args[:1], args[2:]...)
		}
		args = append(args, "--child")
		
		cmd := exec.Command(os.Args[0], args...)
		
		// 分离标准输入输出
		cmd.Stdin = nil
		cmd.Stdout = nil
		cmd.Stderr = nil
		
		// 设置进程组（Unix）或创建新进程（Windows）
		cmd.SysProcAttr = daemonSysProcAttr()
		
		if err := cmd.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "✗ 启动后台进程失败: %v\n", err)
			os.Exit(1)
		}
		
		// 写入PID文件
		if err := writePidFile(cmd.Process.Pid); err != nil {
			fmt.Fprintf(os.Stderr, "⚠ 写入PID文件失败: %v\n", err)
		}
		
		fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		fmt.Printf("  OmniGate %s\n", version)
		fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
		fmt.Printf("✓ 服务已在后台启动 (PID: %d)\n", cmd.Process.Pid)
		fmt.Printf("  数据目录: %s\n", appHome)
		fmt.Printf("  查看日志: tail -f %s\n", expandHome(logPath))
		fmt.Println()
		fmt.Println("  命令:")
		fmt.Println("    omnigate status  - 查看状态")
		fmt.Println("    omnigate stop    - 停止服务")
		
		// 父进程退出
		return
	}

	// 子进程写入自己的PID
	if isChild {
		if err := writePidFile(os.Getpid()); err != nil {
			slog.Warn("write pid file failed", "err", err)
		}
		// 确保退出时清理PID文件
		defer removePidFile()
	}

	// 展开 ~ 到用户主目录(日志的流/关闭 token 不展开)
	dbPath = expandHome(dbPath)
	cfgPath = expandHome(cfgPath)
	if !isLogToken(logPath) {
		logPath = expandHome(logPath)
	}

	// 配置日志(失败时 fallback 到 stderr,不阻塞启动)
	logFile, err := setupLogger(logPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v; falling back to stderr\n", err)
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	}
	if logFile != nil {
		defer logFile.Close()
	}

	boot, err := config.LoadBootstrap(cfgPath)
	if err != nil {
		slog.Error("load bootstrap config failed", "err", err, "path", cfgPath)
		os.Exit(1)
	}

	listen := boot.Server.Listen
	if listenOverride != "" {
		listen = listenOverride
	}

	if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			slog.Error("create db dir failed", "err", err, "dir", dir)
			os.Exit(1)
		}
	}

	st, err := store.Open(dbPath)
	if err != nil {
		slog.Error("open store failed", "err", err, "db", dbPath)
		os.Exit(1)
	}

	rt, err := config.NewRuntimeManager(st)
	if err != nil {
		slog.Error("init runtime config failed", "err", err)
		os.Exit(1)
	}

	go func() {
		if err := store.Backfill(st.DB); err != nil {
			slog.Warn("backfill request_log_daily failed", "err", err)
			return
		}
		slog.Info("request_log_daily backfill ok")
	}()

	auth := api.AdminAuth{
		Username: boot.Admin.Username,
		Password: boot.Admin.Password,
		ApiKey:   boot.Admin.ApiKey,
	}
	plane := proxy.New(st, rt)
	srv := api.New(st, rt, auth, plane, plane)
	httpSrv := &http.Server{
		Addr:              listen,
		Handler:           srv.Router(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 等待 HTTP 服务器启动
	serverReady := make(chan struct{})
	go func() {
		// 短暂延迟确保监听已绑定
		time.Sleep(100 * time.Millisecond)
		close(serverReady)
	}()

	// 保留期清理：启动 30s 后先跑一次，之后每小时一次；保留期为 0 的表在 PurgeRetentions 内部跳过。
	go func() {
		boot := time.NewTimer(30 * time.Second)
		defer boot.Stop()
		tick := time.NewTicker(time.Hour)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-boot.C:
			case <-tick.C:
			}
			cfg := rt.Snapshot()
			deleted, err := store.PurgeRetentions(st.DB, cfg.LogRetentionDays, cfg.CaptureRetentionDays)
			if err != nil {
				slog.Warn("retention purge failed", "err", err)
				continue
			}
			if len(deleted) > 0 {
				slog.Info("retention purge done", "deleted", deleted)
			}
		}
	}()

	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server error", "err", err)
			os.Exit(1)
		}
	}()

	// 等待服务器就绪
	<-serverReady

	// 输出启动信息到 stdout（即使在守护模式下也输出）
	accessURL := fmt.Sprintf("http://%s", listen)
	if listen == "0.0.0.0:17777" || listen == ":17777" {
		accessURL = "http://127.0.0.1:17777"
	}

	shortCommit := commit
	if len(commit) > 7 {
		shortCommit = commit[:7]
	}

	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("  OmniGate %s (%s)\n", version, shortCommit)
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Printf("  访问地址:  %s\n", accessURL)
	fmt.Printf("  数据目录:  %s\n", appHome)
	fmt.Printf("  数据库:    %s\n", dbPath)
	fmt.Printf("  配置文件:  %s\n", cfgPath)
	fmt.Printf("  日志文件:  %s\n", logPath)
	fmt.Printf("  认证模式:  %s\n", auth.Mode())
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")

	// 仅在前台模式或子进程中显示启动完成消息
	if foreground {
		fmt.Println("\n✓ 服务已启动（前台模式，按 Ctrl+C 停止）")
		slog.Info("foreground mode: serving",
			"listen", listen,
			"db", dbPath,
			"config", cfgPath,
			"log", logPath,
			"admin_auth", auth.Mode(),
			"v1_auth", auth.V1Protected())
	} else {
		// 后台子进程静默启动
		slog.Info("background mode: serving", "pid", os.Getpid())
	}

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed", "err", err)
	}
	if err := st.Close(); err != nil {
		slog.Error("close store failed", "err", err)
	}
	slog.Info("bye")
}
