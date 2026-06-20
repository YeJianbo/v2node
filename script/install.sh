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
                INSTALL_MODE_ARG="$2"; shift 2 ;;
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
        need_install_yum wget curl unzip tar cronie socat ca-certificates pv jq openssl
        update-ca-trust force-enable >/dev/null 2>&1 || true
    elif [[ x"${release}" == x"alpine" ]]; then
        need_install_apk wget curl unzip tar socat ca-certificates pv jq openssl
        update-ca-certificates >/dev/null 2>&1 || true
    elif [[ x"${release}" == x"debian" ]]; then
        need_install_apt wget curl unzip tar cron socat ca-certificates pv jq openssl
        update-ca-certificates >/dev/null 2>&1 || true
    elif [[ x"${release}" == x"ubuntu" ]]; then
        need_install_apt wget curl unzip tar cron socat ca-certificates pv jq openssl
        update-ca-certificates >/dev/null 2>&1 || true
    elif [[ x"${release}" == x"arch" ]]; then
        echo "更新包数据库..."
        pacman -Sy --noconfirm >/dev/null 2>&1
        # --needed 会跳过已安装的包，非常高效
        echo "安装必需的包..."
        pacman -S --noconfirm --needed wget curl unzip tar cronie socat ca-certificates pv jq openssl >/dev/null 2>&1
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
        local config_file="/etc/v2node/config.json"
        local action="生成"

        if ! [[ "$node_id" =~ ^[0-9]+$ ]]; then
            echo -e "${red}节点ID必须为整数${plain}"
            return 1
        fi

        mkdir -p /etc/v2node >/dev/null 2>&1
        if [[ -f "$config_file" ]]; then
            local tmp_file
            tmp_file=$(mktemp)
            if ! command -v jq >/dev/null 2>&1; then
                echo -e "${red}当前系统缺少 jq，无法追加节点，请先执行 v2node update 或手动安装 jq${plain}"
                rm -f "$tmp_file"
                return 1
            fi
            if ! jq empty "$config_file" >/dev/null 2>&1; then
                echo -e "${red}现有配置文件不是合法 JSON，已停止追加节点，请先检查 ${config_file}${plain}"
                rm -f "$tmp_file"
                return 1
            fi
            if ! jq \
                --arg api_host "$api_host" \
                --argjson node_id "$node_id" \
                --arg api_key "$api_key" \
                '.Nodes = ((if (.Nodes | type) == "array" then .Nodes else [] end) + [{
                    "ApiHost": $api_host,
                    "NodeID": $node_id,
                    "ApiKey": $api_key,
                    "Timeout": 15
                }])' \
                "$config_file" > "$tmp_file"; then
                echo -e "${red}追加节点到配置文件失败${plain}"
                rm -f "$tmp_file"
                return 1
            fi
            mv "$tmp_file" "$config_file"
            action="追加"
        else
        cat > "$config_file" <<EOF
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
    local url="https://github.com/${repo_slug}/releases/download/${version}/v2node-linux-${arch}.zip"

    curl -fLsS "$url" | pv -s 30M -W -N "下载进度" > /usr/local/v2node/v2node-linux.zip
}

install_v2node() {
    local version_param="$1"
    local release_repo="$REPO_SLUG"
    if [[ -e /usr/local/v2node/ ]]; then
        rm -rf /usr/local/v2node/
    fi

    mkdir /usr/local/v2node/ -p
    cd /usr/local/v2node/

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
        if ! download_release_zip "$release_repo" "$last_version"; then
            if [[ "$release_repo" != "$UPSTREAM_REPO_SLUG" ]]; then
                echo -e "${yellow}从你的 fork 下载 release 失败，回退到上游 release 源${plain}"
                release_repo="$UPSTREAM_REPO_SLUG"
            fi
            if ! download_release_zip "$release_repo" "$last_version"; then
                echo -e "${red}下载 v2node 失败，请确保你的服务器能够下载 Github 的文件${plain}"
                exit 1
            fi
        fi
    else
        last_version=$version_param
        if ! download_release_zip "$release_repo" "$last_version"; then
            echo -e "${yellow}你的 fork 中不存在版本 ${last_version}，回退到上游 release 源${plain}"
            release_repo="$UPSTREAM_REPO_SLUG"
            if ! download_release_zip "$release_repo" "$last_version"; then
                echo -e "${red}下载 v2node $1 失败，请确保此版本存在${plain}"
                exit 1
            fi
        fi
    fi

    unzip v2node-linux.zip
    rm v2node-linux.zip -f
    chmod +x v2node
    mkdir /etc/v2node/ -p
    cp geoip.dat /etc/v2node/
    cp geosite.dat /etc/v2node/
    if ! curl -fsSL "${SCRIPT_BASE_URL}/v2node-probe.sh" -o /usr/local/v2node/v2node-probe.sh; then
        echo -e "${red}下载探针同步脚本失败${plain}"
        exit 1
    fi
    chmod +x /usr/local/v2node/v2node-probe.sh
    if [[ x"${release}" == x"alpine" ]]; then
        rm /etc/init.d/v2node -f
        cat <<EOF > /etc/init.d/v2node
#!/sbin/openrc-run

name="v2node"
description="v2node"

command="/usr/local/v2node/v2node"
command_args="server"
command_user="root"

pidfile="/run/v2node.pid"
command_background="yes"

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
ExecStart=/usr/local/v2node/v2node server
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
    elif [[ ! -f /etc/v2node/config.json ]]; then
        # 如果通过 CLI 传入了完整参数，则直接生成配置并跳过交互
        if [[ -n "$API_HOST_ARG" && -n "$NODE_ID_ARG" && -n "$API_KEY_ARG" ]]; then
            generate_v2node_config "$API_HOST_ARG" "$NODE_ID_ARG" "$API_KEY_ARG"
            echo -e "${green}已根据参数生成 /etc/v2node/config.json${plain}"
            first_install=false
        else
            cp config.json /etc/v2node/
            first_install=true
        fi
    else
        if [[ -n "$API_HOST_ARG" && -n "$NODE_ID_ARG" && -n "$API_KEY_ARG" ]]; then
            if ! generate_v2node_config "$API_HOST_ARG" "$NODE_ID_ARG" "$API_KEY_ARG"; then
                exit 1
            fi
            echo -e "${green}检测到现有安装，已向 /etc/v2node/config.json 追加一个节点${plain}"
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


    curl -o /usr/bin/v2node -Ls "${SCRIPT_BASE_URL}/v2node.sh"
    chmod +x /usr/bin/v2node

    cd $cur_dir
    rm -f install.sh
    echo "------------------------------------------"
    echo -e "管理脚本使用方法: "
    echo "------------------------------------------"
    echo "v2node              - 显示管理菜单 (功能更多)"
    echo "v2node start        - 启动 v2node"
    echo "v2node stop         - 停止 v2node"
    echo "v2node restart      - 重启 v2node"
    echo "v2node status       - 查看 v2node 状态"
    echo "v2node enable       - 设置 v2node 开机自启"
    echo "v2node disable      - 取消 v2node 开机自启"
    echo "v2node log          - 查看 v2node 日志"
    echo "v2node generate     - 生成 v2node 配置文件"
    echo "v2node update       - 更新 v2node"
    echo "v2node update x.x.x - 更新 v2node 指定版本"
    echo "v2node install      - 安装 v2node"
    echo "v2node uninstall    - 卸载 v2node"
    echo "v2node version      - 查看 v2node 版本"
    echo "------------------------------------------"
    curl -fsS --max-time 10 "https://api.v-50.me/counter" || true

    if [[ "$INSTALL_MODE_ARG" == "machine" ]]; then
        echo -e "${green}已启用探针模式。后续在面板为该在线服务器分配 v2node 节点后，会自动同步到本机。${plain}"
    elif [[ $first_install == true ]]; then
        read -rp "检测到你为第一次安装 v2node，是否自动生成 /etc/v2node/config.json？(y/n): " if_generate
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
    rm -f /etc/v2node/probe.env
    if [[ x"${release}" == x"alpine" ]]; then
        if [[ -f /etc/init.d/v2node-probe ]]; then
            service v2node-probe stop >/dev/null 2>&1 || true
            rc-update del v2node-probe default >/dev/null 2>&1 || true
            rm -f /etc/init.d/v2node-probe
        fi
    else
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
    local backup_file=""
    local escaped_panel_url
    local escaped_machine_token
    escaped_panel_url=$(escape_env_value "$panel_url")
    escaped_machine_token=$(escape_env_value "$MACHINE_TOKEN_ARG")

    mkdir -p /etc/v2node
    if [[ -f /etc/v2node/config.json ]]; then
        backup_file="/etc/v2node/config.manual.backup.$(date +%Y%m%d%H%M%S).json"
        cp /etc/v2node/config.json "$backup_file"
    fi

    cat > /etc/v2node/probe.env <<EOF
PANEL_URL='${escaped_panel_url}'
MACHINE_TOKEN='${escaped_machine_token}'
MACHINE_ID='${MACHINE_ID_ARG}'
SYNC_INTERVAL='15'
EOF

    cat > /etc/v2node/config.json <<EOF
{
    "Log": {
        "Level": "warning",
        "Output": "",
        "Access": "none"
    },
    "Nodes": []
}
EOF

    if [[ x"${release}" == x"alpine" ]]; then
        cat <<EOF > /etc/init.d/v2node-probe
#!/sbin/openrc-run

name="v2node-probe"
description="v2node probe sync service"

command="/usr/local/v2node/v2node-probe.sh"
command_args="daemon"
command_user="root"
pidfile="/run/v2node-probe.pid"
command_background="yes"

depend() {
    need net
}
EOF
        chmod +x /etc/init.d/v2node-probe
        rc-update add v2node-probe default >/dev/null 2>&1 || true
        service v2node-probe restart >/dev/null 2>&1 || service v2node-probe start >/dev/null 2>&1
        service v2node start >/dev/null 2>&1 || true
    else
        cat <<EOF > /etc/systemd/system/v2node-probe.service
[Unit]
Description=v2node Probe Sync Service
After=network.target
Wants=network.target

[Service]
Type=simple
User=root
Group=root
ExecStart=/usr/local/v2node/v2node-probe.sh daemon
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
        systemctl daemon-reload
        systemctl enable v2node >/dev/null 2>&1 || true
        systemctl enable v2node-probe >/dev/null 2>&1 || true
        systemctl restart v2node >/dev/null 2>&1 || systemctl start v2node >/dev/null 2>&1
        systemctl restart v2node-probe >/dev/null 2>&1 || systemctl start v2node-probe >/dev/null 2>&1
    fi

    /usr/local/v2node/v2node-probe.sh sync >/dev/null 2>&1 || true

    if [[ -n "$backup_file" ]]; then
        echo -e "${yellow}已将原有手工配置备份到: ${backup_file}${plain}"
    fi
}

parse_args "$@"
echo -e "${green}开始安装${plain}"
install_base
install_v2node "$VERSION_ARG"
