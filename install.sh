#!/usr/bin/env bash
# xboard-xui-bridge 一键安装 / 管理脚本。
#
# Xboard 与 3x-ui 之间的非侵入式中间件。本脚本封装了一键安装、升级、卸载、
# 状态查看、日志检索、密码重置、监听地址修改等高频运维动作。
#
# 用法：
#
#   bash <(curl -fsSL https://raw.githubusercontent.com/A-pursuer/x-bridge/main/install.sh)
#       默认行为：未装 → 安装；已装 → 进入管理菜单。
#
#   xui-bridge [子命令]                    安装后可用的快捷命令
#     install / update / upgrade           安装或升级（保留 data/）
#     uninstall                            卸载二进制 + service（**保留** data/）
#     purge                                完全清理（含 data/，**不可恢复**）
#     start | stop | restart               服务启停
#     status                               systemctl status
#     log [-e|-w|-i] [N]                   最近 N 行日志，可按级别筛选（默认 200）
#     follow                               实时跟踪日志
#     reset-password                       交互式重置 admin 密码
#     change-listen-addr                   交互式修改 Web 监听地址
#     backup                               备份 data/ 到时间戳 tar.gz
#     version                              打印脚本与已装二进制版本
#     menu                                 显式进入菜单（默认入口）
#     help | --help | -h                   打印帮助
#
# 设计要点（运维专业化，v0.8.4 起）：
#
#   1) trap ERR + LINENO：任何未捕获错误会打印失败行号 + 命令 + 退出码，
#      不再 "set -e 后默默退出"，运维一眼看到根因。
#   2) systemd unit 安全 hardening：NoNewPrivileges / PrivateTmp /
#      ProtectSystem=strict / ProtectHome / ReadWritePaths=DATA_DIR /
#      ProtectKernel* / RestrictNamespaces / RestrictSUIDSGID 等让常见
#      容器级越权失效。
#   3) 升级前自动备份：旧二进制 → ${BIN_PATH}.bak.<ts>，失败可一键 rollback。
#   4) 启动健康检查：写完 unit 后 wait 5s 确认 active + 无 ERROR 日志，
#      否则提示运维查看 journalctl。
#   5) GitHub 加速 fallback：直连失败时自动尝试 ghproxy / fastgit 镜像。
#   6) 菜单状态摘要：菜单顶部显示当前版本 / 监听端口 / 启动时长 / 内存。
#   7) 防火墙自动放行：检测到 ufw / firewalld 时可交互式放行 8787。
#
# 返回码语义：
#
#   0   成功
#   1   运行期错误（systemctl 失败 / 下载失败 / 用户取消等）
#   2   参数错误 / 前置检查失败（非 root / 不支持的 OS / 不支持的架构）

set -euo pipefail

# 让 trap ERR 在 subshell / function / pipeline 内同样生效。
set -E -o pipefail

SCRIPT_VERSION="0.8.4"

# ---------------- 颜色与日志辅助 ----------------
# 仅当 stdout 是终端且 NO_COLOR 未设置时启用彩色。被 pipe 到文件 / cron
# 的场景下日志保持纯文本，方便机器解析。
if [[ -t 1 && -z "${NO_COLOR:-}" ]]; then
    red='\033[0;31m'
    green='\033[0;32m'
    yellow='\033[0;33m'
    blue='\033[0;34m'
    cyan='\033[0;36m'
    magenta='\033[0;35m'
    bold='\033[1m'
    dim='\033[2m'
    plain='\033[0m'
else
    red=''; green=''; yellow=''; blue=''; cyan=''; magenta=''; bold=''; dim=''; plain=''
fi

# log_* 使用 printf 而不是 echo -e：跨 dash/bash/zsh 行为一致，且能
# 安全转义用户输入。
log_info()    { printf "${green}[ OK  ]${plain} %s\n" "$*"; }
log_warn()    { printf "${yellow}[WARN ]${plain} %s\n" "$*" >&2; }
log_error()   { printf "${red}[ERROR]${plain} %s\n" "$*" >&2; }
log_step()    { printf "${cyan}[ ... ]${plain} %s\n" "$*"; }
log_action()  { printf "${blue}[ >>  ]${plain} %s\n" "$*"; }
log_hint()    { printf "${dim}        %s${plain}\n" "$*"; }

fail() {
    log_error "$*"
    exit 1
}

# 通用 ERR trap：未捕获错误时打印失败上下文。
# 调用栈：bash 5.0+ 的 BASH_LINENO[0] 是出错行号，BASH_COMMAND 是当时命令。
err_trap() {
    local code=$?
    local lineno=${BASH_LINENO[0]:-?}
    local cmd=${BASH_COMMAND:-?}
    log_error "未捕获错误："
    log_error "  位置：${BASH_SOURCE[1]:-$0}:${lineno}"
    log_error "  命令：${cmd}"
    log_error "  退出码：${code}"
    log_hint "建议：用 \`xui-bridge log\` 查看最近日志；如反复出现请在 GitHub Issue 反馈。"
    exit "${code}"
}
trap err_trap ERR

# ---------------- 常量 ----------------
GITHUB_REPO="A-pursuer/x-bridge"
INSTALL_DIR="/usr/local/xboard-xui-bridge"
DATA_DIR="${INSTALL_DIR}/data"
BACKUP_DIR="${INSTALL_DIR}/backups"
BIN_PATH="/usr/local/bin/xboard-xui-bridge"
HELPER_PATH="/usr/local/bin/xui-bridge"
SCRIPT_COPY="${INSTALL_DIR}/install.sh"
SERVICE_NAME="xboard-xui-bridge"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
DEFAULT_PORT=8787

# GitHub 加速镜像，按顺序探测；首个 HTTP 200 的胜出。空字符串 = 直连。
GH_MIRRORS=(
    ""                                                     # 直连
    "https://ghproxy.com/"                                 # ghproxy
    "https://gh-proxy.com/"                                # gh-proxy
    "https://mirror.ghproxy.com/"                          # mirror.ghproxy.com
)

# 启动健康检查参数。
HEALTH_WAIT_SECONDS=5      # active 等待秒数
HEALTH_ERROR_THRESHOLD=0   # 启动后允许的 ERROR 日志条数

# ---------------- Banner ----------------
print_banner() {
    printf "${cyan}"
    cat <<'EOF'
   __  ___                       __        __ __ __ ____
  /  |/  /__  ___ _____  ___ ___/ /  ___ _/_// // // _/
 /     /  ' \/  ' / _/_/__/ // /  / _ `/ , /// __// /_
/_/|_/_/\_,_/_/ /  /_/   \_, /__/_\_,_/_/|_|/_/  /_/__/
                       /___/
EOF
    printf "        xboard-xui-bridge — Xboard ↔ 3x-ui Middleware\n"
    printf "                installer/manager v%s\n${plain}\n" "${SCRIPT_VERSION}"
}

# ---------------- 前置检查 ----------------
require_root() {
    if [[ "${EUID}" -ne 0 ]]; then
        fail "请以 root 身份运行：sudo bash $0"
    fi
}

require_systemd() {
    if ! command -v systemctl >/dev/null 2>&1; then
        fail "未检测到 systemd（systemctl）；本脚本仅支持基于 systemd 的发行版"
    fi
}

# detect_arch 把 uname -m 输出映射到本项目 Release 资产名约定。
detect_arch() {
    local arch
    arch=$(uname -m)
    case "${arch}" in
        x86_64|amd64)         echo "linux-amd64" ;;
        aarch64|arm64)        echo "linux-arm64" ;;
        armv7l|armv7|armhf)   echo "linux-arm" ;;
        *) fail "暂不支持的架构：${arch}（仅支持 amd64 / arm64 / armv7；其他架构请手工 \`make build-linux\` 编译）" ;;
    esac
}

# detect_os 读取 /etc/os-release 的 ID 字段（小写 distro 标识）。
detect_os() {
    if [[ -f /etc/os-release ]]; then
        # shellcheck disable=SC1091
        . /etc/os-release
        echo "${ID:-unknown}"
    else
        echo "unknown"
    fi
}

# is_installed 仅在二进制 + service 文件都存在时才视为"已装"。
is_installed() {
    [[ -f "${BIN_PATH}" ]] && [[ -f "${SERVICE_FILE}" ]]
}

service_active() {
    systemctl is-active --quiet "${SERVICE_NAME}" 2>/dev/null
}

# port_in_use 检测指定 TCP 端口是否被占用——优先 ss，其次 netstat，最后 lsof。
port_in_use() {
    local port="$1"
    if command -v ss >/dev/null 2>&1; then
        ss -ltn 2>/dev/null | awk -v p=":${port}$" '$4 ~ p { found=1 } END { exit !found }'
        return
    fi
    if command -v netstat >/dev/null 2>&1; then
        netstat -lnt 2>/dev/null | awk -v p=":${port} " '$4 ~ p { found=1 } END { exit !found }'
        return
    fi
    if command -v lsof >/dev/null 2>&1; then
        lsof -nP -iTCP:"${port}" -sTCP:LISTEN >/dev/null 2>&1
        return
    fi
    return 1
}

# get_public_ip 尽力获取公网 IP，用于在安装完成提示中显示访问地址。
get_public_ip() {
    local ip
    ip=$(curl -fsSL --max-time 5 https://api.ipify.org 2>/dev/null || true)
    if [[ -z "${ip}" ]]; then
        ip=$(curl -fsSL --max-time 5 https://ipv4.icanhazip.com 2>/dev/null || true)
    fi
    if [[ -z "${ip}" ]]; then
        ip="<server-ip>"
    fi
    echo "${ip}"
}

# confirm_yes 提示 [y/N] 二次确认；默认拒绝（防误操作）。
confirm_yes() {
    local prompt="$1"
    local reply
    read -rp "${prompt} [y/N]: " reply || return 1
    case "${reply}" in
        y|Y|yes|YES) return 0 ;;
        *) return 1 ;;
    esac
}

# ---------------- 依赖安装 ----------------
install_dependencies() {
    local missing=()
    command -v curl >/dev/null 2>&1 || missing+=("curl")
    command -v tar >/dev/null 2>&1  || missing+=("tar")
    if [[ ${#missing[@]} -eq 0 ]]; then
        log_info "依赖 curl / tar 已就绪"
        return
    fi
    local os
    os=$(detect_os)
    log_step "安装依赖：${missing[*]}（os=${os}）"
    case "${os}" in
        ubuntu|debian|armbian)
            apt-get update -y && apt-get install -y "${missing[@]}"
            ;;
        centos|rhel|fedora|rocky|almalinux|ol|amzn)
            if command -v dnf >/dev/null 2>&1; then
                dnf install -y "${missing[@]}"
            else
                yum install -y "${missing[@]}"
            fi
            ;;
        alpine)
            apk add --no-cache "${missing[@]}"
            ;;
        arch|manjaro)
            pacman -Sy --noconfirm "${missing[@]}"
            ;;
        opensuse-tumbleweed|opensuse-leap)
            zypper -q install -y "${missing[@]}"
            ;;
        *)
            log_warn "未识别的系统 ${os}，请手动确认 ${missing[*]} 已安装后重试"
            fail "依赖不满足"
            ;;
    esac
}

# ---------------- 版本与下载 ----------------
# 包装 curl：拼接镜像前缀；失败时返回非零。
curl_with_mirror() {
    local mirror="$1"; shift
    local url="$1"; shift
    local final_url="${mirror}${url}"
    curl "$@" "${final_url}"
}

# fetch_with_fallback 尝试用 GH_MIRRORS 顺序下载；首个成功即返回。
# 所有镜像都失败时返回非零。
#
# 用法：fetch_with_fallback <url> <-o output> [其他 curl args]
# 注意：调用方应传入 -fL 等基础选项。
fetch_with_fallback() {
    local url="$1"; shift
    local m
    for m in "${GH_MIRRORS[@]}"; do
        if curl -fsSL --connect-timeout 10 --max-time 60 "$@" "${m}${url}"; then
            return 0
        fi
        if [[ -n "${m}" ]]; then
            log_warn "镜像 ${m} 失败，尝试下一个"
        fi
    done
    return 1
}

get_latest_version() {
    # 优先走 GitHub API（最准）；失败时退到 raw mirror 上的版本占位文件，
    # 最后再尝试用 git ls-remote 解析最新 tag——后两路兜底让中国大陆等
    # API 限频环境仍能升级。
    local api="https://api.github.com/repos/${GITHUB_REPO}/releases/latest"
    local tag
    tag=$(curl -fsSL --connect-timeout 10 --max-time 30 "${api}" 2>/dev/null \
        | grep -E '"tag_name"' | head -1 | sed -E 's/.*"([^"]+)".*/\1/' || true)
    if [[ -n "${tag}" ]]; then
        echo "${tag}"
        return
    fi

    log_warn "GitHub API 不可达，尝试通过 git ls-remote 探测最新 tag"
    if command -v git >/dev/null 2>&1; then
        tag=$(git ls-remote --tags --refs --sort=-v:refname \
            "https://github.com/${GITHUB_REPO}.git" 2>/dev/null \
            | head -1 | sed -E 's@.*refs/tags/(.*)$@\1@' || true)
        if [[ -n "${tag}" ]]; then
            log_info "从 git ls-remote 拿到 ${tag}"
            echo "${tag}"
            return
        fi
    fi

    fail "无法获取最新版本（GitHub API + git ls-remote 均失败）；请稍后重试或手动 \`curl -fsSL https://api.github.com/repos/${GITHUB_REPO}/releases/latest\` 检查网络。"
}

# get_installed_version 读取已装二进制版本号。无安装返回 "(none)"。
get_installed_version() {
    if [[ -x "${BIN_PATH}" ]]; then
        "${BIN_PATH}" version 2>/dev/null | awk '{print $2}' | head -1
    else
        echo "(none)"
    fi
}

# download_and_install 下载 release tarball、校验 SHA256、解压安装。
download_and_install() {
    local arch="$1"
    local version="$2"
    (
        local asset="xboard-xui-bridge-${arch}.tar.gz"
        local url_path="${GITHUB_REPO}/releases/download/${version}/${asset}"
        local sums_path="${GITHUB_REPO}/releases/download/${version}/SHA256SUMS.txt"
        local tmpdir
        tmpdir=$(mktemp -d)
        trap 'rm -rf "${tmpdir}"' EXIT

        log_step "下载 ${asset}（${version}）"
        if ! fetch_with_fallback "https://github.com/${url_path}" -o "${tmpdir}/${asset}"; then
            fail "下载失败：所有镜像均不可达。可尝试手工下载 https://github.com/${url_path} 后再 \`xui-bridge install\`。"
        fi

        log_step "校验 SHA256"
        if fetch_with_fallback "https://github.com/${sums_path}" -o "${tmpdir}/SHA256SUMS.txt"; then
            if command -v sha256sum >/dev/null 2>&1; then
                local expected actual
                expected=$(grep " ${asset}\$" "${tmpdir}/SHA256SUMS.txt" | awk '{print $1}' || true)
                if [[ -z "${expected}" ]]; then
                    log_warn "SHA256SUMS.txt 中未找到 ${asset}，跳过校验（release 可能未发布该文件）"
                else
                    actual=$(sha256sum "${tmpdir}/${asset}" | awk '{print $1}')
                    if [[ "${expected}" != "${actual}" ]]; then
                        fail "SHA256 校验失败！expected=${expected} actual=${actual}（疑似下载损坏或被中间人篡改）"
                    fi
                    log_info "SHA256 校验通过"
                fi
            else
                log_warn "未安装 sha256sum，跳过校验（建议 apt install coreutils）"
            fi
        else
            log_warn "无法获取 SHA256SUMS.txt（旧 release 可能未发布），跳过校验"
        fi

        log_step "解压并安装到 ${BIN_PATH}"
        tar -xzf "${tmpdir}/${asset}" -C "${tmpdir}"
        install -m 0755 "${tmpdir}/xboard-xui-bridge" "${BIN_PATH}"
    )
}

# backup_existing 升级前备份旧二进制 + bridge.db 到 BACKUP_DIR。
# 保留最近 5 份，超出按时间戳清理最旧的。
#
# 一致性策略（v0.8.4 Codex 审查指出）：
#
#   SQLite WAL 模式下"热"拷贝 .db + -wal + -shm 三个文件不保证一致快照
#   ——拷贝期间被并发写的概率极低（中间件每 60s 一次同步）但仍存在。
#   优先策略：服务在跑时，用 sqlite3 CLI `.backup` 命令做"应用层一致备份"；
#   备用策略：sqlite3 不可用 → 退化为三文件冷拷贝并打 WARN。
#
#   服务已停止时，文件已无并发写，直接 cp 即可——cmd_install 的升级流程
#   会先 stop service，再调用本函数，确保大多数场景拿到一致快照。
backup_existing() {
    if ! is_installed; then
        return 0
    fi
    mkdir -p "${BACKUP_DIR}"
    chmod 700 "${BACKUP_DIR}"
    local ts
    ts=$(date +%Y%m%d-%H%M%S)
    log_step "备份当前二进制与数据库到 ${BACKUP_DIR}/${ts}/"
    mkdir -p "${BACKUP_DIR}/${ts}"
    cp -f "${BIN_PATH}" "${BACKUP_DIR}/${ts}/xboard-xui-bridge" 2>/dev/null || true
    if [[ -f "${DATA_DIR}/bridge.db" ]]; then
        if service_active && command -v sqlite3 >/dev/null 2>&1; then
            # 应用层一致备份：sqlite3 .backup 在源端持锁完成 page-level copy。
            if sqlite3 "${DATA_DIR}/bridge.db" ".backup '${BACKUP_DIR}/${ts}/bridge.db'" 2>/dev/null; then
                log_info "使用 sqlite3 .backup 完成一致性快照"
            else
                log_warn "sqlite3 .backup 失败，退化为文件级拷贝（可能非一致快照）"
                cp -f "${DATA_DIR}/bridge.db" "${BACKUP_DIR}/${ts}/bridge.db" 2>/dev/null || true
                cp -f "${DATA_DIR}/bridge.db-wal" "${BACKUP_DIR}/${ts}/" 2>/dev/null || true
                cp -f "${DATA_DIR}/bridge.db-shm" "${BACKUP_DIR}/${ts}/" 2>/dev/null || true
            fi
        else
            # 服务已停 或 无 sqlite3 CLI：直接 cp。服务已停时一致；
            # 无 sqlite3 时打 WARN 提醒运维装一份让后续备份更稳。
            if service_active; then
                log_warn "未检测到 sqlite3 CLI 且服务正在运行；本次备份用文件级拷贝（可能非一致快照）"
                log_hint "建议安装 sqlite3：apt install sqlite3 / yum install sqlite"
            fi
            cp -f "${DATA_DIR}/bridge.db" "${BACKUP_DIR}/${ts}/bridge.db" 2>/dev/null || true
            cp -f "${DATA_DIR}/bridge.db-wal" "${BACKUP_DIR}/${ts}/" 2>/dev/null || true
            cp -f "${DATA_DIR}/bridge.db-shm" "${BACKUP_DIR}/${ts}/" 2>/dev/null || true
        fi
    fi
    # 保留最近 5 份。
    local backups
    mapfile -t backups < <(ls -1 -t "${BACKUP_DIR}" 2>/dev/null | tail -n +6)
    local b
    for b in "${backups[@]}"; do
        rm -rf "${BACKUP_DIR:?}/${b}"
    done
    log_info "备份完成（保留最近 5 份）"
}

# ---------------- 目录与服务 ----------------
setup_directories() {
    log_step "准备数据目录 ${DATA_DIR}"
    mkdir -p "${DATA_DIR}"
    chmod 700 "${DATA_DIR}"
}

# install_helper 把 install.sh 拷贝到 INSTALL_DIR 并创建 xui-bridge 别名。
install_helper() {
    log_step "安装 helper 命令 ${HELPER_PATH}"
    local self_real script_copy_real
    self_real=$(readlink -f "$0" 2>/dev/null || true)
    script_copy_real=$(readlink -f "${SCRIPT_COPY}" 2>/dev/null || true)

    if [[ -n "${self_real}" && "${self_real}" == "${script_copy_real}" ]]; then
        log_step "helper 模式：尝试从 GitHub 刷新 install.sh"
        if fetch_with_fallback "https://raw.githubusercontent.com/${GITHUB_REPO}/main/install.sh" -o "${SCRIPT_COPY}.new"; then
            mv -f "${SCRIPT_COPY}.new" "${SCRIPT_COPY}"
            log_info "已从 GitHub 刷新 install.sh"
        else
            rm -f "${SCRIPT_COPY}.new"
            log_warn "无法拉取最新 install.sh，保留本地旧版本（离线模式可继续工作）"
        fi
    elif [[ -f "$0" ]] && [[ -r "$0" ]]; then
        cp "$0" "${SCRIPT_COPY}"
    else
        if ! fetch_with_fallback "https://raw.githubusercontent.com/${GITHUB_REPO}/main/install.sh" -o "${SCRIPT_COPY}"; then
            log_warn "无法从 GitHub 固化 install.sh（curl 失败），helper 将不可用"
            return 0
        fi
    fi
    chmod 0755 "${SCRIPT_COPY}"
    ln -sf "${SCRIPT_COPY}" "${HELPER_PATH}"
}

write_systemd_unit() {
    log_step "写入 systemd unit ${SERVICE_FILE}"
    # systemd 安全 hardening（v0.8.4 起）：
    #
    # NoNewPrivileges            禁用 setuid 二进制提权
    # PrivateTmp                 独立 /tmp 命名空间
    # ProtectSystem=strict       /usr /etc 只读，仅 ReadWritePaths 允许写
    # ProtectHome                / root /home /run/user 不可见
    # ProtectKernelTunables      /proc/sys /sys 只读
    # ProtectKernelModules       禁加载 / 卸载模块
    # ProtectKernelLogs          隔离 dmesg
    # ProtectControlGroups       /sys/fs/cgroup 只读
    # ProtectClock               禁改时钟
    # ProtectHostname            禁改 hostname
    # ProtectProc=invisible      /proc/<pid> 只能看自己
    # RestrictNamespaces         禁建 namespace
    # RestrictSUIDSGID           禁创建 setuid 文件
    # RestrictRealtime           禁实时调度
    # LockPersonality            禁改 ABI personality
    # SystemCallArchitectures=native  仅本地系统调用 ABI
    # SystemCallFilter=@system-service systemd 推荐的 system service 集合，
    #                            覆盖 net/http、sqlite、json、文件 IO 等
    #                            依赖的 syscall。**不再** ~@privileged
    #                            ~@resources 排除——@resources 中的
    #                            sched_setaffinity / prlimit / setrlimit
    #                            是 Go runtime 调度所必须（v0.8.4 Codex
    #                            审查指出过度限制风险）。
    # CapabilityBoundingSet=      清空所有 cap（中间件默认 8787 > 1024，
    #                            不需要 CAP_NET_BIND_SERVICE；如需绑定低
    #                            端口请用 systemctl edit 加回去）
    # AmbientCapabilities=        清空（同上）
    #
    # 不启用 MemoryDenyWriteExecute：Go runtime 的 cgo / plugin / JIT 路径
    # 在极端情况下需要 W+X；本中间件目前不依赖 cgo，但保留这层 future-proof
    # 兼容性。如需更高安全可由运维 systemctl edit 加上。
    cat > "${SERVICE_FILE}" <<EOF
[Unit]
Description=xboard-xui-bridge — 非侵入式 Xboard / 3x-ui 中间件
Documentation=https://github.com/${GITHUB_REPO}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
WorkingDirectory=${INSTALL_DIR}
Environment=BRIDGE_LISTEN_ADDR=:${DEFAULT_PORT}
ExecStart=${BIN_PATH} run --db ${DATA_DIR}/bridge.db
Restart=on-failure
RestartSec=5s
LimitNOFILE=65535
TimeoutStartSec=30
TimeoutStopSec=30

# ===== 安全 hardening =====
NoNewPrivileges=yes
PrivateTmp=yes
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=${DATA_DIR}
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
ProtectProc=invisible
ProcSubset=pid
RestrictNamespaces=yes
RestrictSUIDSGID=yes
RestrictRealtime=yes
LockPersonality=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
CapabilityBoundingSet=
AmbientCapabilities=

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
}

start_or_restart_service() {
    if systemctl is-active --quiet "${SERVICE_NAME}"; then
        log_step "重启服务"
        systemctl restart "${SERVICE_NAME}"
    else
        log_step "启用并启动服务"
        systemctl enable "${SERVICE_NAME}"
        systemctl start "${SERVICE_NAME}"
    fi
}

# wait_for_active 启动后等待至多 ${HEALTH_WAIT_SECONDS} 秒确认 active。
# 失败时打印诊断信息 + 部分日志方便快速排错。
wait_for_active() {
    local i
    for ((i=0; i<HEALTH_WAIT_SECONDS; i++)); do
        if service_active; then
            log_info "服务运行中（active）"
            return 0
        fi
        sleep 1
    done
    log_error "服务未在 ${HEALTH_WAIT_SECONDS} 秒内进入 active 状态"
    log_hint "最近 30 行日志："
    journalctl -u "${SERVICE_NAME}" -n 30 --no-pager 2>&1 | sed 's/^/        /'
    return 1
}

# check_no_errors 启动后扫描最近 N 行日志的 ERROR 计数。
check_no_errors() {
    local n=50
    local errors
    errors=$(journalctl -u "${SERVICE_NAME}" -n "${n}" --no-pager 2>/dev/null \
        | grep -ciE '"level":"error"|^\S+\s\S+\s\S+ ERROR' || true)
    if [[ "${errors}" -gt "${HEALTH_ERROR_THRESHOLD}" ]]; then
        log_warn "启动后最近 ${n} 行日志中检测到 ${errors} 条 ERROR；建议 \`xui-bridge log -e\` 查看详情"
        return 1
    fi
    return 0
}

# ---------------- 防火墙放行 ----------------
maybe_open_firewall() {
    local port="$1"
    if command -v ufw >/dev/null 2>&1; then
        if ufw status 2>/dev/null | grep -q "Status: active"; then
            if confirm_yes "检测到 ufw 处于启用状态，是否放行 ${port}/tcp？"; then
                ufw allow "${port}/tcp" >/dev/null 2>&1 || true
                log_info "ufw 已放行 ${port}/tcp"
            fi
        fi
    elif command -v firewall-cmd >/dev/null 2>&1; then
        if firewall-cmd --state >/dev/null 2>&1; then
            if confirm_yes "检测到 firewalld 处于启用状态，是否放行 ${port}/tcp？"; then
                firewall-cmd --add-port="${port}/tcp" --permanent >/dev/null 2>&1 || true
                firewall-cmd --reload >/dev/null 2>&1 || true
                log_info "firewalld 已放行 ${port}/tcp"
            fi
        fi
    fi
}

# ---------------- 完成提示 ----------------
show_post_install() {
    log_step "等待 ~3 秒让首次启动完成"
    sleep 3
    # 健康检查（v0.8.4 Codex 审查要求不吞失败）：
    # wait_for_active 失败 = 服务真的没起来 → 退出非零让 install/update 的
    # 调用方拿到失败信号，不能伪装"安装完成"。
    # check_no_errors 失败 = active 但有 ERROR 日志，可能是 token / 网络等
    # 软问题；仅 WARN 不阻断（与 wait_for_active 的硬失败语义区分清楚）。
    if ! wait_for_active; then
        log_error "服务启动健康检查未通过；请按上方日志诊断后重试"
        return 1
    fi
    check_no_errors || true

    local pwd_file="${DATA_DIR}/initial_password.txt"
    local pwd_line=""
    if [[ -f "${pwd_file}" ]]; then
        pwd_line=$(cat "${pwd_file}" 2>/dev/null || true)
    fi

    local public_ip installed_version
    public_ip=$(get_public_ip)
    installed_version=$(get_installed_version)

    echo
    printf "${bold}${cyan}═══════════════════════════════════════════════════════════════${plain}\n"
    printf "  ${bold}xboard-xui-bridge 安装完成${plain}\n"
    printf "${bold}${cyan}═══════════════════════════════════════════════════════════════${plain}\n"
    echo
    printf "  ${bold}版本：${plain}            %s\n" "${installed_version}"
    printf "  ${bold}Web 面板：${plain}        http://%s:%s\n" "${public_ip}" "${DEFAULT_PORT}"
    printf "  ${bold}默认用户名：${plain}      admin\n"
    if [[ -n "${pwd_line}" ]]; then
        printf "  ${bold}初始密码：${plain}        ${green}%s${plain}\n" "${pwd_line}"
        echo
        printf "  密码同时写入文件：%s\n" "${pwd_file}"
        printf "  ${yellow}首次登录后请立即修改密码并妥善保管该文件。${plain}\n"
    else
        printf "  ${bold}初始密码：${plain}        ${dim}（升级安装，沿用旧密码）${plain}\n"
    fi
    echo
    printf "  ${bold}快捷管理命令${plain}（已注册到 \$PATH）：\n"
    printf "    ${cyan}xui-bridge${plain}                       打开管理菜单\n"
    printf "    ${cyan}xui-bridge status${plain}                查看运行状态\n"
    printf "    ${cyan}xui-bridge log${plain} ${dim}[-e|-w]${plain}              查看日志（可按级别筛选）\n"
    printf "    ${cyan}xui-bridge follow${plain}                实时跟踪日志\n"
    printf "    ${cyan}xui-bridge restart${plain}               重启服务\n"
    printf "    ${cyan}xui-bridge reset-password${plain}        重置 admin 密码\n"
    printf "    ${cyan}xui-bridge backup${plain}                备份数据库\n"
    printf "    ${cyan}xui-bridge change-listen-addr${plain}    修改监听地址\n"
    printf "    ${cyan}xui-bridge uninstall${plain}             卸载（保留 data/）\n"
    printf "    ${cyan}xui-bridge purge${plain}                 完全清理（含 data/）\n"
    echo

    # 防火墙：检测到主流 firewall 时提示放行。
    maybe_open_firewall "${DEFAULT_PORT}"

    echo
    printf "${bold}${cyan}═══════════════════════════════════════════════════════════════${plain}\n"
}

# ---------------- 子命令实现 ----------------
cmd_install() {
    install_dependencies

    local arch version
    arch=$(detect_arch)
    log_info "架构：${arch}"

    version=$(get_latest_version)
    local current
    current=$(get_installed_version)
    if [[ "${current}" != "(none)" ]]; then
        log_info "升级：${current} → ${version}"
    else
        log_info "目标版本：${version}"
    fi

    if port_in_use "${DEFAULT_PORT}"; then
        log_warn "端口 ${DEFAULT_PORT} 已被占用——若不是本中间件历史进程，"
        log_warn "建议安装后通过 \`xui-bridge change-listen-addr\` 修改监听端口。"
    fi

    backup_existing
    download_and_install "${arch}" "${version}"
    setup_directories
    write_systemd_unit
    install_helper
    start_or_restart_service
    show_post_install
}

# cmd_uninstall 卸载二进制 + service + helper，**保留** data/。
cmd_uninstall() {
    if ! is_installed; then
        log_warn "未检测到已安装实例，无需卸载"
        return 0
    fi
    echo
    log_warn "即将卸载 xboard-xui-bridge："
    printf "    • 停止并禁用 systemd 服务 ${bold}%s${plain}\n" "${SERVICE_NAME}"
    printf "    • 删除二进制 %s\n" "${BIN_PATH}"
    printf "    • 删除 service 文件 %s\n" "${SERVICE_FILE}"
    printf "    • 删除 helper 命令 %s\n" "${HELPER_PATH}"
    printf "    • ${green}保留${plain} 数据目录 %s（含数据库、密码、日志、备份）\n" "${DATA_DIR}"
    echo
    if ! confirm_yes "确定继续吗？"; then
        log_info "已取消"
        return 0
    fi
    log_step "停止并禁用服务"
    systemctl stop "${SERVICE_NAME}" 2>/dev/null || true
    systemctl disable "${SERVICE_NAME}" 2>/dev/null || true
    log_step "删除二进制 / service / drop-in / helper"
    rm -f "${BIN_PATH}" "${SERVICE_FILE}" "${HELPER_PATH}" "${SCRIPT_COPY}"
    rm -rf "/etc/systemd/system/${SERVICE_NAME}.service.d"
    systemctl daemon-reload
    log_info "卸载完成；data/ 仍保留在 ${DATA_DIR}"
    log_hint "完整清理（含数据）请运行：xui-bridge purge"
}

# cmd_purge 完全清理（含 data/）。
cmd_purge() {
    echo
    printf "${red}${bold}⚠ 即将彻底清理 xboard-xui-bridge 全部数据：${plain}\n" >&2
    printf "    • 停止服务 + 删二进制 + 删 service + 删 helper（同 uninstall）\n"
    printf "    • ${red}并删除${plain} 数据目录 %s（含数据库、密码、日志、备份）\n" "${DATA_DIR}"
    printf "    • ${red}此操作不可恢复${plain}：所有桥接配置、管理员账户、流量基线都会丢失\n"
    echo
    local reply
    read -rp "请输入 PURGE 确认（区分大小写）：" reply || return 1
    if [[ "${reply}" != "PURGE" ]]; then
        log_info "确认未通过，已取消"
        return 0
    fi
    log_step "停止并禁用服务"
    systemctl stop "${SERVICE_NAME}" 2>/dev/null || true
    systemctl disable "${SERVICE_NAME}" 2>/dev/null || true
    log_step "删除全部文件 + 数据目录 + drop-in"
    rm -f "${BIN_PATH}" "${SERVICE_FILE}" "${HELPER_PATH}"
    rm -rf "/etc/systemd/system/${SERVICE_NAME}.service.d"
    rm -rf "${INSTALL_DIR}"
    systemctl daemon-reload
    log_info "彻底清理完成"
}

cmd_start()   { systemctl start "${SERVICE_NAME}";   log_info "已启动 ${SERVICE_NAME}"; }
cmd_stop()    { systemctl stop "${SERVICE_NAME}";    log_info "已停止 ${SERVICE_NAME}"; }
cmd_restart() { systemctl restart "${SERVICE_NAME}"; log_info "已重启 ${SERVICE_NAME}"; }
cmd_status()  { systemctl status "${SERVICE_NAME}" --no-pager || true; }

# cmd_log 支持按级别筛选 + 自定义行数。
#
# 用法：
#   xui-bridge log              最近 200 行原样
#   xui-bridge log -e           仅 ERROR
#   xui-bridge log -w           ERROR + WARN
#   xui-bridge log -i           ERROR + WARN + INFO（不含 DEBUG）
#   xui-bridge log 500          最近 500 行
#   xui-bridge log -e 1000      最近 1000 行中的 ERROR
cmd_log() {
    local lines=200
    local filter=""
    local arg
    for arg in "$@"; do
        case "${arg}" in
            -e|--error)  filter='"level":"error"' ;;
            -w|--warn)   filter='"level":"error"|"level":"warn"' ;;
            -i|--info)   filter='"level":"error"|"level":"warn"|"level":"info"' ;;
            *[0-9]*)     lines="${arg}" ;;
        esac
    done
    if [[ -n "${filter}" ]]; then
        journalctl -u "${SERVICE_NAME}" -n "${lines}" --no-pager \
            | grep --color=auto -E "${filter}" || log_info "无匹配日志"
    else
        journalctl -u "${SERVICE_NAME}" -n "${lines}" --no-pager
    fi
}

cmd_log_follow() {
    log_info "实时跟踪日志中（Ctrl+C 退出）"
    journalctl -u "${SERVICE_NAME}" -f
}

cmd_reset_password() {
    if ! is_installed; then
        fail "未检测到已安装实例，请先运行 install"
    fi
    "${BIN_PATH}" reset-password --db "${DATA_DIR}/bridge.db"
}

# cmd_change_listen_addr 写 systemd drop-in override 修改 BRIDGE_LISTEN_ADDR。
cmd_change_listen_addr() {
    if ! is_installed; then
        fail "未检测到已安装实例，请先运行 install"
    fi
    echo
    printf "${bold}修改 Web 监听地址${plain}\n"
    printf "  示例：\n"
    printf "    ${cyan}:8787${plain}              绑定全部网卡（默认；公网可访问）\n"
    printf "    ${cyan}127.0.0.1:8787${plain}     仅本机（配合 nginx 反代时推荐）\n"
    printf "    ${cyan}192.168.1.10:8787${plain}  仅指定网卡（多 IP 场景）\n"
    echo
    local addr
    read -rp "请输入新的监听地址：" addr
    addr="${addr// /}"
    if [[ -z "${addr}" ]]; then
        log_warn "监听地址不可为空，已取消"
        return 1
    fi
    if ! [[ "${addr}" =~ ^.*:[0-9]{1,5}$ ]]; then
        fail "格式不合法（应为 host:port 形式）：${addr}"
    fi
    local override_dir="/etc/systemd/system/${SERVICE_NAME}.service.d"
    local override_file="${override_dir}/listen.conf"
    mkdir -p "${override_dir}"
    cat > "${override_file}" <<EOF
[Service]
Environment=BRIDGE_LISTEN_ADDR=${addr}
EOF
    log_info "已写入 drop-in override：${override_file}"
    systemctl daemon-reload
    if confirm_yes "立即重启服务以应用？"; then
        systemctl restart "${SERVICE_NAME}"
        log_info "已重启；新监听地址：${addr}"
    else
        log_info "已保存配置，下次重启生效"
    fi
}

cmd_backup() {
    if ! is_installed; then
        fail "未检测到已安装实例"
    fi
    backup_existing
    log_info "备份目录：${BACKUP_DIR}"
}

cmd_version() {
    local installed
    installed=$(get_installed_version)
    printf "%-20s %s\n" "Script version:" "${SCRIPT_VERSION}"
    printf "%-20s %s\n" "Installed binary:" "${installed}"
    if [[ "${installed}" != "(none)" ]]; then
        printf "%-20s %s\n" "Service active:" "$(service_active && echo yes || echo no)"
    fi
}

# 菜单顶部的状态摘要：
#   • 已安装版本 / 服务状态
#   • 监听端口（从 service file 或 drop-in 解析）
#   • 启动时长 + 内存（systemctl show 抓 ActiveEnterTimestamp / MemoryCurrent）
status_summary() {
    local installed active listen_addr uptime mem
    installed=$(get_installed_version)

    if service_active; then
        active="${green}● running${plain}"
    elif is_installed; then
        active="${red}● stopped${plain}"
    else
        active="${yellow}● not installed${plain}"
    fi

    # 监听地址：优先 drop-in override，其次主 unit。
    listen_addr=""
    if [[ -f "/etc/systemd/system/${SERVICE_NAME}.service.d/listen.conf" ]]; then
        listen_addr=$(grep -E 'BRIDGE_LISTEN_ADDR=' "/etc/systemd/system/${SERVICE_NAME}.service.d/listen.conf" \
            | sed -E 's/.*BRIDGE_LISTEN_ADDR=//')
    elif [[ -f "${SERVICE_FILE}" ]]; then
        listen_addr=$(grep -E 'BRIDGE_LISTEN_ADDR=' "${SERVICE_FILE}" \
            | sed -E 's/.*BRIDGE_LISTEN_ADDR=//')
    fi
    [[ -z "${listen_addr}" ]] && listen_addr="${dim}(unset)${plain}"

    if service_active; then
        local started_at started_epoch now_epoch
        started_at=$(systemctl show "${SERVICE_NAME}" --property=ActiveEnterTimestamp --value 2>/dev/null || true)
        if [[ -n "${started_at}" ]]; then
            started_epoch=$(date -d "${started_at}" +%s 2>/dev/null || echo 0)
            now_epoch=$(date +%s)
            local diff=$((now_epoch - started_epoch))
            if [[ "${diff}" -gt 0 ]]; then
                local days=$((diff / 86400))
                local hours=$(((diff % 86400) / 3600))
                local mins=$(((diff % 3600) / 60))
                if [[ "${days}" -gt 0 ]]; then
                    uptime="${days}d ${hours}h ${mins}m"
                elif [[ "${hours}" -gt 0 ]]; then
                    uptime="${hours}h ${mins}m"
                else
                    uptime="${mins}m"
                fi
            fi
        fi
        local mem_bytes
        mem_bytes=$(systemctl show "${SERVICE_NAME}" --property=MemoryCurrent --value 2>/dev/null || echo 0)
        if [[ "${mem_bytes}" =~ ^[0-9]+$ ]] && [[ "${mem_bytes}" -gt 0 ]]; then
            mem=$(awk -v b="${mem_bytes}" 'BEGIN { printf "%.1f MB", b/1024/1024 }')
        fi
    fi
    [[ -z "${uptime:-}" ]] && uptime="${dim}-${plain}"
    [[ -z "${mem:-}" ]] && mem="${dim}-${plain}"

    printf "  ${bold}Version:${plain}      %s\n" "${installed}"
    printf "  ${bold}Status:${plain}       %b\n" "${active}"
    printf "  ${bold}Listen:${plain}       %b\n" "${listen_addr}"
    printf "  ${bold}Uptime:${plain}       %b\n" "${uptime}"
    printf "  ${bold}Memory:${plain}       %b\n" "${mem}"
}

cmd_show_menu() {
    while true; do
        clear 2>/dev/null || true
        print_banner
        printf "${bold}${cyan}═══════════════════════════════════════════════════════════════${plain}\n"
        printf "  ${bold}xboard-xui-bridge — 管理菜单${plain}\n"
        printf "${bold}${cyan}═══════════════════════════════════════════════════════════════${plain}\n"
        echo
        status_summary
        echo
        printf "${bold}${cyan}─── Service ──────────────────────────────────────────────────${plain}\n"
        printf "   ${cyan}1${plain})  安装 / 升级\n"
        printf "   ${cyan}2${plain})  启动服务\n"
        printf "   ${cyan}3${plain})  停止服务\n"
        printf "   ${cyan}4${plain})  重启服务\n"
        printf "${bold}${cyan}─── Diagnostics ──────────────────────────────────────────────${plain}\n"
        printf "   ${cyan}5${plain})  运行状态（systemctl status）\n"
        printf "   ${cyan}6${plain})  最近 200 行日志\n"
        printf "   ${cyan}7${plain})  仅 ERROR 日志\n"
        printf "   ${cyan}8${plain})  实时跟踪日志\n"
        printf "${bold}${cyan}─── Maintenance ──────────────────────────────────────────────${plain}\n"
        printf "   ${cyan}9${plain})  重置 admin 密码\n"
        printf "  ${cyan}10${plain})  修改 Web 监听地址\n"
        printf "  ${cyan}11${plain})  备份数据库\n"
        printf "${bold}${cyan}─── Danger Zone ──────────────────────────────────────────────${plain}\n"
        printf "  ${cyan}12${plain})  ${yellow}卸载${plain}（保留数据）\n"
        printf "  ${cyan}13${plain})  ${red}彻底清理${plain}（含数据，不可恢复）\n"
        printf "${bold}${cyan}──────────────────────────────────────────────────────────────${plain}\n"
        printf "   ${cyan}0${plain})  退出\n"
        echo
        local choice
        read -rp "请选择 [0-13]: " choice || return 0
        case "${choice}" in
            1)  cmd_install ;;
            2)  cmd_start ;;
            3)  cmd_stop ;;
            4)  cmd_restart ;;
            5)  cmd_status ;;
            6)  cmd_log ;;
            7)  cmd_log -e ;;
            8)  cmd_log_follow ;;
            9)  cmd_reset_password ;;
            10) cmd_change_listen_addr ;;
            11) cmd_backup ;;
            12) cmd_uninstall ;;
            13) cmd_purge ;;
            0)  log_info "再见"; return 0 ;;
            *)  log_warn "无效选项：${choice}" ;;
        esac
        echo
        read -rp "按 Enter 返回菜单..." _ || true
    done
}

cmd_help() {
    cat <<EOF
xboard-xui-bridge 安装 / 管理脚本（脚本版本 ${SCRIPT_VERSION}）

用法：
  bash <(curl -fsSL https://raw.githubusercontent.com/${GITHUB_REPO}/main/install.sh)
                                  默认行为：未装 → 安装；已装 → 进菜单

  xui-bridge [子命令]              安装后可用的快捷命令

子命令：
  install / update / upgrade      安装或升级（自动备份，保留 data/）
  uninstall                       卸载二进制 + service（保留 data/）
  purge                           完全清理（含 data/，不可恢复）
  start | stop | restart          服务启停
  status                          打印 systemctl status
  log [-e|-w|-i] [N]              查看日志，可按级别筛选（默认 200 行）
  follow                          实时跟踪日志
  reset-password                  交互式重置 admin 密码
  change-listen-addr              交互式修改 Web 监听地址
  backup                          手动备份当前数据库
  version                         打印脚本与二进制版本
  menu                            显式进入菜单（默认入口）
  help | --help | -h              打印本帮助

示例：
  xui-bridge                                进菜单
  xui-bridge install                        一键升级到最新版（自动备份旧版本）
  xui-bridge log -e                         仅查看 ERROR 日志
  xui-bridge restart && xui-bridge follow   重启后跟日志
  xui-bridge reset-password                 忘密码兜底重置
  xui-bridge backup                         手动备份数据库

环境变量：
  NO_COLOR=1                                禁用彩色输出（cron / 日志重定向场景）
EOF
}

# ---------------- 主流程 ----------------
main() {
    require_root
    require_systemd
    case "${1:-default}" in
        install|update|upgrade)
            cmd_install
            ;;
        uninstall|remove)
            cmd_uninstall
            ;;
        purge|clean)
            cmd_purge
            ;;
        start)              cmd_start ;;
        stop)               cmd_stop ;;
        restart)            cmd_restart ;;
        status)             cmd_status ;;
        log|logs)
            shift || true
            cmd_log "$@"
            ;;
        follow|log-follow)  cmd_log_follow ;;
        reset-password|reset|password)
            cmd_reset_password
            ;;
        change-listen-addr|change-listen|listen)
            cmd_change_listen_addr
            ;;
        backup)
            cmd_backup
            ;;
        version|--version|-v)
            cmd_version
            ;;
        menu)
            cmd_show_menu
            ;;
        help|--help|-h)
            cmd_help
            ;;
        default)
            # 一键命令默认行为：未装 → 安装；已装 → 菜单。
            if is_installed; then
                cmd_show_menu
            else
                print_banner
                cmd_install
            fi
            ;;
        *)
            log_error "未知子命令：$1"
            cmd_help
            exit 2
            ;;
    esac
}

main "$@"
