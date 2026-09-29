#!/usr/bin/env bash
#
# 把面试服务当作"本机常驻服务"来管理: 起在后台、写日志、记 PID。
#
# 为什么需要它:
#   直接在终端里 `./bin/interviewd -serve :8101` 是前台运行 —— 关掉那个
#   终端窗口、或者不小心 Ctrl+C, 服务就没了, 而浏览器里的表现是
#   "页面突然打不开"。面试这种要连着开几十分钟的东西, 不该绑在一个
#   会随手被关掉的终端上。
#
# 用法:
#   scripts/server.sh start [端口]     # 默认 8101, 后台启动
#   scripts/server.sh stop             # 停止
#   scripts/server.sh restart [端口]   # 停掉再起
#   scripts/server.sh status           # 看状态(含"给的是新前端还是旧前端")
#   scripts/server.sh logs             # 跟踪日志(Ctrl+C 只退出 tail, 不停服务)
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PORT="${2:-8101}"
PID_FILE="$ROOT/data/interviewd.pid"
LOG_FILE="$ROOT/data/interviewd.log"
BIN="$ROOT/bin/interviewd"

mkdir -p "$ROOT/data"

log() { printf '\033[36m[server]\033[0m %s\n' "$*"; }
warn() { printf '\033[33m[server]\033[0m %s\n' "$*"; }
die() { printf '\033[31m[server]\033[0m %s\n' "$*" >&2; exit 1; }

# running_pid 返回 pid 文件里那个进程的 PID(若它确实还活着)。
running_pid() {
  [ -f "$PID_FILE" ] || return 1
  local pid
  pid="$(cat "$PID_FILE" 2>/dev/null || true)"
  [ -n "$pid" ] || return 1
  kill -0 "$pid" 2>/dev/null || return 1
  echo "$pid"
}

# port_pids 返回占用端口的进程(可能有多个, 取全部)。
port_pids() {
  lsof -nP -tiTCP:"$1" -sTCP:LISTEN 2>/dev/null || true
}

# frontend_kind 判断服务端给的是新前端还是旧前端。
#
# 这是本项目最容易误判的一件事: 前端被 go:embed 编译进二进制,
# 所以"代码是新的"和"浏览器看到的是新的"是两回事。这里直接问服务端。
frontend_kind() {
  local port="$1" body
  body="$(curl -s --max-time 4 "http://127.0.0.1:$port/" 2>/dev/null || true)"
  if [ -z "$body" ]; then
    echo "无法连接"
  elif printf '%s' "$body" | grep -q '/js/app.js'; then
    echo "新版(模块化前端)"
  else
    echo "旧版(单文件 app.js) —— 需要重新构建并重启"
  fi
}

stop_port() {
  local port="$1" pids
  pids="$(port_pids "$port")"
  [ -z "$pids" ] && return 0
  # shellcheck disable=SC2086
  kill $pids 2>/dev/null || true
  for _ in 1 2 3; do
    sleep 1
    pids="$(port_pids "$port")"
    [ -z "$pids" ] && return 0
  done
  # shellcheck disable=SC2086
  kill -9 $pids 2>/dev/null || true
  sleep 1
  pids="$(port_pids "$port")"
  if [ -n "$pids" ]; then
    die "端口 $port 仍被占用(进程 $pids)。请手动执行: kill -9 $pids (必要时加 sudo)"
  fi
}

cmd_start() {
  local port="$1" pid
  if pid="$(running_pid)"; then
    log "服务已在运行 (pid $pid)"
    cmd_status "$port"
    return 0
  fi

  [ -x "$BIN" ] || die "找不到可执行文件 $BIN, 先执行: make build"

  if [ -n "$(port_pids "$port")" ]; then
    warn "端口 $port 上已有别的进程, 先停掉它们"
    stop_port "$port"
  fi

  log "后台启动: http://localhost:$port/"
  # nohup + 重定向, 让它脱离当前终端: 关掉终端窗口服务也能继续跑。
  # setsid 在 macOS 上不存在, 因此用 nohup + disown 这个组合。
  RECORDING_DIR="${RECORDING_DIR:-$ROOT/data/recordings}" \
    nohup "$BIN" -serve ":$port" >"$LOG_FILE" 2>&1 &
  pid=$!
  disown "$pid" 2>/dev/null || true
  echo "$pid" >"$PID_FILE"

  # 等它就绪, 而不是让用户"刷新看看有没有好"。
  for _ in $(seq 1 20); do
    sleep 0.5
    if [ -n "$(port_pids "$port")" ]; then
      log "已启动 (pid $pid), 日志: $LOG_FILE"
      cmd_status "$port"
      return 0
    fi
    kill -0 "$pid" 2>/dev/null || break
  done
  warn "启动似乎失败了, 最后 15 行日志:"
  tail -n 15 "$LOG_FILE" 2>/dev/null || true
  rm -f "$PID_FILE"
  exit 1
}

cmd_stop() {
  local pid
  if pid="$(running_pid)"; then
    log "停止 pid $pid"
    kill "$pid" 2>/dev/null || true
    for _ in 1 2 3 4 5; do
      sleep 1
      kill -0 "$pid" 2>/dev/null || break
    done
    kill -0 "$pid" 2>/dev/null && kill -9 "$pid" 2>/dev/null || true
  fi
  rm -f "$PID_FILE"
  # 顺手清掉"不是这次启动的"残留进程(历史遗留的旧版服务)。
  local others
  others="$(lsof -a -c interview -iTCP -sTCP:LISTEN -t 2>/dev/null | sort -u || true)"
  if [ -n "$others" ]; then
    warn "还有残留的 interviewd 进程: $(echo "$others" | tr '\n' ' ')"
    # shellcheck disable=SC2086
    kill $others 2>/dev/null || true
    sleep 1
    # shellcheck disable=SC2086
    kill -9 $others 2>/dev/null || true
  fi
  log "已停止"
}

cmd_status() {
  local port="$1" pid
  printf '\n  端口        : %s\n' "$port"
  if pid="$(running_pid)"; then
    printf '  状态        : 运行中 (pid %s)\n' "$pid"
  else
    printf '  状态        : 未运行(pid 文件缺失或进程已退出)\n'
  fi
  local pids
  pids="$(port_pids "$port" | tr '\n' ' ')"
  printf '  监听进程    : %s\n' "${pids:-无}"
  printf '  前端版本    : %s\n' "$(frontend_kind "$port")"
  printf '  健康检查    : %s\n' "$(curl -s --max-time 3 "http://127.0.0.1:$port/healthz" 2>/dev/null || echo 不通)"
  printf '  录制目录    : %s\n' "${RECORDING_DIR:-$ROOT/data/recordings}"
  printf '  日志        : %s\n\n' "$LOG_FILE"
}

case "${1:-status}" in
  start) cmd_start "$PORT" ;;
  stop) cmd_stop ;;
  restart)
    cmd_stop
    cmd_start "$PORT"
    ;;
  status) cmd_status "$PORT" ;;
  logs) tail -n 50 -f "$LOG_FILE" ;;
  *)
    cat <<'USAGE'
用法: scripts/server.sh <命令> [端口]
  start [端口]    后台启动服务(默认 8101)
  stop            停止服务, 并清理残留的旧版进程
  restart [端口]  停掉再起
  status [端口]   查看状态(含"给的是新前端还是旧前端")
  logs            跟踪日志
USAGE
    exit 1
    ;;
esac
