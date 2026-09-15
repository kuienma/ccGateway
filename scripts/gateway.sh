#!/usr/bin/env bash

set -euo pipefail

readonly SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
readonly PROJECT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
readonly ENV_FILE="${GATEWAY_ENV_FILE:-${PROJECT_DIR}/.env}"
readonly RUNTIME_DIR="${GATEWAY_RUNTIME_DIR:-${PROJECT_DIR}/.runtime}"
readonly PID_FILE="${RUNTIME_DIR}/gateway.pid"
readonly LOG_FILE="${RUNTIME_DIR}/gateway.log"
readonly BINARY_FILE="${RUNTIME_DIR}/gateway"
readonly DEFAULT_STOP_WAIT_SECONDS=20
readonly START_CHECK_ATTEMPTS=20
readonly START_CHECK_INTERVAL_SECONDS=0.25
readonly DEFAULT_LISTEN_ADDR=:8080

usage() {
  cat <<'USAGE'
用法: ./scripts/gateway.sh <command>

command:
  start    后台启动；加载 .env、构建二进制并记录 PID
  stop     发送 SIGTERM 并等待优雅退出
  restart  停止后重新启动
  status   查看运行状态
  reload   发送 SIGHUP，原子热更新模型配置
  logs     持续查看运行日志
  run      前台运行，适合本地调试
USAGE
}

load_env() {
  if [[ ! -f "${ENV_FILE}" ]]; then
    echo "找不到环境配置: ${ENV_FILE}" >&2
    echo "请参考 ${PROJECT_DIR}/.env.example 创建配置。" >&2
    return 1
  fi

  set -a
  # .env 是本机受信任的 shell 配置，加载后导出其中变量给网关进程。
  source "${ENV_FILE}"
  set +a
}

read_pid() {
  if [[ ! -f "${PID_FILE}" ]]; then
    return 1
  fi

  local pid
  pid="$(<"${PID_FILE}")"
  if [[ ! "${pid}" =~ ^[0-9]+$ ]]; then
    return 1
  fi
  printf '%s' "${pid}"
}

is_running() {
  local pid
  pid="$(read_pid)" || return 1
  kill -0 "${pid}" 2>/dev/null && is_gateway_process "${pid}"
}

is_gateway_process() {
  local pid="$1"
  local command
  command="$(ps -p "${pid}" -o command= 2>/dev/null || true)"
  [[ "${command}" == *"${BINARY_FILE}"* ]]
}

owns_listen_port() {
  local pid="$1"
  local port="$2"
  lsof -nP -a -p "${pid}" -iTCP:"${port}" -sTCP:LISTEN >/dev/null 2>&1
}

build_binary() {
  mkdir -p "${RUNTIME_DIR}"
  echo "正在构建网关..."
  (
    cd "${PROJECT_DIR}"
    CGO_ENABLED=0 go build -trimpath -o "${BINARY_FILE}" ./cmd/gateway
  )
}

health_url() {
  local address="${LISTEN_ADDR:-${DEFAULT_LISTEN_ADDR}}"
  local port="${address##*:}"
  local host="${address%:*}"
  case "${host}" in
    ""|0.0.0.0|"[::]"|::)
      host=127.0.0.1
      ;;
  esac
  printf "http://%s:%s/healthz" "${host}" "${port}"
}

start_gateway() {
  if is_running; then
    echo "网关已经运行，PID: $(read_pid)"
    return 0
  fi

  rm -f "${PID_FILE}"
  load_env
  build_binary
  mkdir -p "${RUNTIME_DIR}"

  echo "正在启动网关..."
  nohup "${BINARY_FILE}" >>"${LOG_FILE}" 2>&1 &
  local pid=$!
  printf '%s\n' "${pid}" >"${PID_FILE}"

  local attempt
  for ((attempt = 1; attempt <= START_CHECK_ATTEMPTS; attempt++)); do
    if ! kill -0 "${pid}" 2>/dev/null; then
      echo "网关启动失败，最近日志：" >&2
      tail -n 20 "${LOG_FILE}" >&2 || true
      rm -f "${PID_FILE}"
      return 1
    fi
    if command -v curl >/dev/null 2>&1 && curl --fail --silent --max-time 1 "$(health_url)" >/dev/null; then
      local port="${LISTEN_ADDR:-${DEFAULT_LISTEN_ADDR}}"
      port="${port##*:}"
      if command -v lsof >/dev/null 2>&1; then
        if ! owns_listen_port "${pid}" "${port}"; then
          sleep "${START_CHECK_INTERVAL_SECONDS}"
          continue
        fi
      else
        sleep "${START_CHECK_INTERVAL_SECONDS}"
        if ! kill -0 "${pid}" 2>/dev/null || ! is_gateway_process "${pid}"; then
          continue
        fi
      fi
      if ! kill -0 "${pid}" 2>/dev/null || ! is_gateway_process "${pid}"; then
        continue
      fi
      echo "网关启动成功，PID: ${pid}"
      echo "健康检查: $(health_url)"
      echo "日志文件: ${LOG_FILE}"
      return 0
    fi
    sleep "${START_CHECK_INTERVAL_SECONDS}"
  done

  if ! command -v curl >/dev/null 2>&1; then
    echo "网关启动成功，PID: ${pid}（系统未安装 curl，已跳过 HTTP 健康检查）"
    echo "日志文件: ${LOG_FILE}"
    return 0
  fi

  echo "网关进程已启动，但健康检查未在限定时间内通过: $(health_url)" >&2
  tail -n 20 "${LOG_FILE}" >&2 || true
  kill -TERM "${pid}" 2>/dev/null || true
  wait "${pid}" 2>/dev/null || true
  rm -f "${PID_FILE}"
  return 1
}

stop_gateway() {
  local pid
  if ! pid="$(read_pid)"; then
    echo "网关未运行。"
    return 0
  fi
  if ! kill -0 "${pid}" 2>/dev/null; then
    rm -f "${PID_FILE}"
    echo "网关未运行，已清理失效 PID 文件。"
    return 0
  fi
  if ! is_gateway_process "${pid}"; then
    echo "PID ${pid} 不是当前网关进程，拒绝停止。" >&2
    return 1
  fi

  local wait_seconds="${STOP_WAIT_SECONDS:-${DEFAULT_STOP_WAIT_SECONDS}}"
  if [[ ! "${wait_seconds}" =~ ^[1-9][0-9]*$ ]]; then
    echo "STOP_WAIT_SECONDS 必须是正整数。" >&2
    return 1
  fi

  echo "正在停止网关，PID: ${pid}"
  kill -TERM "${pid}"

  local waited
  for ((waited = 0; waited < wait_seconds * 4; waited++)); do
    if ! kill -0 "${pid}" 2>/dev/null; then
      rm -f "${PID_FILE}"
      echo "网关已停止。"
      return 0
    fi
    sleep 0.25
  done

  echo "等待 ${wait_seconds} 秒后进程仍未退出，请检查日志: ${LOG_FILE}" >&2
  return 1
}

status_gateway() {
  local pid
  if pid="$(read_pid)" && kill -0 "${pid}" 2>/dev/null && is_gateway_process "${pid}"; then
    echo "网关正在运行，PID: ${pid}"
    echo "日志文件: ${LOG_FILE}"
    return 0
  fi

  echo "网关未运行。"
  return 1
}

reload_gateway() {
  local pid
  if ! pid="$(read_pid)" || ! kill -0 "${pid}" 2>/dev/null || ! is_gateway_process "${pid}"; then
    echo "网关未运行，无法重新加载模型配置。" >&2
    return 1
  fi

  kill -HUP "${pid}"
  echo "已发送 SIGHUP，模型配置将在校验通过后生效。"
  echo "可通过日志确认结果: ${LOG_FILE}"
}

logs_gateway() {
  mkdir -p "${RUNTIME_DIR}"
  touch "${LOG_FILE}"
  tail -f "${LOG_FILE}"
}

run_foreground() {
  load_env
  build_binary
  exec "${BINARY_FILE}"
}

main() {
  local command="${1:-}"
  case "${command}" in
    start)
      start_gateway
      ;;
    stop)
      stop_gateway
      ;;
    restart)
      stop_gateway
      start_gateway
      ;;
    status)
      status_gateway
      ;;
    reload)
      reload_gateway
      ;;
    logs)
      logs_gateway
      ;;
    run)
      run_foreground
      ;;
    *)
      usage
      return 1
      ;;
  esac
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
