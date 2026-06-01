#!/usr/bin/env bash
set -euo pipefail

REPO_OWNER="${REPO_OWNER:-giovadifiore}"
REPO_NAME="${REPO_NAME:-pve-exporter}"
REPO_REF="${REPO_REF:-main}"
GO_VERSION="${GO_VERSION:-1.26.2}"
GO_BIN=""
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
SERVICE_FILE="${SERVICE_FILE:-/etc/systemd/system/smartctl-exporter.service}"
LISTEN_ADDRESS="${LISTEN_ADDRESS:-:9634}"
METRICS_PATH="${METRICS_PATH:-/metrics}"
SMARTCTL_BIN="${SMARTCTL_BIN:-/usr/sbin/smartctl}"
SCRAPE_TIMEOUT="${SCRAPE_TIMEOUT:-10s}"

version_ge() {
  # Returns success when $1 >= $2
  [[ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -n1)" == "$2" ]]
}

detect_go_arch() {
  case "$(uname -m)" in
    x86_64|amd64)
      echo "amd64"
      ;;
    aarch64|arm64)
      echo "arm64"
      ;;
    *)
      echo "unsupported"
      ;;
  esac
}

ensure_go_toolchain() {
  local current_go=""
  local current_version=""

  if command -v go >/dev/null 2>&1; then
    current_go="$(command -v go)"
    current_version="$("${current_go}" version | awk '{print $3}' | sed 's/^go//')"
  fi

  if [[ -n "${current_version}" ]] && version_ge "${current_version}" "${GO_VERSION}"; then
    echo "Using existing Go ${current_version} (${current_go})."
    GO_BIN="${current_go}"
    return 0
  fi

  local go_arch
  go_arch="$(detect_go_arch)"
  if [[ "${go_arch}" == "unsupported" ]]; then
    echo "Unsupported CPU architecture for automatic Go install: $(uname -m)" >&2
    exit 1
  fi

  local go_tgz="go${GO_VERSION}.linux-${go_arch}.tar.gz"
  local go_url="https://go.dev/dl/${go_tgz}"

  echo "Installing Go ${GO_VERSION} (${go_arch}) from ${go_url}..."
  curl -fsSL "${go_url}" -o "${TMP_DIR}/${go_tgz}"
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "${TMP_DIR}/${go_tgz}"

  export PATH="/usr/local/go/bin:${PATH}"
  hash -r
  GO_BIN="/usr/local/go/bin/go"

  local installed_version
  installed_version="$("${GO_BIN}" version | awk '{print $3}' | sed 's/^go//')"
  if ! version_ge "${installed_version}" "${GO_VERSION}"; then
    echo "Failed to install compatible Go version. Found: ${installed_version}" >&2
    exit 1
  fi

  echo "Go toolchain ready: $("${GO_BIN}" version)"
}

if [[ ${EUID} -ne 0 ]]; then
  echo "This installer must run as root (use sudo)." >&2
  exit 1
fi

if ! command -v systemctl >/dev/null 2>&1; then
  echo "systemd is required, but systemctl was not found." >&2
  exit 1
fi

if ! command -v apt-get >/dev/null 2>&1; then
  echo "Unsupported OS: this installer currently supports Debian/Ubuntu/Proxmox (apt-get)." >&2
  exit 1
fi

export DEBIAN_FRONTEND=noninteractive

echo "[1/6] Installing dependencies..."
apt-get update
apt-get install -y --no-install-recommends ca-certificates curl git smartmontools

if [[ ! -x "${SMARTCTL_BIN}" ]]; then
  echo "smartctl binary not found at ${SMARTCTL_BIN}." >&2
  echo "Set SMARTCTL_BIN to the correct path and run again." >&2
  exit 1
fi

TMP_DIR="$(mktemp -d)"
cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT

ensure_go_toolchain
echo "Using Go binary: ${GO_BIN}"

echo "[2/6] Downloading source from GitHub..."
git clone --depth 1 --branch "${REPO_REF}" "https://github.com/${REPO_OWNER}/${REPO_NAME}.git" "${TMP_DIR}/repo"

echo "[3/6] Building smartctl-exporter..."
(
  cd "${TMP_DIR}/repo"
  "${GO_BIN}" build -o "${TMP_DIR}/smartctl-exporter" ./agents/smartctl-exporter
)

echo "[4/6] Installing binary to ${INSTALL_DIR}..."
install -d "${INSTALL_DIR}"
install -m 0755 "${TMP_DIR}/smartctl-exporter" "${INSTALL_DIR}/smartctl-exporter"

echo "[5/6] Writing systemd unit to ${SERVICE_FILE}..."
cat > "${SERVICE_FILE}" <<EOF
[Unit]
Description=SMARTCTL Exporter for Prometheus
Documentation=https://github.com/${REPO_OWNER}/${REPO_NAME}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
Group=root
ExecStart=${INSTALL_DIR}/smartctl-exporter -listen-address ${LISTEN_ADDRESS} -metrics-path ${METRICS_PATH} -smartctl-bin ${SMARTCTL_BIN} -timeout ${SCRAPE_TIMEOUT}
Restart=on-failure
RestartSec=5

NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=full
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true

[Install]
WantedBy=multi-user.target
EOF

echo "[6/6] Enabling and starting service..."
systemctl daemon-reload
systemctl enable --now smartctl-exporter

if systemctl is-active --quiet smartctl-exporter; then
  echo "Installation complete. smartctl-exporter is active."
  echo "Health check: curl http://127.0.0.1:9634/health"
  echo "JSON test:   curl 'http://127.0.0.1:9634/metrics?disk=sda'"
else
  echo "Service failed to start. Showing status:" >&2
  systemctl --no-pager -l status smartctl-exporter >&2 || true
  exit 1
fi
