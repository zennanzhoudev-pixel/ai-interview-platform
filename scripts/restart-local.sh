#!/usr/bin/env bash
#
# 把本机正在跑的旧版面试服务换成"当前代码构建出来的新版"。
#
# 为什么需要这么一个脚本:
#   前端是用 go:embed 编译进二进制的(部署只有一个文件、不依赖 CDN,
#   面试页面不会因为外网挂了白屏)。代价是 —— 改了 web/ 下面的任何文件,
#   都必须重新构建并重启进程, 浏览器才会看到新界面。
#   忘了这一步的表现就是"代码明明改了, 打开还是旧系统", 而这件事
#   已经真实发生过一次, 所以这里把它做成一条命令。
#
# 用法:
#   scripts/restart-local.sh                 # 换掉 8101 上的进程并启动新版
#   scripts/restart-local.sh 8080            # 指定端口
#   scripts/restart-local.sh 8101 --clean-legacy   # 顺手清掉 8111/8112/8113 的验证残留
#
set -euo pipefail

PORT="${1:-8101}"
CLEAN_LEGACY="${2:-}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

log() { printf '\033[36m[restart]\033[0m %s\n' "$*"; }
warn() { printf '\033[33m[restart]\033[0m %s\n' "$*"; }
die() { printf '\033[31m[restart]\033[0m %s\n' "$*" >&2; exit 1; }

# stop_listener 停掉占用指定端口的进程。
# 先 TERM, 给它 3 秒优雅退出(服务自己会等面试结束), 再 KILL。
stop_listener() {
  local port="$1" pids
  pids="$(lsof -nP -tiTCP:"$port" -sTCP:LISTEN 2>/dev/null || true)"
  if [ -z "$pids" ]; then
    return 0
  fi
  log "端口 $port 上还有进程: $(echo "$pids" | tr '\n' ' ')"
  # shellcheck disable=SC2086
  kill $pids 2>/dev/null || true
  for _ in 1 2 3; do
    sleep 1
    pids="$(lsof -nP -tiTCP:"$port" -sTCP:LISTEN 2>/dev/null || true)"
    [ -z "$pids" ] && return 0
  done
  # shellcheck disable=SC2086
  kill -9 $pids 2>/dev/null || true
  sleep 1
  pids="$(lsof -nP -tiTCP:"$port" -sTCP:LISTEN 2>/dev/null || true)"
  if [ -n "$pids" ]; then
    die "端口 $port 仍被占用(进程 $pids)。这个进程不在当前用户权限内, 请手动执行:
        kill -9 $(echo "$pids" | tr '\n' ' ')
     如果提示 operation not permitted, 再加上 sudo:
        sudo kill -9 $(echo "$pids" | tr '\n' ' ')"
  fi
}

if [ "$CLEAN_LEGACY" = "--clean-legacy" ]; then
  for p in 8111 8112 8113; do
    stop_listener "$p"
  done
  log "验证残留已清理(8111/8112/8113)"
fi

stop_listener "$PORT"

log "重新构建(前端在二进制里, 必须重建)"
if ! command -v go >/dev/null 2>&1; then
  die "找不到 go, 请先安装 Go 1.22 以上版本"
fi
go build -trimpath -ldflags="-s -w" -o bin/interviewd ./cmd/interviewd

log "构建完成: $(ls -la bin/interviewd | awk '{print $5" bytes  "$6" "$7" "$8}')"
log "启动 http://localhost:$PORT/ (简历与录像默认落在 data/recordings)"
log "打开后请硬刷新浏览器: Mac 用 Cmd+Shift+R, Windows 用 Ctrl+F5"

RECORDING_DIR="${RECORDING_DIR:-data/recordings}" \
  exec ./bin/interviewd -serve ":$PORT"
