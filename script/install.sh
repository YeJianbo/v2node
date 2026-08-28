#!/bin/bash

red='\033[0;31m'
green='\033[0;32m'
yellow='\033[0;33m'
plain='\033[0m'

cur_dir=$(pwd)
REPO_SLUG="YeJianbo/v2node"
UPSTREAM_REPO_SLUG="wyx2685/v2node"
SCRIPT_BRANCH="main"
SCRIPT_BASE_URL="https://raw.githubusercontent.com/${REPO_SLUG}/${SCRIPT_BRANCH}/script"
AGENT_BIN="/usr/local/v2node/v2node"
CONFIG_DIR="/etc/.buncloud-agent"
LEGACY_CONFIG_DIR="/etc/v2node"
CONFIG_FILE="${V2NODE_CONFIG_FILE:-${CONFIG_DIR}/config.enc.json}"
PLAIN_CONFIG_FILE="${V2NODE_CONFIG_PLAIN_FILE:-${CONFIG_DIR}/config.json}"
CONFIG_KEY_FILE="${V2NODE_CONFIG_KEY_FILE:-${CONFIG_DIR}/config.key}"
PROBE_STATE_FILE="${V2NODE_PROBE_STATE_FILE:-${CONFIG_DIR}/state.json}"
RUNTIME_ENV_FILE="${V2NODE_RUNTIME_ENV_FILE:-${CONFIG_DIR}/runtime.env}"
CONFIG_ENCRYPTION_ENABLED=1

ensure_private_dir() {
    local dir="$1"
    mkdir -p "$dir" >/dev/null 2>&1
    chmod 700 "$dir" >/dev/null 2>&1 || true
}

ensure_config_key() {
    ensure_private_dir "$CONFIG_DIR"
    if [[ "${CONFIG_ENCRYPTION_ENABLED:-1}" != "1" ]]; then
        printf ''
        return 0
    fi
    if [[ -s "$CONFIG_KEY_FILE" ]]; then
        tr -d '\r\n' < "$CONFIG_KEY_FILE"
        return 0
    fi

    local key
    if ! key=$("$AGENT_BIN" config keygen 2>/dev/null); then
        return 1
    fi
    printf '%s' "$key" > "$CONFIG_KEY_FILE"
    chmod 600 "$CONFIG_KEY_FILE" >/dev/null 2>&1 || true
    printf '%s' "$key"
}

decrypt_config_to_file() {
    local output_file="$1"
    local key="$2"

    if [[ "${CONFIG_ENCRYPTION_ENABLED:-1}" != "1" ]]; then
        if [[ -f "$CONFIG_FILE" ]]; then
            cp "$CONFIG_FILE" "$output_file"
            return 0
        fi
        return 1
    fi

    if [[ -f "$CONFIG_FILE" ]]; then
        "$AGENT_BIN" config decrypt --in "$CONFIG_FILE" --out "$output_file" --key "$key" >/dev/null 2>&1
        return $?
    fi

    if [[ -f "$PLAIN_CONFIG_FILE" ]]; then
        cp "$PLAIN_CONFIG_FILE" "$output_file"
        return 0
    fi

    if [[ -f "${LEGACY_CONFIG_DIR}/config.json" ]]; then
        cp "${LEGACY_CONFIG_DIR}/config.json" "$output_file"
        return 0
    fi

    return 1
}

encrypt_config_from_file() {
    local input_file="$1"
    local key="$2"

    ensure_private_dir "$CONFIG_DIR"
    if [[ "${CONFIG_ENCRYPTION_ENABLED:-1}" != "1" ]]; then
        cp "$input_file" "$CONFIG_FILE"
        chmod 600 "$CONFIG_FILE" >/dev/null 2>&1 || true
        return 0
    fi

    if ! "$AGENT_BIN" config encrypt --in "$input_file" --out "$CONFIG_FILE" --key "$key" >/dev/null 2>&1; then
        return 1
    fi

    chmod 600 "$CONFIG_FILE" >/dev/null 2>&1 || true
    rm -f "$PLAIN_CONFIG_FILE" "${LEGACY_CONFIG_DIR}/config.json"
    return 0
}

seed_plain_config_file() {
    local output_file="$1"
    cat > "$output_file" <<EOF
{
    "Log": {
        "Level": "warning",
        "Output": "",
        "Access": "none"
    },
    "Nodes": []
}
EOF
}

has_existing_config() {
    [[ -f "$CONFIG_FILE" || -f "$PLAIN_CONFIG_FILE" || -f "${LEGACY_CONFIG_DIR}/config.json" ]]
}

migrate_existing_config() {
    if [[ -f "$CONFIG_FILE" ]]; then
        return 0
    fi

    local source_file=""
    local config_key

    if [[ -f "$PLAIN_CONFIG_FILE" ]]; then
        source_file="$PLAIN_CONFIG_FILE"
    elif [[ -f "${LEGACY_CONFIG_DIR}/config.json" ]]; then
        source_file="${LEGACY_CONFIG_DIR}/config.json"
    else
        return 0
    fi

    if ! config_key=$(ensure_config_key); then
        echo -e "${red}读取配置加密密钥失败，已停止启动以保护现有节点配置${plain}"
        return 1
    fi

    if ! encrypt_config_from_file "$source_file" "$config_key"; then
        echo -e "${red}迁移旧节点配置失败，已停止启动以保护现有节点配置${plain}"
        return 1
    fi

    echo -e "${green}已将旧节点配置迁移到 ${CONFIG_FILE}${plain}"
}

refresh_runtime_env_file() {
    local key

    ensure_private_dir "$CONFIG_DIR"
    if [[ "${CONFIG_ENCRYPTION_ENABLED:-1}" != "1" ]]; then
        cat > "$RUNTIME_ENV_FILE" <<EOF
export BUNCLOUD_CONFIG_PATH='$(escape_env_value "$CONFIG_FILE")'
export V2NODE_CONFIG_PATH='$(escape_env_value "$CONFIG_FILE")'
export V2NODE_CONFIG_FILE='$(escape_env_value "$CONFIG_FILE")'
export V2NODE_PROBE_STATE_FILE='$(escape_env_value "$PROBE_STATE_FILE")'
export V2NODE_CONFIG_PLAIN='true'
EOF
        chmod 600 "$RUNTIME_ENV_FILE" >/dev/null 2>&1 || true
        return 0
    fi
    if [[ ! -s "$CONFIG_KEY_FILE" ]]; then
        return 1
    fi

    key=$(tr -d '\r\n' < "$CONFIG_KEY_FILE")
    cat > "$RUNTIME_ENV_FILE" <<EOF
export BUNCLOUD_CONFIG_PATH='$(escape_env_value "$CONFIG_FILE")'
export V2NODE_CONFIG_PATH='$(escape_env_value "$CONFIG_FILE")'
export BUNCLOUD_CONFIG_KEY='$(escape_env_value "$key")'
export V2NODE_CONFIG_KEY='$(escape_env_value "$key")'
export V2NODE_CONFIG_FILE='$(escape_env_value "$CONFIG_FILE")'
export V2NODE_CONFIG_KEY_FILE='$(escape_env_value "$CONFIG_KEY_FILE")'
export V2NODE_PROBE_STATE_FILE='$(escape_env_value "$PROBE_STATE_FILE")'
export V2NODE_CONFIG_PLAIN='$(if [[ "${CONFIG_ENCRYPTION_ENABLED:-1}" == "1" ]]; then printf false; else printf true; fi)'
EOF
    chmod 600 "$RUNTIME_ENV_FILE" >/dev/null 2>&1 || true
}

# check root
[[ $EUID -ne 0 ]] && echo -e "${red}错误：${plain} 必须使用root用户运行此脚本！\n" && exit 1

# check os
if [[ -f /etc/redhat-release ]]; then
    release="centos"
elif cat /etc/issue | grep -Eqi "alpine"; then
    release="alpine"
elif cat /etc/issue | grep -Eqi "debian"; then
    release="debian"
elif cat /etc/issue | grep -Eqi "ubuntu"; then
    release="ubuntu"
elif cat /etc/issue | grep -Eqi "centos|red hat|redhat|rocky|alma|oracle linux"; then
    release="centos"
elif cat /proc/version | grep -Eqi "debian"; then
    release="debian"
elif cat /proc/version | grep -Eqi "ubuntu"; then
    release="ubuntu"
elif cat /proc/version | grep -Eqi "centos|red hat|redhat|rocky|alma|oracle linux"; then
    release="centos"
elif cat /proc/version | grep -Eqi "arch"; then
    release="arch"
else
    echo -e "${red}未检测到系统版本，请联系脚本作者！${plain}\n" && exit 1
fi

########################
# 参数解析
########################
VERSION_ARG=""
API_HOST_ARG=""
NODE_ID_ARG=""
API_KEY_ARG=""
INSTALL_MODE_ARG="node"
INSTALL_MODE_EXPLICIT=0
PANEL_URL_ARG=""
MACHINE_TOKEN_ARG=""
MACHINE_ID_ARG=""
ENROLL_TOKEN_ARG=""
MACHINE_NAME_ARG=""

parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --api-host)
                API_HOST_ARG="$2"; shift 2 ;;
            --node-id)
                NODE_ID_ARG="$2"; shift 2 ;;
            --api-key)
                API_KEY_ARG="$2"; shift 2 ;;
            --mode)
                INSTALL_MODE_ARG="$2"; INSTALL_MODE_EXPLICIT=1; shift 2 ;;
            --panel)
                PANEL_URL_ARG="$2"; shift 2 ;;
            --token)
                MACHINE_TOKEN_ARG="$2"; shift 2 ;;
            --machine-id)
                MACHINE_ID_ARG="$2"; shift 2 ;;
            --enroll-token)
                ENROLL_TOKEN_ARG="$2"; shift 2 ;;
            --machine-name)
                MACHINE_NAME_ARG="$2"; shift 2 ;;
            -h|--help)
                echo "用法: $0 [版本号] [--api-host URL] [--node-id ID] [--api-key KEY] [--mode node|machine] [--panel URL] [--token TOKEN] [--machine-id ID] [--enroll-token TOKEN] [--machine-name NAME]"
                exit 0 ;;
            --*)
                echo "未知参数: $1"; exit 1 ;;
            *)
                # 兼容第一个位置参数作为版本号
                if [[ -z "$VERSION_ARG" ]]; then
                    VERSION_ARG="$1"; shift
                else
                    shift
                fi ;;
        esac
    done
}

restore_existing_machine_mode() {
    if [[ "$INSTALL_MODE_EXPLICIT" == "1" ]]; then
        return 0
    fi

    local state_file=""
    if [[ -f "${CONFIG_DIR}/state.json" ]]; then
        state_file="${CONFIG_DIR}/state.json"
    elif [[ -f "${CONFIG_DIR}/probe-state.json" ]]; then
        state_file="${CONFIG_DIR}/probe-state.json"
    fi

    if [[ -n "$state_file" ]] && jq -e 'type == "object"' "$state_file" >/dev/null 2>&1; then
        PANEL_URL_ARG=$(jq -r '.panel_url // ""' "$state_file")
        MACHINE_TOKEN_ARG=$(jq -r '.machine_token // ""' "$state_file")
        MACHINE_ID_ARG=$(jq -r '.machine_id // ""' "$state_file")
    elif [[ -f "${LEGACY_CONFIG_DIR}/probe.env" ]]; then
        # shellcheck disable=SC1090
        source "${LEGACY_CONFIG_DIR}/probe.env"
        PANEL_URL_ARG="${PANEL_URL:-}"
        MACHINE_TOKEN_ARG="${MACHINE_TOKEN:-}"
        MACHINE_ID_ARG="${MACHINE_ID:-}"
    fi

    if [[ -n "$PANEL_URL_ARG" && -n "$MACHINE_TOKEN_ARG" && "$MACHINE_ID_ARG" =~ ^[0-9]+$ ]] && (( MACHINE_ID_ARG > 0 )); then
        INSTALL_MODE_ARG="machine"
        echo -e "${green}检测到现有机器接入配置，本次升级将自动迁移到原生 Ravel${plain}"
    fi
}

arch=$(uname -m)

if [[ $arch == "x86_64" || $arch == "x64" || $arch == "amd64" ]]; then
    arch="64"
elif [[ $arch == "aarch64" || $arch == "arm64" ]]; then
    arch="arm64-v8a"
elif [[ $arch == "s390x" ]]; then
    arch="s390x"
else
    arch="64"
    echo -e "${red}检测架构失败，使用默认架构: ${arch}${plain}"
fi

if [ "$(getconf WORD_BIT)" != '32' ] && [ "$(getconf LONG_BIT)" != '64' ] ; then
    echo "本软件不支持 32 位系统(x86)，请使用 64 位系统(x86_64)，如果检测有误，请联系作者"
    exit 2
fi

# os version
if [[ -f /etc/os-release ]]; then
    os_version=$(awk -F'[= ."]' '/VERSION_ID/{print $3}' /etc/os-release)
fi
if [[ -z "$os_version" && -f /etc/lsb-release ]]; then
    os_version=$(awk -F'[= ."]+' '/DISTRIB_RELEASE/{print $2}' /etc/lsb-release)
fi

if [[ x"${release}" == x"centos" ]]; then
    if [[ ${os_version} -le 6 ]]; then
        echo -e "${red}请使用 CentOS 7 或更高版本的系统！${plain}\n" && exit 1
    fi
    if [[ ${os_version} -eq 7 ]]; then
        echo -e "${red}注意： CentOS 7 无法使用hysteria1/2协议！${plain}\n"
    fi
elif [[ x"${release}" == x"ubuntu" ]]; then
    if [[ ${os_version} -lt 16 ]]; then
        echo -e "${red}请使用 Ubuntu 16 或更高版本的系统！${plain}\n" && exit 1
    fi
elif [[ x"${release}" == x"debian" ]]; then
    if [[ ${os_version} -lt 8 ]]; then
        echo -e "${red}请使用 Debian 8 或更高版本的系统！${plain}\n" && exit 1
    fi
fi

install_base() {
    # 优化版本：批量检查和安装包，减少系统调用
    need_install_apt() {
        local packages=("$@")
        local missing=()
        
        # 批量检查已安装的包
        local installed_list=$(dpkg-query -W -f='${Package}\n' 2>/dev/null | sort)
        
        for p in "${packages[@]}"; do
            if ! echo "$installed_list" | grep -q "^${p}$"; then
                missing+=("$p")
            fi
        done
        
        if [[ ${#missing[@]} -gt 0 ]]; then
            echo "安装缺失的包: ${missing[*]}"
            apt-get update -y >/dev/null 2>&1
            DEBIAN_FRONTEND=noninteractive apt-get install -y "${missing[@]}" >/dev/null 2>&1
        fi
    }

    need_install_yum() {
        local packages=("$@")
        local missing=()
        
        # 批量检查已安装的包
        local installed_list=$(rpm -qa --qf '%{NAME}\n' 2>/dev/null | sort)
        
        for p in "${packages[@]}"; do
            if ! echo "$installed_list" | grep -q "^${p}$"; then
                missing+=("$p")
            fi
        done
        
        if [[ ${#missing[@]} -gt 0 ]]; then
            echo "安装缺失的包: ${missing[*]}"
            yum install -y "${missing[@]}" >/dev/null 2>&1
        fi
    }

    need_install_apk() {
        local packages=("$@")
        local missing=()
        
        # 批量检查已安装的包
        local installed_list=$(apk info 2>/dev/null | sort)
        
        for p in "${packages[@]}"; do
            if ! echo "$installed_list" | grep -q "^${p}$"; then
                missing+=("$p")
            fi
        done
        
        if [[ ${#missing[@]} -gt 0 ]]; then
            echo "安装缺失的包: ${missing[*]}"
            apk add --no-cache "${missing[@]}" >/dev/null 2>&1
        fi
    }

    # 一次性安装所有必需的包
    if [[ x"${release}" == x"centos" ]]; then
        # 检查并安装 epel-release
        if ! rpm -q epel-release >/dev/null 2>&1; then
            echo "安装 EPEL 源..."
            yum install -y epel-release >/dev/null 2>&1
        fi
        need_install_yum wget curl unzip tar cronie socat ca-certificates pv jq openssl iputils
        update-ca-trust force-enable >/dev/null 2>&1 || true
    elif [[ x"${release}" == x"alpine" ]]; then
        need_install_apk wget curl unzip tar socat ca-certificates pv jq openssl iputils
        update-ca-certificates >/dev/null 2>&1 || true
    elif [[ x"${release}" == x"debian" ]]; then
        need_install_apt wget curl unzip tar cron socat ca-certificates pv jq openssl iputils-ping
        update-ca-certificates >/dev/null 2>&1 || true
    elif [[ x"${release}" == x"ubuntu" ]]; then
        need_install_apt wget curl unzip tar cron socat ca-certificates pv jq openssl iputils-ping
        update-ca-certificates >/dev/null 2>&1 || true
    elif [[ x"${release}" == x"arch" ]]; then
        echo "更新包数据库..."
        pacman -Sy --noconfirm >/dev/null 2>&1
        # --needed 会跳过已安装的包，非常高效
        echo "安装必需的包..."
        pacman -S --noconfirm --needed wget curl unzip tar cronie socat ca-certificates pv jq openssl iputils >/dev/null 2>&1
    fi
}

# 0: running, 1: not running, 2: not installed
check_status() {
    if [[ ! -f /usr/local/v2node/v2node ]]; then
        return 2
    fi
    if [[ x"${release}" == x"alpine" ]]; then
        temp=$(service v2node status | awk '{print $3}')
        if [[ x"${temp}" == x"started" ]]; then
            return 0
        else
            return 1
        fi
    else
        temp=$(systemctl status v2node | grep Active | awk '{print $3}' | cut -d "(" -f2 | cut -d ")" -f1)
        if [[ x"${temp}" == x"running" ]]; then
            return 0
        else
            return 1
        fi
    fi
}

generate_v2node_config() {
        local api_host="$1"
        local node_id="$2"
        local api_key="$3"
        local config_file="$CONFIG_FILE"
        local action="生成"
        local plain_config_file
        local config_key

        if ! [[ "$node_id" =~ ^[0-9]+$ ]]; then
            echo -e "${red}节点ID必须为整数${plain}"
            return 1
        fi

        if ! config_key=$(ensure_config_key); then
            echo -e "${red}生成配置加密密钥失败${plain}"
            return 1
        fi

        plain_config_file=$(mktemp)
        if decrypt_config_to_file "$plain_config_file" "$config_key"; then
            if ! command -v jq >/dev/null 2>&1; then
                echo -e "${red}当前系统缺少 jq，无法追加节点，请先执行 v2node update 或手动安装 jq${plain}"
                rm -f "$plain_config_file"
                return 1
            fi
            if ! jq empty "$plain_config_file" >/dev/null 2>&1; then
                echo -e "${red}现有配置文件不是合法 JSON，已停止追加节点，请先检查 ${config_file}${plain}"
                rm -f "$plain_config_file"
                return 1
            fi
            if ! jq \
                --arg api_host "$api_host" \
                --argjson node_id "$node_id" \
                --arg api_key "$api_key" \
                '
                def normalized_node_id:
                    ((.NodeID // .node_id // 0) | tonumber? // 0);

                {
                    "ApiHost": $api_host,
                    "NodeID": $node_id,
                    "ApiKey": $api_key,
                    "Timeout": 15
                } as $newNode
                | .Nodes = (
                    ((if (.Nodes | type) == "array" then .Nodes else [] end)
                    | map(select((normalized_node_id == $node_id) | not)))
                    + [$newNode]
                )
                ' \
                "$plain_config_file" > "${plain_config_file}.next"; then
                echo -e "${red}追加节点到配置文件失败${plain}"
                rm -f "$plain_config_file" "${plain_config_file}.next"
                return 1
            fi
            mv "${plain_config_file}.next" "$plain_config_file"
            action="追加"
        else
            cat > "$plain_config_file" <<EOF
{
    "Log": {
        "Level": "warning",
        "Output": "",
        "Access": "none"
    },
    "Nodes": [
        {
            "ApiHost": "${api_host}",
            "NodeID": ${node_id},
            "ApiKey": "${api_key}",
            "Timeout": 15
        }
    ]
}
EOF
        fi

        if ! encrypt_config_from_file "$plain_config_file" "$config_key"; then
            echo -e "${red}写入加密配置失败${plain}"
            rm -f "$plain_config_file"
            return 1
        fi
        rm -f "$plain_config_file"
        echo -e "${green}V2node 配置文件${action}完成,正在重新启动服务${plain}"
        if [[ x"${release}" == x"alpine" ]]; then
            service v2node restart
        else
            systemctl restart v2node
        fi
        sleep 2
        check_status
        echo -e ""
        if [[ $? == 0 ]]; then
            echo -e "${green}v2node 重启成功${plain}"
        else
            echo -e "${red}v2node 可能启动失败，请使用 v2node log 查看日志信息${plain}"
        fi
}

get_latest_release_tag() {
    local repo_slug="$1"
    curl -fsLs "https://api.github.com/repos/${repo_slug}/releases/latest" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/'
}

download_release_zip() {
    local repo_slug="$1"
    local version="$2"
    local output_file="$3"
    local url="https://github.com/${repo_slug}/releases/download/${version}/v2node-linux-${arch}.zip"

    curl -fLsS "$url" | pv -s 30M -W -N "下载进度" > "$output_file"
}

download_release_zip_with_retry() {
    local repo_slug="$1"
    local version="$2"
    local output_file="$3"
    local attempt

    for attempt in 1 2 3 4 5 6; do
        if download_release_zip "$repo_slug" "$version" "$output_file"; then
            return 0
        fi
        rm -f "$output_file"
        if [[ "$attempt" -lt 6 ]]; then
            echo -e "${yellow}Release 已创建但当前架构产物尚未就绪，10 秒后重试 (${attempt}/6)${plain}"
            sleep 10
        fi
    done

    return 1
}

stop_existing_v2node_processes() {
    if [[ x"${release}" == x"alpine" ]]; then
        service v2node-probe stop >/dev/null 2>&1 || true
        service v2node stop >/dev/null 2>&1 || true
		service ravel stop >/dev/null 2>&1 || true
        pkill -f '/usr/local/v2node/v2node-probe.sh daemon' >/dev/null 2>&1 || true
        pkill -f '/usr/local/v2node/v2node server' >/dev/null 2>&1 || true
        rm -f /run/v2node.pid /run/v2node-probe.pid
        rm -rf /run/v2node-probe.lock
        return
    fi

    systemctl stop v2node-probe >/dev/null 2>&1 || true
    systemctl stop v2node >/dev/null 2>&1 || true
    systemctl stop ravel >/dev/null 2>&1 || true
}

install_v2node() {
    local version_param="$1"
    local release_repo="$REPO_SLUG"
    local install_stage
    local release_zip
    install_stage=$(mktemp -d)
    release_zip="${install_stage}/v2node-linux.zip"
    cd "$install_stage"

    if  [[ -z "$version_param" ]] ; then
        last_version=$(get_latest_release_tag "$release_repo")
        if [[ ! -n "$last_version" ]]; then
            echo -e "${yellow}你的 fork 暂无可用 release，回退到上游 release 源${plain}"
            release_repo="$UPSTREAM_REPO_SLUG"
            last_version=$(get_latest_release_tag "$release_repo")
        fi
        if [[ ! -n "$last_version" ]]; then
            echo -e "${red}检测 v2node 版本失败，可能是超出 Github API 限制，请稍后再试，或手动指定 v2node 版本安装${plain}"
            exit 1
        fi
        echo -e "${green}检测到最新版本：${last_version}，开始安装...${plain}"
        if ! download_release_zip_with_retry "$release_repo" "$last_version" "$release_zip"; then
            rm -rf "$install_stage"
            echo -e "${red}下载 v2node ${last_version} 失败；该版本产物可能仍在构建，请稍后重试${plain}"
            exit 1
        fi
    else
        last_version=$version_param
        if ! download_release_zip_with_retry "$release_repo" "$last_version" "$release_zip"; then
            rm -rf "$install_stage"
            echo -e "${red}下载 v2node $1 失败，请确认 fork 中存在该版本及当前架构产物${plain}"
            exit 1
        fi
    fi

    unzip "$release_zip"
    rm "$release_zip" -f
    if [[ ! -f v2node || ! -f geoip.dat || ! -f geosite.dat ]]; then
        rm -rf "$install_stage"
        echo -e "${red}Release 产物不完整，保留现有服务不变${plain}"
        exit 1
    fi
    chmod +x v2node
    stop_existing_v2node_processes
    rm -rf /usr/local/v2node/
    mkdir -p /usr/local/v2node/
    cp -a "${install_stage}/." /usr/local/v2node/
    rm -rf "$install_stage"
    cd /usr/local/v2node/
    local config_key_probe=""
    config_key_probe=$("$AGENT_BIN" config keygen 2>/dev/null || true)
    if ! [[ "$config_key_probe" =~ ^[A-Za-z0-9+/_=-]{20,}$ ]]; then
        CONFIG_ENCRYPTION_ENABLED=0
        CONFIG_FILE="${V2NODE_PLAIN_CONFIG_FILE:-${LEGACY_CONFIG_DIR}/config.json}"
        PLAIN_CONFIG_FILE="$CONFIG_FILE"
        CONFIG_KEY_FILE="${LEGACY_CONFIG_DIR}/config.key"
        echo -e "${yellow}当前 v2node 二进制不支持加密配置，回退到兼容明文配置模式${plain}"
    fi
    mkdir /etc/v2node/ -p
    cp geoip.dat /etc/v2node/
    cp geosite.dat /etc/v2node/
    ensure_private_dir "$CONFIG_DIR"
    if ! ensure_config_key >/dev/null; then
        echo -e "${red}初始化配置加密密钥失败${plain}"
        exit 1
    fi
    if ! refresh_runtime_env_file; then
        echo -e "${red}写入运行时环境失败${plain}"
        exit 1
    fi
    if ! migrate_existing_config; then
        exit 1
    fi
    cat > /usr/local/v2node/run.sh <<EOF
#!/bin/sh
set -eu
if [ -f "${RUNTIME_ENV_FILE}" ]; then
  . "${RUNTIME_ENV_FILE}"
fi
exec /usr/local/v2node/v2node server -c "\${BUNCLOUD_CONFIG_PATH:-${CONFIG_FILE}}" "\$@"
EOF
    chmod +x /usr/local/v2node/run.sh
    install -d -m 0755 /usr/local/ravel
    install -m 0755 /usr/local/v2node/v2node /usr/local/ravel/ravel
    if ! /usr/local/ravel/ravel ravel --help >/dev/null 2>&1; then
        echo -e "${red}当前 Release 尚未包含原生 Ravel，请安装支持 ravel 子命令的新版本${plain}"
        exit 1
    fi
    if [[ x"${release}" == x"alpine" ]]; then
        rm /etc/init.d/v2node -f
        cat <<EOF > /etc/init.d/v2node
#!/sbin/openrc-run

name="v2node"
description="v2node"

command="/usr/local/v2node/run.sh"
command_args=""
command_user="root"

pidfile="/run/v2node.pid"
command_background="yes"

start_pre() {
        if [ -f "\$pidfile" ] && ! kill -0 "\$(cat "\$pidfile" 2>/dev/null)" 2>/dev/null; then
                rm -f "\$pidfile"
        fi
}

stop_post() {
        rm -f "\$pidfile"
}

depend() {
        need net
}
EOF
        chmod +x /etc/init.d/v2node
        rc-update add v2node default
        echo -e "${green}v2node ${last_version}${plain} 安装完成，已设置开机自启"
    else
        rm /etc/systemd/system/v2node.service -f
        cat <<EOF > /etc/systemd/system/v2node.service
[Unit]
Description=v2node Service
After=network.target nss-lookup.target
Wants=network.target

[Service]
User=root
Group=root
Type=simple
LimitAS=infinity
LimitRSS=infinity
LimitCORE=infinity
LimitNOFILE=999999
WorkingDirectory=/usr/local/v2node/
ExecStart=/usr/local/v2node/run.sh
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF
        systemctl daemon-reload
        systemctl stop v2node
        systemctl enable v2node
        echo -e "${green}v2node ${last_version}${plain} 安装完成，已设置开机自启"
    fi

    disable_machine_probe

    if [[ "$INSTALL_MODE_ARG" == "machine" ]]; then
        setup_machine_probe
        first_install=false
    elif ! has_existing_config; then
        # 如果通过 CLI 传入了完整参数，则直接生成配置并跳过交互
        if [[ -n "$API_HOST_ARG" && -n "$NODE_ID_ARG" && -n "$API_KEY_ARG" ]]; then
            generate_v2node_config "$API_HOST_ARG" "$NODE_ID_ARG" "$API_KEY_ARG"
            echo -e "${green}已根据参数生成加密配置 ${CONFIG_FILE}${plain}"
            first_install=false
        else
            local plain_config_file
            local config_key
            config_key=$(ensure_config_key)
            plain_config_file=$(mktemp)
            seed_plain_config_file "$plain_config_file"
            if ! encrypt_config_from_file "$plain_config_file" "$config_key"; then
                rm -f "$plain_config_file"
                echo -e "${red}初始化加密配置失败${plain}"
                exit 1
            fi
            rm -f "$plain_config_file"
            first_install=true
        fi
    else
        if [[ -n "$API_HOST_ARG" && -n "$NODE_ID_ARG" && -n "$API_KEY_ARG" ]]; then
            if ! generate_v2node_config "$API_HOST_ARG" "$NODE_ID_ARG" "$API_KEY_ARG"; then
                exit 1
            fi
            echo -e "${green}检测到现有安装，已向加密配置追加一个节点${plain}"
        else
            if [[ x"${release}" == x"alpine" ]]; then
                service v2node start
            else
                systemctl start v2node
            fi
            sleep 2
            check_status
            echo -e ""
            if [[ $? == 0 ]]; then
                echo -e "${green}v2node 重启成功${plain}"
            else
                echo -e "${red}v2node 可能启动失败，请使用 v2node log 查看日志信息${plain}"
            fi
        fi
        first_install=false
    fi


    local manager_script_tmp
    manager_script_tmp=$(mktemp)
    if ! curl -fsSL "${SCRIPT_BASE_URL}/v2node.sh" -o "$manager_script_tmp" \
        || ! bash -n "$manager_script_tmp"; then
        rm -f "$manager_script_tmp"
        echo -e "${red}下载或校验 Ravel 管理脚本失败，保留现有命令不变${plain}"
        exit 1
    fi
    rm -f /usr/bin/ravel /usr/bin/v2node /usr/bin/v2bx
    install -m 0755 "$manager_script_tmp" /usr/bin/ravel
    rm -f "$manager_script_tmp"
    ln -sf /usr/bin/ravel /usr/bin/v2node
    ln -sf /usr/bin/ravel /usr/bin/v2bx

    cd $cur_dir
    rm -f install.sh
    echo "------------------------------------------"
    echo -e "管理脚本使用方法: "
    echo "------------------------------------------"
    echo "ravel               - 显示 Ravel 管理菜单"
    echo "ravel start         - 启动 Ravel"
    echo "ravel stop          - 停止 Ravel"
    echo "ravel restart       - 重启 Ravel"
    echo "ravel status        - 查看 Ravel 状态"
    echo "ravel enable        - 设置 Ravel 开机自启"
    echo "ravel disable       - 取消 Ravel 开机自启"
    echo "ravel log           - 查看 Ravel 日志"
    echo "ravel version       - 查看 Ravel 版本"
    echo "ravel update        - 更新 Ravel"
    echo "v2node / v2bx       - 兼容入口，同样进入 Ravel 管理"
    echo "------------------------------------------"
    curl -fsS --max-time 10 "https://api.v-50.me/counter" || true

    if [[ "$INSTALL_MODE_ARG" == "machine" ]]; then
        echo -e "${green}已启用 Ravel 原生云控模式。后续在面板分配节点后会自动同步到本机。${plain}"
    elif [[ $first_install == true ]]; then
        read -rp "检测到你为第一次安装 Ravel，是否自动生成加密节点配置 ${CONFIG_FILE}？(y/n): " if_generate
        if [[ "$if_generate" =~ ^[Yy]$ ]]; then
            # 交互式收集参数，提供示例默认值
            read -rp "面板API地址[格式: https://example.com/]: " api_host
            api_host=${api_host:-https://example.com/}
            read -rp "节点ID: " node_id
            node_id=${node_id:-1}
            read -rp "节点通讯密钥: " api_key

            # 生成配置文件（覆盖可能从包中复制的模板）
            generate_v2node_config "$api_host" "$node_id" "$api_key"
        else
            echo "${green}已跳过自动生成配置。如需后续生成，可执行: v2node generate${plain}"
        fi
    fi
}

disable_machine_probe() {
    rm -f "$PROBE_STATE_FILE" "${LEGACY_CONFIG_DIR}/probe.env"
    if [[ x"${release}" == x"alpine" ]]; then
        if [[ -f /etc/init.d/ravel ]]; then
            service ravel stop >/dev/null 2>&1 || true
            rc-update del ravel default >/dev/null 2>&1 || true
            rm -f /etc/init.d/ravel
        fi
        if [[ -f /etc/init.d/ravel-probe ]]; then
            service ravel-probe stop >/dev/null 2>&1 || true
            rc-update del ravel-probe default >/dev/null 2>&1 || true
            rm -f /etc/init.d/ravel-probe
        fi
        if [[ -f /etc/init.d/v2node-probe ]]; then
            service v2node-probe stop >/dev/null 2>&1 || true
            rc-update del v2node-probe default >/dev/null 2>&1 || true
            rm -f /etc/init.d/v2node-probe
        fi
    else
        if [[ -f /etc/systemd/system/ravel.service ]]; then
            systemctl stop ravel >/dev/null 2>&1 || true
            systemctl disable ravel >/dev/null 2>&1 || true
            rm -f /etc/systemd/system/ravel.service
        fi
        if [[ -f /etc/systemd/system/ravel-probe.service ]]; then
            systemctl stop ravel-probe >/dev/null 2>&1 || true
            systemctl disable ravel-probe >/dev/null 2>&1 || true
            rm -f /etc/systemd/system/ravel-probe.service
        fi
        if [[ -f /etc/systemd/system/v2node-probe.service ]]; then
            systemctl stop v2node-probe >/dev/null 2>&1 || true
            systemctl disable v2node-probe >/dev/null 2>&1 || true
            rm -f /etc/systemd/system/v2node-probe.service
            systemctl daemon-reload >/dev/null 2>&1 || true
        fi
    fi
}

escape_env_value() {
    printf "%s" "$1" | sed "s/'/'\"'\"'/g"
}

detect_machine_name() {
    if [[ -n "$MACHINE_NAME_ARG" ]]; then
        printf "%s" "$MACHINE_NAME_ARG"
        return
    fi

    hostname -f 2>/dev/null || hostname 2>/dev/null || printf "v2node-probe"
}

detect_primary_ip() {
    local ip
    ip=$(hostname -I 2>/dev/null | tr ' ' '\n' | awk '
        /^127\./ { next }
        /^169\.254\./ { next }
        /^172\.(1[6-9]|2[0-9]|3[0-1])\./ { next }
        /^198\.18\./ { next }
        /^198\.19\./ { next }
        /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/ { print; exit }
    ')
    if [[ -n "$ip" ]]; then
        printf "%s" "$ip"
        return
    fi

    ip=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i=1; i<=NF; i++) if ($i == "src") {print $(i+1); exit}}')
    if [[ -n "$ip" ]]; then
        printf "%s" "$ip"
        return
    fi

    hostname -I 2>/dev/null | awk '{print $1}'
}

enroll_machine_probe() {
    if [[ -z "$ENROLL_TOKEN_ARG" ]]; then
        return 0
    fi

    local panel_url="${PANEL_URL_ARG%/}"
    local machine_name
    local machine_host
    local body
    local response
    local api_token
    local machine_id

    if [[ -z "$panel_url" ]]; then
        echo -e "${red}探针通用接入缺少 --panel 参数${plain}"
        exit 1
    fi

    machine_name=$(detect_machine_name)
    machine_host=$(detect_primary_ip)
    body=$(jq -nc \
        --arg enroll_token "$ENROLL_TOKEN_ARG" \
        --arg name "$machine_name" \
        --arg host "$machine_host" \
        --arg machine_id "$MACHINE_ID_ARG" \
        '{
            enroll_token:$enroll_token,
            name:$name,
            host:$host
        } + (if ($machine_id | length) > 0 then {machine_id:($machine_id | tonumber)} else {} end)')

    echo -e "${green}正在向面板注册探针: ${panel_url}${plain}"
    if ! response=$(curl -fsSL --connect-timeout 8 --max-time 20 \
        -H "Content-Type: application/json" \
        -H "Connection: close" \
        --data "$body" \
        "${panel_url}/api/v1/server/machine/enroll"); then
        echo -e "${red}探针注册失败，请检查面板地址和通用接入令牌${plain}"
        exit 1
    fi

    machine_id=$(printf "%s" "$response" | jq -r '.data.machine_id // .data.id // ""')
    api_token=$(printf "%s" "$response" | jq -r '.data.api_token // .data.token // ""')
    if [[ -z "$machine_id" || -z "$api_token" || "$machine_id" == "null" || "$api_token" == "null" ]]; then
        echo -e "${red}探针注册响应无效: ${response}${plain}"
        exit 1
    fi

    MACHINE_ID_ARG="$machine_id"
    MACHINE_TOKEN_ARG="$api_token"
}

setup_machine_probe() {
    enroll_machine_probe

    if [[ -z "$PANEL_URL_ARG" || -z "$MACHINE_TOKEN_ARG" || -z "$MACHINE_ID_ARG" ]]; then
        echo -e "${red}探针模式缺少 --panel，并且需要 --enroll-token 或 --token + --machine-id${plain}"
        exit 1
    fi

    local panel_url="${PANEL_URL_ARG%/}"
    ensure_private_dir "$CONFIG_DIR"
    if [[ -f "$CONFIG_FILE" ]]; then
        echo -e "${yellow}检测到已有加密配置，探针安装将保留旧配置；后续只合并面板下发节点${plain}"
    else
        local config_key
        local plain_config_file
        if ! config_key=$(ensure_config_key); then
            echo -e "${red}生成配置加密密钥失败${plain}"
            exit 1
        fi
        plain_config_file=$(mktemp)
        seed_plain_config_file "$plain_config_file"
        if ! encrypt_config_from_file "$plain_config_file" "$config_key"; then
            rm -f "$plain_config_file"
            echo -e "${red}初始化加密配置失败${plain}"
            exit 1
        fi
        rm -f "$plain_config_file"
    fi
    refresh_runtime_env_file || true

    jq -n \
        --arg panel_url "$panel_url" \
        --arg machine_token "$MACHINE_TOKEN_ARG" \
        --argjson machine_id "$MACHINE_ID_ARG" \
        --arg protocol "ravel-v1" \
        '{
            panel_url: $panel_url,
            machine_token: $machine_token,
            machine_id: $machine_id,
            protocol: $protocol,
            sync_interval: 30,
            status_interval: 5
        }' > "$PROBE_STATE_FILE"
    chmod 600 "$PROBE_STATE_FILE" >/dev/null 2>&1 || true

    if [[ x"${release}" == x"alpine" ]]; then
        cat <<EOF > /etc/init.d/ravel
#!/sbin/openrc-run

name="ravel"
description="Ravel sync service"

command="/usr/local/ravel/ravel"
command_args="ravel"
command_user="root"
pidfile="/run/ravel.pid"
command_background="yes"

start_pre() {
    if [ -f "\$pidfile" ] && ! kill -0 "\$(cat "\$pidfile" 2>/dev/null)" 2>/dev/null; then
        rm -f "\$pidfile"
    fi
}

stop_post() {
    rm -f "\$pidfile"
}

depend() {
    need net
}
EOF
        chmod +x /etc/init.d/ravel
        rc-update add ravel default >/dev/null 2>&1 || true
        rm -f /run/v2node.pid /run/ravel.pid /run/ravel-probe.pid /run/v2node-probe.pid
        service ravel restart >/dev/null 2>&1 || service ravel start >/dev/null 2>&1
        service v2node-probe stop >/dev/null 2>&1 || true
        rc-update del v2node-probe default >/dev/null 2>&1 || true
        service v2node stop >/dev/null 2>&1 || true
        rc-update del v2node default >/dev/null 2>&1 || true
    else
        cat <<EOF > /etc/systemd/system/ravel.service
[Unit]
Description=Ravel Sync Service
After=network.target
Wants=network.target

[Service]
Type=simple
User=root
Group=root
ExecStart=/usr/local/ravel/ravel ravel
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
        systemctl daemon-reload
        systemctl disable --now v2node >/dev/null 2>&1 || true
        systemctl enable ravel >/dev/null 2>&1 || true
        systemctl restart ravel >/dev/null 2>&1 || systemctl start ravel >/dev/null 2>&1
        systemctl disable --now v2node-probe >/dev/null 2>&1 || true
    fi

}

parse_args "$@"
restore_existing_machine_mode
echo -e "${green}开始安装${plain}"
install_base
install_v2node "$VERSION_ARG"
