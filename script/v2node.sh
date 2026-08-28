#!/bin/bash

red='\033[0;31m'
green='\033[0;32m'
yellow='\033[0;33m'
plain='\033[0m'

cur_dir=$(pwd)
REPO_SLUG="YeJianbo/v2node"
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

is_ravel_mode() {
    [[ -x /usr/local/ravel/ravel ]] \
        && [[ -f /etc/.buncloud-agent/state.json ]] \
        && { [[ -f /etc/systemd/system/ravel.service ]] || [[ -f /etc/init.d/ravel ]]; }
}

runtime_service_name() {
    if is_ravel_mode; then
        echo "ravel"
    else
        echo "v2node"
    fi
}

runtime_binary_path() {
    if is_ravel_mode; then
        echo "/usr/local/ravel/ravel"
    else
        echo "/usr/local/v2node/v2node"
    fi
}

runtime_display_name() {
    echo "Ravel"
}

confirm() {
    if [[ $# > 1 ]]; then
        echo && read -rp "$1 [默认$2]: " temp
        if [[ x"${temp}" == x"" ]]; then
            temp=$2
        fi
    else
        read -rp "$1 [y/n]: " temp
    fi
    if [[ x"${temp}" == x"y" || x"${temp}" == x"Y" ]]; then
        return 0
    else
        return 1
    fi
}

confirm_restart() {
    confirm "是否重启 Ravel" "y"
    if [[ $? == 0 ]]; then
        restart
    else
        show_menu
    fi
}

before_show_menu() {
    echo && echo -n -e "${yellow}按回车返回主菜单: ${plain}" && read temp
    show_menu
}

install() {
    bash <(curl -Ls "${SCRIPT_BASE_URL}/install.sh")
    if [[ $? == 0 ]]; then
        if [[ $# == 0 ]]; then
            start
        else
            start 0
        fi
    fi
}

update() {
    if [[ $# == 0 ]]; then
        echo && echo -n -e "输入指定版本(默认最新版): " && read version
    else
        version=$2
    fi
    bash <(curl -Ls "${SCRIPT_BASE_URL}/install.sh") $version
    if [[ $? == 0 ]]; then
        echo -e "${green}更新完成，已自动重启 Ravel，请使用 ravel log 查看运行日志${plain}"
        exit
    fi

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

config() {
    if is_ravel_mode; then
        echo -e "${yellow}Ravel 配置由面板云端管理，本机不提供明文配置编辑。${plain}"
        if [[ $# == 0 ]]; then
            before_show_menu
        fi
        return 0
    fi
    echo "Ravel 在修改节点配置后会自动尝试重启"
    vi /etc/v2node/config.json
    sleep 2
    restart
    check_status
    case $? in
        0)
            echo -e "Ravel 状态: ${green}已运行${plain}"
            ;;
        1)
            echo -e "检测到 Ravel 未启动或自动重启失败，是否查看日志？[Y/n]" && echo
            read -e -rp "(默认: y):" yn
            [[ -z ${yn} ]] && yn="y"
            if [[ ${yn} == [Yy] ]]; then
               show_log
            fi
            ;;
        2)
            echo -e "Ravel 状态: ${red}未安装${plain}"
    esac
}

unregister_machine_probe() {
    local probe_script="/usr/local/v2node/v2node-probe.sh"

    if [[ ! -x "$probe_script" ]]; then
        return 0
    fi

    if [[ -f /etc/.buncloud-agent/probe-state.json ]]; then
        "$probe_script" unregister >/dev/null 2>&1 && return 0
    elif [[ -f /etc/v2node/probe.env ]]; then
        V2NODE_PROBE_STATE_FILE=/etc/v2node/probe.env \
            "$probe_script" unregister >/dev/null 2>&1 && return 0
    else
        return 0
    fi

    echo -e "${yellow}面板注销探针失败，继续执行本地卸载；后台记录可稍后手动删除${plain}"
}

remove_gost() {
    if [[ x"${release}" == x"alpine" ]]; then
        service gost stop >/dev/null 2>&1 || true
        rc-update del gost default >/dev/null 2>&1 || true
        rm -f /etc/init.d/gost
    else
        systemctl stop gost >/dev/null 2>&1 || true
        systemctl disable gost >/dev/null 2>&1 || true
        rm -f /etc/systemd/system/gost.service
        systemctl daemon-reload >/dev/null 2>&1 || true
    fi

    pkill -x gost >/dev/null 2>&1 || true
    rm -f /etc/gost/config.json /etc/gost/config.json.last-good /usr/bin/gost
    rmdir /etc/gost >/dev/null 2>&1 || true
}

uninstall() {
    confirm "确定要卸载 Ravel 吗?" "n"
    if [[ $? != 0 ]]; then
        if [[ $# == 0 ]]; then
            show_menu
        fi
        return 0
    fi
    unregister_machine_probe
    remove_gost

    if [[ x"${release}" == x"alpine" ]]; then
        service ravel stop >/dev/null 2>&1 || true
        rc-update del ravel default >/dev/null 2>&1 || true
        rm /etc/init.d/ravel -f
        service ravel-probe stop >/dev/null 2>&1 || true
        rc-update del ravel-probe default >/dev/null 2>&1 || true
        rm /etc/init.d/ravel-probe -f
        service v2node-probe stop >/dev/null 2>&1 || true
        rc-update del v2node-probe default >/dev/null 2>&1 || true
        rm /etc/init.d/v2node-probe -f
        service v2node stop
        rc-update del v2node
        rm /etc/init.d/v2node -f
    else
        systemctl stop ravel >/dev/null 2>&1 || true
        systemctl disable ravel >/dev/null 2>&1 || true
        rm /etc/systemd/system/ravel.service -f
        systemctl stop ravel-probe >/dev/null 2>&1 || true
        systemctl disable ravel-probe >/dev/null 2>&1 || true
        rm /etc/systemd/system/ravel-probe.service -f
        systemctl stop v2node-probe >/dev/null 2>&1 || true
        systemctl disable v2node-probe >/dev/null 2>&1 || true
        rm /etc/systemd/system/v2node-probe.service -f
        systemctl stop v2node
        systemctl disable v2node
        rm /etc/systemd/system/v2node.service -f
        systemctl daemon-reload
        systemctl reset-failed
    fi
    rm /etc/v2node/probe.env -f
    rm /etc/v2node/ -rf
    rm /etc/.buncloud-agent/ -rf
    rm /usr/local/v2node/ -rf
    rm /usr/local/ravel/ -rf
    rm /run/ravel.pid /run/ravel-probe.pid /run/v2node-probe.pid -f
    rm /run/ravel.lock /run/v2node-probe.lock -rf
    rm /usr/bin/v2node /usr/bin/v2bx /usr/bin/ravel -f

    echo ""
    echo -e "${green}Ravel、旧探针、GOST、节点程序及本地状态已卸载完成${plain}"
    echo ""

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

start() {
    local service_name
    service_name=$(runtime_service_name)
    check_status
    if [[ $? == 0 ]]; then
        echo ""
        echo -e "${green}Ravel 已运行，无需再次启动，如需重启请选择重启${plain}"
    else
        if [[ x"${release}" == x"alpine" ]]; then
            service "$service_name" start
        else
            systemctl start "$service_name"
        fi
        sleep 2
        check_status
        if [[ $? == 0 ]]; then
            echo -e "${green}Ravel 启动成功，请使用 ravel log 查看运行日志${plain}"
        else
            echo -e "${red}Ravel 可能启动失败，请稍后使用 ravel log 查看日志信息${plain}"
        fi
    fi

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

stop() {
    local service_name
    service_name=$(runtime_service_name)
    if [[ x"${release}" == x"alpine" ]]; then
        service "$service_name" stop
    else
        systemctl stop "$service_name"
    fi
    sleep 2
    check_status
    if [[ $? == 1 ]]; then
        echo -e "${green}Ravel 停止成功${plain}"
    else
        echo -e "${red}Ravel 停止失败，可能是因为停止时间超过了两秒，请稍后查看日志信息${plain}"
    fi

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

restart() {
    local service_name
    service_name=$(runtime_service_name)
    if [[ x"${release}" == x"alpine" ]]; then
        service "$service_name" restart
    else
        systemctl restart "$service_name"
    fi
    sleep 2
    check_status
    if [[ $? == 0 ]]; then
        echo -e "${green}Ravel 重启成功，请使用 ravel log 查看运行日志${plain}"
    else
        echo -e "${red}Ravel 可能启动失败，请稍后使用 ravel log 查看日志信息${plain}"
    fi
    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

status() {
    local service_name
    service_name=$(runtime_service_name)
    if [[ x"${release}" == x"alpine" ]]; then
        service "$service_name" status
    else
        systemctl status "$service_name" --no-pager -l
    fi
    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

enable() {
    local service_name
    service_name=$(runtime_service_name)
    if [[ x"${release}" == x"alpine" ]]; then
        rc-update add "$service_name"
    else
        systemctl enable "$service_name"
    fi
    if [[ $? == 0 ]]; then
        echo -e "${green}Ravel 设置开机自启成功${plain}"
    else
        echo -e "${red}Ravel 设置开机自启失败${plain}"
    fi

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

disable() {
    local service_name
    service_name=$(runtime_service_name)
    if [[ x"${release}" == x"alpine" ]]; then
        rc-update del "$service_name"
    else
        systemctl disable "$service_name"
    fi
    if [[ $? == 0 ]]; then
        echo -e "${green}Ravel 取消开机自启成功${plain}"
    else
        echo -e "${red}Ravel 取消开机自启失败${plain}"
    fi

    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

show_log() {
    local service_name
    service_name=$(runtime_service_name)
    if [[ x"${release}" == x"alpine" ]]; then
        if [[ -f "/var/log/${service_name}.log" ]]; then
            tail -n 100 -f "/var/log/${service_name}.log"
        else
            service "$service_name" status
            echo -e "${yellow}OpenRC 未配置独立日志文件，请查看系统日志。${plain}"
        fi
    else
        journalctl -u "${service_name}.service" -e --no-pager -f
    fi
    if [[ $# == 0 ]]; then
        before_show_menu
    fi
}

update_shell() {
    local manager_script_tmp
    manager_script_tmp=$(mktemp)
    if ! curl -fsSL "${SCRIPT_BASE_URL}/v2node.sh" -o "$manager_script_tmp" \
        || ! bash -n "$manager_script_tmp"; then
        rm -f "$manager_script_tmp"
        echo ""
        echo -e "${red}下载或校验 Ravel 管理脚本失败，现有命令未变${plain}"
        before_show_menu
    else
        rm -f /usr/bin/ravel /usr/bin/v2node /usr/bin/v2bx
        install -m 0755 "$manager_script_tmp" /usr/bin/ravel
        rm -f "$manager_script_tmp"
        ln -sf /usr/bin/ravel /usr/bin/v2node
        ln -sf /usr/bin/ravel /usr/bin/v2bx
        echo -e "${green}升级脚本成功，请重新运行脚本${plain}" && exit 0
    fi
}

# 0: running, 1: not running, 2: not installed
check_status() {
    local service_name binary_path
    service_name=$(runtime_service_name)
    binary_path=$(runtime_binary_path)
    if [[ ! -x "$binary_path" ]]; then
        return 2
    fi
    if [[ x"${release}" == x"alpine" ]]; then
        temp=$(service "$service_name" status 2>/dev/null | awk '{print $3}')
        if [[ x"${temp}" == x"started" ]]; then
            return 0
        else
            return 1
        fi
    else
        if systemctl is-active --quiet "$service_name"; then
            return 0
        else
            return 1
        fi
    fi
}

check_enabled() {
    local service_name
    service_name=$(runtime_service_name)
    if [[ x"${release}" == x"alpine" ]]; then
        temp=$(rc-update show | grep -E "(^|[[:space:]])${service_name}([[:space:]]|$)")
        if [[ x"${temp}" == x"" ]]; then
            return 1
        else
            return 0
        fi
    else
        temp=$(systemctl is-enabled "$service_name" 2>/dev/null)
        if [[ x"${temp}" == x"enabled" ]]; then
            return 0
        else
            return 1;
        fi
    fi
}

check_uninstall() {
    check_status
    if [[ $? != 2 ]]; then
        echo ""
        echo -e "${red}Ravel 已安装，请不要重复安装${plain}"
        if [[ $# == 0 ]]; then
            before_show_menu
        fi
        return 1
    else
        return 0
    fi
}

check_install() {
    check_status
    if [[ $? == 2 ]]; then
        echo ""
        echo -e "${red}请先安装 Ravel${plain}"
        if [[ $# == 0 ]]; then
            before_show_menu
        fi
        return 1
    else
        return 0
    fi
}

show_status() {
    local display_name binary_path runtime_mode
    display_name=$(runtime_display_name)
    binary_path=$(runtime_binary_path)
    if is_ravel_mode; then
        runtime_mode="原生云控模式"
    else
        runtime_mode="节点兼容模式"
    fi
    if [[ -x "$binary_path" ]]; then
        echo -e "运行模式: ${green}${runtime_mode}${plain}"
        echo -n "当前版本: "
        "$binary_path" version 2>/dev/null | head -n 1
    fi
    check_status
    case $? in
        0)
            echo -e "${display_name}状态: ${green}已运行${plain}"
            show_enable_status
            ;;
        1)
            echo -e "${display_name}状态: ${yellow}未运行${plain}"
            show_enable_status
            ;;
        2)
            echo -e "${display_name}状态: ${red}未安装${plain}"
    esac
}

show_enable_status() {
    check_enabled
    if [[ $? == 0 ]]; then
        echo -e "是否开机自启: ${green}是${plain}"
    else
        echo -e "是否开机自启: ${red}否${plain}"
    fi
}

show_v2node_version() {
    local display_name binary_path
    display_name=$(runtime_display_name)
    binary_path=$(runtime_binary_path)
    echo -n "${display_name} 版本："
    "$binary_path" version
    echo ""
    if [[ $# == 0 ]]; then
        before_show_menu
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
                echo -e "${red}当前系统缺少 jq，无法追加节点，请先执行 ravel update 或手动安装 jq${plain}"
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
            echo -e "${green}Ravel 重启成功${plain}"
        else
            echo -e "${red}Ravel 可能启动失败，请使用 ravel log 查看日志信息${plain}"
        fi
}

probe_sync() {
    if is_ravel_mode; then
        echo -e "${green}正在重启 Ravel 并立即从面板同步配置...${plain}"
        restart 0
        return $?
    fi
    if [[ ! -x /usr/local/v2node/v2node-probe.sh ]]; then
        echo -e "${red}未找到探针同步脚本${plain}"
        return 1
    fi
    /usr/local/v2node/v2node-probe.sh sync
}


generate_config_file() {
    # 交互式收集参数，提供示例默认值
    read -rp "面板API地址[格式: https://example.com/]: " api_host
    api_host=${api_host:-https://example.com/}
    read -rp "节点ID: " node_id
    node_id=${node_id:-1}
    read -rp "节点通讯密钥: " api_key

    # 生成配置文件（覆盖可能从包中复制的模板）
    generate_v2node_config "$api_host" "$node_id" "$api_key"
}

# 放开防火墙端口
open_ports() {
    systemctl stop firewalld.service 2>/dev/null
    systemctl disable firewalld.service 2>/dev/null
    setenforce 0 2>/dev/null
    ufw disable 2>/dev/null
    iptables -P INPUT ACCEPT 2>/dev/null
    iptables -P FORWARD ACCEPT 2>/dev/null
    iptables -P OUTPUT ACCEPT 2>/dev/null
    iptables -t nat -F 2>/dev/null
    iptables -t mangle -F 2>/dev/null
    iptables -F 2>/dev/null
    iptables -X 2>/dev/null
    netfilter-persistent save 2>/dev/null
    echo -e "${green}放开防火墙端口成功！${plain}"
}

show_usage() {
    echo "Ravel 管理脚本使用方法: "
    echo "------------------------------------------"
    echo "ravel              - 显示管理菜单"
    echo "ravel start        - 启动 Ravel"
    echo "ravel stop         - 停止 Ravel"
    echo "ravel restart      - 重启 Ravel"
    echo "ravel status       - 查看 Ravel 状态"
    echo "ravel enable       - 设置 Ravel 开机自启"
    echo "ravel disable      - 取消 Ravel 开机自启"
    echo "ravel log          - 查看 Ravel 日志"
    echo "ravel x25519       - 生成 x25519 密钥"
    echo "ravel generate     - 生成节点配置文件"
    echo "ravel probe_sync   - 立即同步面板配置"
    echo "ravel update       - 更新 Ravel"
    echo "ravel update x.x.x - 安装指定版本"
    echo "ravel install      - 安装 Ravel"
    echo "ravel uninstall    - 卸载 Ravel"
    echo "ravel version      - 查看 Ravel 版本"
    echo "v2node / v2bx      - 兼容入口，同样进入 Ravel 管理"
    echo "------------------------------------------"
}

show_menu() {
    echo -e "
  ${green}Ravel 管理脚本，${plain}${red}不适用于docker${plain}
--- https://github.com/YeJianbo/v2node ---
  ${green}0.${plain} 修改配置
————————————————
  ${green}1.${plain} 安装 Ravel
  ${green}2.${plain} 更新 Ravel
  ${green}3.${plain} 卸载 Ravel
————————————————
  ${green}4.${plain} 启动 Ravel
  ${green}5.${plain} 停止 Ravel
  ${green}6.${plain} 重启 Ravel
  ${green}7.${plain} 查看 Ravel 状态
  ${green}8.${plain} 查看 Ravel 日志
————————————————
  ${green}9.${plain} 设置 Ravel 开机自启
  ${green}10.${plain} 取消 Ravel 开机自启
————————————————
  ${green}11.${plain} 查看 Ravel 版本
  ${green}12.${plain} 升级 Ravel 管理脚本
  ${green}13.${plain} 生成节点配置文件
  ${green}14.${plain} 立即同步面板配置
  ${green}15.${plain} 放行 VPS 的所有网络端口
  ${green}16.${plain} 退出脚本
 "
 #后续更新可加入上方字符串中
    show_status
    echo && read -rp "请输入选择 [0-16]: " num

    case "${num}" in
        0) config ;;
        1) check_uninstall && install ;;
        2) check_install && update ;;
        3) check_install && uninstall ;;
        4) check_install && start ;;
        5) check_install && stop ;;
        6) check_install && restart ;;
        7) check_install && status ;;
        8) check_install && show_log ;;
        9) check_install && enable ;;
        10) check_install && disable ;;
        11) check_install && show_v2node_version ;;
        12) update_shell ;;
        13) generate_config_file ;;
        14) probe_sync ;;
        15) open_ports ;;
        16) exit ;;
        *) echo -e "${red}请输入正确的数字 [0-16]${plain}" ;;
    esac
}


if [[ $# > 0 ]]; then
    case $1 in
        "start") check_install 0 && start 0 ;;
        "stop") check_install 0 && stop 0 ;;
        "restart") check_install 0 && restart 0 ;;
        "status") check_install 0 && status 0 ;;
        "enable") check_install 0 && enable 0 ;;
        "disable") check_install 0 && disable 0 ;;
        "log") check_install 0 && show_log 0 ;;
        "update") check_install 0 && update 0 $2 ;;
        "config") config $* ;;
        "generate") generate_config_file ;;
        "probe_sync") probe_sync ;;
        "install") check_uninstall 0 && install 0 ;;
        "uninstall") check_install 0 && uninstall 0 ;;
        "version") check_install 0 && show_v2node_version 0 ;;
        "update_shell") update_shell ;;
        *) show_usage
    esac
else
    show_menu
fi
