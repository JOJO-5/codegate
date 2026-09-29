#!/bin/sh
# Linux user service. loginctl linger keeps the user manager alive before login.
set -eu

action=${1:-install}
unit_dir="$HOME/.config/systemd/user"
unit="$unit_dir/codegate-agent.service"
binary="$HOME/.local/bin/codegate-agent"
config="$HOME/.config/codegate/agent.json"

command -v systemctl >/dev/null 2>&1 || { echo '需要 systemd。' >&2; exit 1; }
case "$action" in
  status)
    systemctl --user status codegate-agent.service
    exit $?
    ;;
  uninstall)
    systemctl --user disable --now codegate-agent.service 2>/dev/null || :
    rm -f "$unit"
    systemctl --user daemon-reload
    echo '已移除 Agent 服务；设备配置和私钥仍保留。未修改用户的 linger 设置。'
    exit 0
    ;;
  install) ;;
  *) echo '用法: agent-autostart-linux.sh install|status|uninstall' >&2; exit 2 ;;
esac

[ -f "$config" ] || { echo "请先配置并配对: $config" >&2; exit 1; }
source_binary=${CODEGATE_AGENT_BINARY:-./bin/codegate-agent}
[ -f "$source_binary" ] || { echo "Agent 程序不存在: $source_binary" >&2; exit 1; }
"$source_binary" doctor -config "$config"

mkdir -p "$unit_dir" "$HOME/.local/bin"
systemctl --user stop codegate-agent.service 2>/dev/null || :
install -m 0755 "$source_binary" "$binary"
cat > "$unit" <<'UNIT'
[Unit]
Description=CodeGate Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%h/.local/bin/codegate-agent run -config %h/.config/codegate/agent.json
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
UNIT

# A user service normally starts at login. Linger starts the user manager at boot.
if command -v loginctl >/dev/null 2>&1; then
  if [ "$(loginctl show-user "$USER" -p Linger --value 2>/dev/null)" != yes ]; then
    echo '启用 linger 需要管理员授权，以便登录前运行 Agent。'
    sudo loginctl enable-linger "$USER"
  fi
else
  echo '缺少 loginctl，无法保证登录前运行。' >&2
  exit 1
fi
systemctl --user daemon-reload
systemctl --user enable --now codegate-agent.service
echo '已启用 Linux Agent 开机自启。重启后请在登录前验证设备在线。'
