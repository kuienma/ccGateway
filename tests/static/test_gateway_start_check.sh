#!/usr/bin/env bash

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(cd "${SCRIPT_DIR}/../.." && pwd)"

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

echo "gateway start ownership check passed"
