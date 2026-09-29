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
#   scripts/restart-local.sh 8101 --clean-all      # 先把所有残留的 interviewd 进程都停掉
#
set -euo pipefail

PORT="${1:-8101}"
CLEAN_ALL="${2:-}"
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

# clean_stray 停掉"所有正在监听端口的 interviewd 进程"。
#
# 不写死端口号: 端口是会变的(验证时随手起在 8111/8112/8113/8120 都可能),
# 而写死清单的结果就是"清了三遍, 第四天又被另一个残留进程绕晕"。
# 按进程名 + 监听态筛选, 目标永远是当前真实存在的那些。
#
# 只杀"正在监听"的监听态进程, 因此不会误伤正在跑的模拟面试进程
# (interviewd 不带 -serve 时不监听任何端口)。
clean_stray() {
  local self=$$ keep_port="$1" pids pid alive
  # 注意 lsof 的过滤器默认是"或"关系: 必须加 -a 才是"同时满足"。
  # 少写一个 -a 的后果不是查不准, 而是会把 WeChat / ControlCenter 这些
  # 完全无关的监听进程一起列进来 —— 而下面的循环会去杀它们。
  pids="$(lsof -a -c interview -iTCP -sTCP:LISTEN -t 2>/dev/null | sort -u || true)"
  [ -z "$pids" ] && { log "没有发现残留的 interviewd 进程"; return 0; }

  local victims=""
  for pid in $pids; do
    [ "$pid" = "$self" ] && continue
    # 二次确认: 可执行文件必须真的是 interviewd。
    # 按进程名匹配本身就够用, 但"名字以 interview 开头的别的程序"是可能存在的,
    # 而这条命令会杀进程, 所以宁可多查一次。
    if ! lsof -a -p "$pid" -d txt 2>/dev/null | grep -q '/interviewd'; then
      continue
    fi
    # 目标端口的进程交给 stop_listener 处理, 这里跳过避免重复报错。
    if lsof -nP -a -p "$pid" -iTCP:"$keep_port" -sTCP:LISTEN >/dev/null 2>&1; then
      continue
    fi
    victims="$victims $pid"
  done
  [ -z "${victims#" "}" ] && { log "没有其它端口的残留进程需要清理"; return 0; }

  log "清理残留进程:$victims"
  # shellcheck disable=SC2086
  kill $victims 2>/dev/null || true
  sleep 1
  # shellcheck disable=SC2086
  kill -9 $victims 2>/dev/null || true
  sleep 1
  alive=""
  for pid in $victims; do
    kill -0 "$pid" 2>/dev/null && alive="$alive $pid"
  done
  if [ -n "$alive" ]; then
    warn "以下进程停不掉(不在当前用户权限内), 请手动执行: kill -9$alive"
    warn "若提示 operation not permitted, 用: sudo kill -9$alive"
  else
    log "残留进程已清理"
  fi
}

stop_listener "$PORT"

if [ "$CLEAN_ALL" = "--clean-all" ]; then
  clean_stray "$PORT"
fi

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
