#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"
TEST_RUNTIME_DIR="$(mktemp -d)"
export GATEWAY_RUNTIME_DIR="${TEST_RUNTIME_DIR}"

cleanup() {
  if [[ -n "${observed_pid:-}" ]] && builtin kill -0 "${observed_pid}" 2>/dev/null; then
    builtin kill -TERM "${observed_pid}" 2>/dev/null || true
  fi
  rm -rf "${TEST_RUNTIME_DIR}"
}
trap cleanup EXIT

source "${PROJECT_DIR}/scripts/gateway.sh"

lsof() {
  local pid=""
  local expect_pid=false
  local argument
  for argument in "$@"; do
    if [[ "${expect_pid}" == true ]]; then
      pid="${argument}"
      break
    fi
    if [[ "${argument}" == "-p" ]]; then
      expect_pid=true
    fi
  done
  [[ "${pid}" == "123" ]]
}

if ! owns_listen_port 123 38080; then
  echo "预期 PID 123 被识别为端口所有者" >&2
  exit 1
fi
if owns_listen_port 456 38080; then
  echo "不应把旧进程健康响应归属给 PID 456" >&2
  exit 1
fi

build_binary() {
  mkdir -p "${RUNTIME_DIR}"
  cat >"${BINARY_FILE}" <<'FAKE_GATEWAY'
#!/usr/bin/env bash
trap 'exit 0' TERM
while true; do
  /bin/sleep 1
done
FAKE_GATEWAY
  chmod +x "${BINARY_FILE}"
}

load_env() {
  LISTEN_ADDR=:38080
  export LISTEN_ADDR
}

curl() {
  return 0
}

owns_listen_port() {
  observed_pid="$1"
  return 1
}

is_gateway_process() {
  builtin kill -0 "$1" 2>/dev/null
}

sleep() {
  /bin/sleep 0.01
}

tail() {
  return 0
}

if start_gateway; then
  echo "旧进程健康响应不应让新进程启动成功" >&2
  exit 1
fi
if [[ -f "${PID_FILE}" ]]; then
  echo "启动失败后应清理 PID 文件" >&2
  exit 1
fi
for _ in {1..50}; do
  if ! builtin kill -0 "${observed_pid}" 2>/dev/null; then
    break
  fi
  /bin/sleep 0.01
done
if builtin kill -0 "${observed_pid}" 2>/dev/null; then
  echo "启动失败后应停止新进程 PID ${observed_pid}" >&2
  exit 1
fi

echo "gateway start ownership check passed"
