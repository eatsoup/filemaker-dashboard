#!/usr/bin/env bash
# Install / update / uninstall the FileMaker Usage Dashboard as a systemd service.
#
# Subcommands:
#   build       Build the binary from source into ./filemaker-dashboard
#   install     Install (binary + config + systemd unit) and start the service
#   update      Download the latest release binary for this arch and restart
#   uninstall   Stop, disable, remove the service and binary (keeps config + DB)
#   status      Print service status

set -euo pipefail

REPO="eatsoup/filemaker-dashboard"
SERVICE_NAME="filemaker-dashboard"
SERVICE_USER="filemaker-dashboard"

BIN_PATH="/usr/local/bin/${SERVICE_NAME}"
CONFIG_DIR="/etc/${SERVICE_NAME}"
CONFIG_PATH="${CONFIG_DIR}/config.yaml"
DATA_DIR="/var/lib/${SERVICE_NAME}"
UNIT_PATH="/etc/systemd/system/${SERVICE_NAME}.service"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

die() { echo "error: $*" >&2; exit 1; }
need_cmd() { command -v "$1" >/dev/null 2>&1 || die "'$1' is required but not installed"; }
need_root() { [[ ${EUID} -eq 0 ]] || die "this command must run as root (try: sudo $0 $*)"; }

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac
}

cmd_build() {
  need_cmd go
  cd "${REPO_ROOT}"
  echo ">> building ${SERVICE_NAME}"
  CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "${SERVICE_NAME}" .
  echo ">> built: ${REPO_ROOT}/${SERVICE_NAME}"
}

ensure_user() {
  if ! id -u "${SERVICE_USER}" >/dev/null 2>&1; then
    echo ">> creating system user ${SERVICE_USER}"
    useradd --system --no-create-home --shell /usr/sbin/nologin \
      --home-dir "${DATA_DIR}" "${SERVICE_USER}"
  fi
}

install_binary_from_path() {
  local src="$1"
  [[ -f "${src}" ]] || die "binary not found at ${src} (run '$0 build' first or use '$0 update')"
  install -m 0755 -o root -g root "${src}" "${BIN_PATH}"
  echo ">> installed binary at ${BIN_PATH}"
}

install_config() {
  install -d -m 0755 -o root -g root "${CONFIG_DIR}"
  install -d -m 0750 -o "${SERVICE_USER}" -g "${SERVICE_USER}" "${DATA_DIR}"
  if [[ -f "${CONFIG_PATH}" ]]; then
    echo ">> config already exists at ${CONFIG_PATH}, leaving as-is"
    return
  fi
  local example="${REPO_ROOT}/config.example.yaml"
  [[ -f "${example}" ]] || die "config.example.yaml not found at ${example}"
  install -m 0640 -o root -g "${SERVICE_USER}" "${example}" "${CONFIG_PATH}"
  # Point db_path at the FHS data dir so the service's WorkingDirectory doesn't matter.
  sed -i "s|^db_path:.*|db_path: ${DATA_DIR}/filemaker.db|" "${CONFIG_PATH}"
  echo ">> wrote default config to ${CONFIG_PATH} — edit logfile_path and initial_admin before starting"
}

install_unit() {
  local src="${REPO_ROOT}/scripts/${SERVICE_NAME}.service"
  [[ -f "${src}" ]] || die "service unit not found at ${src}"
  install -m 0644 -o root -g root "${src}" "${UNIT_PATH}"
  systemctl daemon-reload
  echo ">> installed systemd unit at ${UNIT_PATH}"
}

cmd_install() {
  need_root "$@"
  need_cmd systemctl
  ensure_user

  local src="${REPO_ROOT}/${SERVICE_NAME}"
  if [[ ! -f "${src}" ]]; then
    echo ">> no built binary at ${src}; building from source"
    cmd_build
  fi
  install_binary_from_path "${src}"
  install_config
  install_unit

  systemctl enable "${SERVICE_NAME}.service"
  if grep -qE '^\s*logfile_path:\s*/Library/' "${CONFIG_PATH}"; then
    cat <<EOF
>> NOTE: ${CONFIG_PATH} still has the example logfile_path. Edit it, then:
     sudo systemctl start ${SERVICE_NAME}
EOF
  else
    systemctl restart "${SERVICE_NAME}.service"
    systemctl --no-pager --lines=5 status "${SERVICE_NAME}.service" || true
  fi
}

cmd_uninstall() {
  need_root "$@"
  need_cmd systemctl
  if systemctl list-unit-files | grep -q "^${SERVICE_NAME}.service"; then
    systemctl disable --now "${SERVICE_NAME}.service" || true
  fi
  rm -f "${UNIT_PATH}"
  systemctl daemon-reload
  rm -f "${BIN_PATH}"
  echo ">> removed service and binary"
  echo ">> kept ${CONFIG_DIR} and ${DATA_DIR} (delete manually if desired)"
}

cmd_update() {
  need_root "$@"
  need_cmd curl
  need_cmd systemctl
  local arch; arch="$(detect_arch)"
  local asset="filemaker-dashboard-linux-${arch}"
  local sha_asset="${asset}.sha256"

  echo ">> querying latest release for ${REPO}"
  local api="https://api.github.com/repos/${REPO}/releases/latest"
  local tag; tag="$(curl -fsSL "${api}" | sed -nE 's/.*"tag_name":\s*"([^"]+)".*/\1/p' | head -n1)"
  [[ -n "${tag}" ]] || die "could not determine latest release tag"
  echo ">> latest: ${tag}"

  local url="https://github.com/${REPO}/releases/download/${tag}/${asset}"
  local sha_url="https://github.com/${REPO}/releases/download/${tag}/${sha_asset}"

  local tmp; tmp="$(mktemp -d)"
  trap 'rm -rf "${tmp}"' EXIT
  echo ">> downloading ${asset}"
  curl -fsSL "${url}" -o "${tmp}/${asset}"
  if curl -fsSL "${sha_url}" -o "${tmp}/${sha_asset}" 2>/dev/null; then
    echo ">> verifying sha256"
    (cd "${tmp}" && sha256sum -c "${sha_asset}") || die "sha256 mismatch"
  else
    echo ">> no .sha256 published, skipping verification"
  fi

  local was_active=0
  systemctl is-active --quiet "${SERVICE_NAME}.service" && was_active=1 || true

  if [[ -f "${BIN_PATH}" ]]; then
    cp -a "${BIN_PATH}" "${BIN_PATH}.prev"
  fi
  install -m 0755 -o root -g root "${tmp}/${asset}" "${BIN_PATH}"
  echo ">> installed ${tag} at ${BIN_PATH}"

  if [[ ${was_active} -eq 1 ]]; then
    if ! systemctl restart "${SERVICE_NAME}.service"; then
      echo ">> restart failed, rolling back"
      [[ -f "${BIN_PATH}.prev" ]] && mv "${BIN_PATH}.prev" "${BIN_PATH}"
      systemctl restart "${SERVICE_NAME}.service" || true
      die "update aborted"
    fi
    rm -f "${BIN_PATH}.prev"
    systemctl --no-pager --lines=5 status "${SERVICE_NAME}.service" || true
  fi
}

cmd_status() {
  need_cmd systemctl
  systemctl --no-pager status "${SERVICE_NAME}.service" || true
}

usage() {
  cat <<EOF
Usage: $0 <command>

Commands:
  build       Build ${SERVICE_NAME} from source (no root)
  install     Install service, config and binary, then enable+start (root)
  update      Pull the latest release for this arch and restart (root)
  uninstall   Disable and remove service + binary; keep config + DB (root)
  status      Show systemctl status

Paths used:
  binary:   ${BIN_PATH}
  config:   ${CONFIG_PATH}
  data:     ${DATA_DIR}
  unit:     ${UNIT_PATH}
  user:     ${SERVICE_USER}
EOF
}

case "${1:-}" in
  build)     shift; cmd_build "$@" ;;
  install)   shift; cmd_install "$@" ;;
  update)    shift; cmd_update "$@" ;;
  uninstall) shift; cmd_uninstall "$@" ;;
  status)    shift; cmd_status "$@" ;;
  ""|-h|--help|help) usage ;;
  *) usage; exit 2 ;;
esac
