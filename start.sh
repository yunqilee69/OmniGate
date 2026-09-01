#!/usr/bin/env bash
# OmniGate 一键启动脚本
#   ./start.sh          开发模式：后端 127.0.0.1:17777 + 前端 vite 热更新 17778
#   ./start.sh --prod   生产模式：仅后端（内嵌前端产物），浏览器访问 17778
set -euo pipefail
cd "$(dirname "$0")"

ROOT="$(pwd)"
BIN="$ROOT/omnigate"
LOG_DIR="$ROOT/logs"
DATA_DIR="$ROOT/data"
mkdir -p "$LOG_DIR" "$DATA_DIR"

BACKEND_PORT=17777
FRONTEND_PORT=17778

# ---------- 工具定位（兼容 nvm 安装的 node 与用户目录安装的 go） ----------
find_go() {
  if command -v go >/dev/null 2>&1; then command -v go; return; fi
  for c in "$HOME/.local/go/bin/go" /usr/local/go/bin/go; do
    [ -x "$c" ] && { echo "$c"; return; }
  done
  echo ""
}
find_npm() {
  if command -v npm >/dev/null 2>&1; then command -v npm; return; fi
  for c in "$HOME/.nvm/versions/node"/*/bin/npm; do
    [ -x "$c" ] && { echo "$c"; return; }
  done
  echo ""
}

port_busy() {
  if command -v ss >/dev/null 2>&1; then
    ss -tln 2>/dev/null | grep -q ":$1 "
  else
    lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1
  fi
}

PIDS=()
CLEANED=0
# kill_tree 递归终止 pid 及其全部后代（macOS 无 setsid 且非组长时，组杀不可用，按进程树收割）
kill_tree() {
  local pid=$1
  echo "[DEBUG kill_tree] Processing PID $pid" >&2
  local kids
  kids=$(pgrep -P "$pid" 2>/dev/null || true)
  if [ -n "$kids" ]; then
    echo "[DEBUG kill_tree] Found children: $kids" >&2
  fi
  # 先递归杀子进程
  for k in $kids; do kill_tree "$k"; done
  # 再杀父进程：先 TERM，等不到就 KILL
  if kill -0 "$pid" 2>/dev/null; then
    echo "[DEBUG kill_tree] Sending TERM to $pid" >&2
    kill -TERM "$pid" 2>/dev/null || true
    for i in {1..5}; do
      if ! kill -0 "$pid" 2>/dev/null; then
        echo "[DEBUG kill_tree] PID $pid died" >&2
        return
      fi
      sleep 0.2
    done
    echo "[DEBUG kill_tree] PID $pid still alive, sending KILL" >&2
    kill -KILL "$pid" 2>/dev/null || true
  else
    echo "[DEBUG kill_tree] PID $pid already dead" >&2
  fi
}

cleanup() {
  [ "$CLEANED" -eq 1 ] && return
  CLEANED=1
  echo "" >&2
  echo "正在停止所有进程..." >&2
  
  # 方案1：杀死整个进程组（包括所有子进程和孙进程）
  # 脚本作为进程组长，-$$ 表示整个进程组
  if kill -0 -$$ 2>/dev/null; then
    kill -TERM -$$ 2>/dev/null || true
  fi
  
  # 等待进程优雅退出
  sleep 2
  
  # 强制杀死进程组中仍在运行的进程
  if kill -0 -$$ 2>/dev/null; then
    kill -KILL -$$ 2>/dev/null || true
  fi
  
  # 方案2：逐个杀死记录的直接子进程（作为补充）
  for pid in "${PIDS[@]}"; do
    if kill -0 "$pid" 2>/dev/null; then
      kill -KILL "$pid" 2>/dev/null || true
    fi
  done
}
trap cleanup EXIT INT TERM

# run_bg 后台启动：所有子进程留在同一进程组，这样 kill -TERM -$$ 可以一次性停止所有进程
run_bg() {
  "$@" &
  PIDS+=($!)
}

wait_healthy() {
  local port=$1 name=$2 i=0
  while [ $i -lt 30 ]; do
    if curl -sf -o /dev/null "http://127.0.0.1:$port/"; then
      echo "[OK] $name 就绪 (port $port)"; return 0
    fi
    sleep 0.5; i=$((i + 1))
  done
  echo "[FAIL] $name 未在 15s 内就绪，查看 $LOG_DIR/ 下日志"; exit 1
}

build_backend() {
  GO_BIN="$(find_go)"
  [ -z "$GO_BIN" ] && { echo "[FAIL] 未找到 go，请先安装"; exit 1; }
  echo "[..] 编译后端"
  "$GO_BIN" build -o "$BIN" ./cmd/omnigate
}

if port_busy "$BACKEND_PORT"; then
  echo "[SKIP] 端口 $BACKEND_PORT 已被占用（后端可能已在运行）"
else
  build_backend
  # 开发模式:db / config 用仓库内本地路径(便于 rm -rf data/ 重置),
  # 日志走 stdout 由 shell 重定向到 backend.log(避免双重写文件)。
  echo "[..] 启动后端 (db: $DATA_DIR/omnigate.db)"
  run_bg "$BIN" --db "$DATA_DIR/omnigate.db" --config "$ROOT/config.yaml" --log stdout --foreground \
    > "$LOG_DIR/backend.log" 2>&1
  wait_healthy "$BACKEND_PORT" "后端"
  # 启用 debug 模式
  sleep 1  # 等待数据库初始化
  if [ -f "$DATA_DIR/omnigate.db" ]; then
    sqlite3 "$DATA_DIR/omnigate.db" "INSERT OR REPLACE INTO app_config (key, value) VALUES ('debug.stream_log', 'true')" 2>/dev/null || true
    echo "[OK] Debug 模式已启用"
  fi
fi

# ---------- 前端 ----------
if [ "${1:-}" = "--prod" ]; then
  echo ""
  echo "================ OmniGate（生产模式）================"
  echo "  管理界面 + API : http://127.0.0.1:$BACKEND_PORT"
  echo "  代理入口       : http://127.0.0.1:$BACKEND_PORT/v1/chat/completions"
  echo "  后端日志       : $LOG_DIR/backend.log"
  echo "========================================================"
  echo "Ctrl+C 停止"
  wait "${PIDS[@]:-}" 2>/dev/null || wait
  exit 0
fi

if port_busy "$FRONTEND_PORT"; then
  echo "[SKIP] 端口 $FRONTEND_PORT 已被占用（前端可能已在运行）"
else
  NPM_BIN="$(find_npm)"
  [ -z "$NPM_BIN" ] && { echo "[FAIL] 未找到 npm，请先安装 Node.js"; exit 1; }
  if [ ! -d web/node_modules ]; then
    echo "[..] 首次运行：安装前端依赖"
    (cd web && "$NPM_BIN" install --no-audit --no-fund) \
      || (cd web && "$NPM_BIN" install --no-audit --no-fund --registry=https://registry.npmmirror.com)
  fi
  echo "[..] 启动前端 vite (热更新)"
  run_bg bash -c "cd '$ROOT/web' && '$NPM_BIN' run dev" > "$LOG_DIR/frontend.log" 2>&1
  wait_healthy "$FRONTEND_PORT" "前端"
fi

echo ""
echo "================ OmniGate（开发模式）================"
echo "  前端(热更新)    : http://localhost:$FRONTEND_PORT"
echo "  后端 API/代理   : http://127.0.0.1:$BACKEND_PORT"
echo "  日志目录        : $LOG_DIR/"
echo "========================================================"
echo "Ctrl+C 一键停止全部"

# 不使用 wait（会导致 trap 中无法访问子进程）
# 改用手动循环检查，这样在 SIGINT 时仍保持对子进程的控制
while true; do
  # 检查是否有任何子进程还活着
  any_alive=false
  for pid in "${PIDS[@]}"; do
    if kill -0 "$pid" 2>/dev/null; then
      any_alive=true
      break
    fi
  done
  
  if ! $any_alive && [ ${#PIDS[@]} -gt 0 ]; then
    break
  fi
  
  sleep 1 & wait $! || break
done
