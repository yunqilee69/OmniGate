#!/usr/bin/env bash
# 本地打包并把当前构建装到本机全局 npm，然后后台启动。
# 全局前缀不固定：按当前 npm 解析（nvm / fnm / volta / 自定义 prefix 均可）。
# 默认后台 daemon，脚本退出后服务继续跑（~/.omnigate/）。
#
#   ./install-local.sh              构建前端 + 二进制，pack 后 npm i -g，再后台启动
#   ./install-local.sh --skip-web   复用已有 web 产物
#   ./install-local.sh --no-start   只安装，不启动
set -euo pipefail
cd "$(dirname "$0")"

ROOT="$(pwd)"
PKG_DIR="$ROOT/npm"
SKIP_WEB=0
NO_START=0
for arg in "$@"; do
  case "$arg" in
    --skip-web) SKIP_WEB=1 ;;
    --no-start) NO_START=1 ;;
    -h|--help)
      sed -n '2,9p' "$0"
      exit 0
      ;;
    *)
      echo "[FAIL] 未知参数: $arg"
      echo "  用法: $0 [--skip-web] [--no-start]"
      exit 1
      ;;
  esac
done

# ---------- 工具定位（兼容 nvm / 用户目录 Go） ----------
find_go() {
  if command -v go >/dev/null 2>&1; then command -v go; return; fi
  for c in "$HOME/.local/go/bin/go" /usr/local/go/bin/go /opt/go/bin/go; do
    [ -x "$c" ] && { echo "$c"; return; }
  done
  echo ""
}

find_npm() {
  if command -v npm >/dev/null 2>&1; then command -v npm; return; fi
  # nvm：取版本号最高的一份
  local latest=""
  for c in "$HOME/.nvm/versions/node"/*/bin/npm; do
    [ -x "$c" ] || continue
    latest="$c"
  done
  [ -n "$latest" ] && { echo "$latest"; return; }
  for c in "$HOME/.fnm/current/bin/npm" "$HOME/.volta/bin/npm"; do
    [ -x "$c" ] && { echo "$c"; return; }
  done
  echo ""
}

GO_BIN="$(find_go)"
[ -z "$GO_BIN" ] && { echo "[FAIL] 未找到 go，请先安装 Go 1.27+"; exit 1; }
NPM_BIN="$(find_npm)"
[ -z "$NPM_BIN" ] && { echo "[FAIL] 未找到 npm，请先安装 Node.js"; exit 1; }
NODE_BIN="$(command -v node 2>/dev/null || true)"
if [ -z "$NODE_BIN" ]; then
  NODE_BIN="$(dirname "$NPM_BIN")/node"
fi
[ -x "$NODE_BIN" ] || { echo "[FAIL] 未找到 node（与 npm 同目录）"; exit 1; }

# 当前 npm 的全局前缀：nvm 下随 Node 版本变；也可被 npm config set prefix 覆盖。
# 不用硬编码 ~/.nvm/... 或 /usr/local。
NPM_PREFIX="$("$NPM_BIN" prefix -g)"
NPM_ROOT="$("$NPM_BIN" root -g)"
NPM_BINDIR="$NPM_PREFIX/bin"
if [ ! -d "$NPM_PREFIX" ]; then
  echo "[FAIL] npm 全局前缀不存在: $NPM_PREFIX"
  exit 1
fi

echo "================ OmniGate 本地安装 ================"
echo "  go            : $GO_BIN ($("$GO_BIN" version | awk '{print $3}'))"
echo "  npm           : $NPM_BIN"
echo "  node          : $NODE_BIN"
echo "  全局前缀      : $NPM_PREFIX"
echo "  全局 node_modules : $NPM_ROOT"
echo "  全局 bin      : $NPM_BINDIR"
echo "===================================================="

# ---------- 版本：git describe，无 tag 时回落到 npm/package.json ----------
git_version() {
  if git -C "$ROOT" describe --tags --always --dirty 2>/dev/null; then
    return
  fi
  echo ""
}
raw_ver="$(git_version)"
raw_ver="${raw_ver#v}"
pkg_ver="$("$NODE_BIN" -p "require('$PKG_DIR/package.json').version")"
VERSION="${raw_ver:-$pkg_ver}"
[ -z "$VERSION" ] && VERSION="dev"
COMMIT="$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo none)"
DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

echo "[..] 版本 $VERSION  commit $COMMIT"

# ---------- 前端 ----------
if [ "$SKIP_WEB" = "1" ]; then
  if [ ! -f "$ROOT/internal/webui/dist/index.html" ]; then
    echo "[FAIL] --skip-web 但 internal/webui/dist/index.html 不存在，请先构建前端"
    exit 1
  fi
  echo "[SKIP] 复用已有 web 产物"
else
  if [ ! -d "$ROOT/web/node_modules" ]; then
    echo "[..] 安装前端依赖"
    (cd "$ROOT/web" && "$NPM_BIN" ci --no-audit --no-fund) \
      || (cd "$ROOT/web" && "$NPM_BIN" ci --no-audit --no-fund --registry=https://registry.npmmirror.com)
  fi
  echo "[..] 构建前端 → internal/webui/dist/"
  (cd "$ROOT/web" && "$NPM_BIN" run build)
fi

# ---------- 后端（内嵌前端，ldflags 与 goreleaser 对齐） ----------
BIN_NAME="omnigate"
if [ "$(uname -s)" = "Windows_NT" ] || [ "${OS:-}" = "Windows_NT" ]; then
  BIN_NAME="omnigate.exe"
fi
echo "[..] 编译后端 $BIN_NAME"
CGO_ENABLED=0 "$GO_BIN" build -trimpath \
  -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
  -o "$ROOT/$BIN_NAME" ./cmd/omnigate

# ---------- 准备 npm 包：vendor 放本地二进制，跳过 postinstall 下载 ----------
VENDOR_DIR="$PKG_DIR/vendor"
mkdir -p "$VENDOR_DIR"
rm -f "$VENDOR_DIR/omnigate" "$VENDOR_DIR/omnigate.exe"
cp "$ROOT/$BIN_NAME" "$VENDOR_DIR/$BIN_NAME"
chmod +x "$VENDOR_DIR/$BIN_NAME"

# pack 工作目录：npm pack 默认不带 vendor（files 白名单），先拷一份再改 files。
STAGE="$(mktemp -d "${TMPDIR:-/tmp}/omnigate-pack.XXXXXX")"
cleanup() { rm -rf "$STAGE"; }
trap cleanup EXIT

cp "$PKG_DIR/package.json" "$STAGE/package.json"
cp "$PKG_DIR/README.md" "$STAGE/README.md"
cp "$PKG_DIR/LICENSE" "$STAGE/LICENSE"
cp -R "$PKG_DIR/bin" "$STAGE/bin"
cp -R "$PKG_DIR/scripts" "$STAGE/scripts"
mkdir -p "$STAGE/vendor"
cp "$VENDOR_DIR/$BIN_NAME" "$STAGE/vendor/$BIN_NAME"

# 同步版本号到暂存 package.json，并允许 vendor 进 tarball。
"$NODE_BIN" -e "
const fs = require('fs');
const p = '$STAGE/package.json';
const j = JSON.parse(fs.readFileSync(p, 'utf8'));
j.version = process.argv[1];
if (!j.files.includes('vendor/')) j.files.push('vendor/');
fs.writeFileSync(p, JSON.stringify(j, null, 2) + '\n');
" "$VERSION"

echo "[..] npm pack"
TARBALL="$("$NPM_BIN" pack --pack-destination "$STAGE" --silent --ignore-scripts "$STAGE")"
# npm pack 可能打印相对名或绝对路径
if [ -f "$STAGE/$TARBALL" ]; then
  TARBALL_PATH="$STAGE/$TARBALL"
elif [ -f "$TARBALL" ]; then
  TARBALL_PATH="$TARBALL"
else
  TARBALL_PATH="$(ls -1 "$STAGE"/*.tgz | head -n 1)"
fi
[ -f "$TARBALL_PATH" ] || { echo "[FAIL] npm pack 未产出 tarball"; exit 1; }
echo "[OK] 包 $TARBALL_PATH"

echo "[..] npm install -g --prefix $NPM_PREFIX"
OMNIGATE_SKIP_POSTINSTALL=1 "$NPM_BIN" install -g --prefix "$NPM_PREFIX" --ignore-scripts --no-fund --no-audit "$TARBALL_PATH"

INSTALLED_PKG="$NPM_ROOT/@cloudomni/omnigate"
INSTALLED_VENDOR="$INSTALLED_PKG/vendor/$BIN_NAME"
if [ ! -x "$INSTALLED_VENDOR" ]; then
  echo "[..] vendor 二进制未随 tarball 落地，补拷到 $INSTALLED_VENDOR"
  mkdir -p "$INSTALLED_PKG/vendor"
  cp "$VENDOR_DIR/$BIN_NAME" "$INSTALLED_VENDOR"
  chmod +x "$INSTALLED_VENDOR"
fi

SHIM="$NPM_BINDIR/omnigate"
if [ ! -e "$SHIM" ] && [ ! -e "$SHIM.cmd" ]; then
  echo "[WARN] 未找到全局 shim $SHIM"
  echo "      把 $NPM_BINDIR 加进 PATH 后再试"
fi

OG="$SHIM"
if [ ! -x "$OG" ]; then
  OG="$INSTALLED_VENDOR"
fi

echo
echo "================ 安装完成 ================="
echo "  包目录        : $INSTALLED_PKG"
echo "  二进制        : $INSTALLED_VENDOR"
echo "  命令          : $SHIM"
echo "  校验          : omnigate --version"
"$OG" --version
echo
if ! command -v omnigate >/dev/null 2>&1; then
  echo "[HINT] 当前 PATH 找不到 omnigate，把下面目录加入 PATH："
  echo "       $NPM_BINDIR"
fi

if [ "$NO_START" = "1" ]; then
  echo "[SKIP] --no-start：不启动服务"
  echo "================================================"
  exit 0
fi

# 用户实例默认跑在 ~/.omnigate/。已在跑则先停，让新二进制生效。
# `status`/`stop` 在未运行时非 0，if 包裹避免 set -e 中断。
if "$OG" status >/dev/null 2>&1; then
  echo "[..] 已有实例在运行，先停止再启动新版本"
  "$OG" stop
fi

echo "[..] 后台启动"
"$OG" start
echo "================================================"
