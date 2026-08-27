#!/usr/bin/env bash
# OmniGate 一键启动脚本
#   ./start.sh          开发模式：后端 127.0.0.1:17778 + 前端 vite 热更新 17777
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

port_busy() { ss -tln 2>/dev/null | grep -q ":$1 "; }

PIDS=()
cleanup() {
  for pid in "${PIDS[@]:-}"; do
    [ -n "$pid" ] && kill -- "-$pid" 2>/dev/null || true
  done
}
trap cleanup EXIT INT TERM

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

# ---------- 后端 ----------
build_backend() {
  GO_BIN="$(find_go)"
  [ -z "$GO_BIN" ] && { echo "[FAIL] 未找到 go，请先安装"; exit 1; }
  echo "[..] 编译后端"
  "$GO_BIN" build -o "$BIN" ./cmd/omnigate
}

if port_busy "$BACKEND_PORT"; then
  echo "[SKIP] 端口 $BACKEND_PORT 已被占用（后端可能已在运行）"
else
  if [ ! -x "$BIN" ] || [ -n "$(find cmd internal -name '*.go' -newer "$BIN" 2>/dev/null | head -1)" ]; then
    build_backend
  fi
  # 开发模式:db / config 用仓库内本地路径(便于 rm -rf data/ 重置),
  # 日志走 stdout 由 shell 重定向到 backend.log(避免双重写文件)。
  echo "[..] 启动后端 (db: $DATA_DIR/omnigate.db)"
  setsid "$BIN" --db "$DATA_DIR/omnigate.db" --config "$ROOT/config.yaml" --log stdout \
    > "$LOG_DIR/backend.log" 2>&1 &
  PIDS+=($!)
  wait_healthy "$BACKEND_PORT" "后端"
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
  setsid bash -c "cd '$ROOT/web' && '$NPM_BIN' run dev" > "$LOG_DIR/frontend.log" 2>&1 &
  PIDS+=($!)
  wait_healthy "$FRONTEND_PORT" "前端"
fi

echo ""
echo "================ OmniGate（开发模式）================"
echo "  前端(热更新)    : http://localhost:$FRONTEND_PORT"
echo "  后端 API/代理   : http://127.0.0.1:$BACKEND_PORT"
echo "  日志目录        : $LOG_DIR/"
echo "========================================================"
echo "Ctrl+C 一键停止全部"
wait
