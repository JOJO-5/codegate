#!/bin/sh
# macOS system LaunchDaemon running as the paired (unprivileged) user.
set -eu

action=${1:-install}
label=com.codegate.agent
plist=/Library/LaunchDaemons/$label.plist
binary=/usr/local/libexec/codegate/codegate-agent
config="$HOME/Library/Application Support/codegate/agent.json"
account=$(id -un)

case "$action" in
  status)
    sudo launchctl print "system/$label"
    exit $?
    ;;
  uninstall)
    sudo launchctl bootout system "$plist" 2>/dev/null || :
    sudo rm -f "$plist"
    echo '已移除 Agent 开机任务；设备配置和私钥仍保留。'
    exit 0
    ;;
  install) ;;
  *) echo '用法: agent-autostart-macos.sh install|status|uninstall' >&2; exit 2 ;;
esac

[ "$(uname -s)" = Darwin ] || { echo '仅支持 macOS。' >&2; exit 1; }
[ -f "$config" ] || { echo "请先配置并配对: $config" >&2; exit 1; }
source_binary=${CODEGATE_AGENT_BINARY:-./bin/codegate-agent}
[ -f "$source_binary" ] || { echo "Agent 程序不存在: $source_binary" >&2; exit 1; }
"$source_binary" doctor -config "$config"

xml_escape() {
  printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g' -e 's/"/\&quot;/g' -e "s/'/\&apos;/g"
}
escaped_user=$(xml_escape "$account")
escaped_home=$(xml_escape "$HOME")
escaped_config=$(xml_escape "$config")

tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT HUP INT TERM
cat > "$tmp" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$label</string>
  <key>UserName</key><string>$escaped_user</string>
  <key>ProgramArguments</key>
  <array>
    <string>$binary</string>
    <string>run</string>
    <string>-config</string>
    <string>$escaped_config</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>HOME</key><string>$escaped_home</string>
    <key>PATH</key><string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
  </dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
</dict>
</plist>
PLIST
plutil -lint "$tmp" >/dev/null

sudo launchctl bootout system "$plist" 2>/dev/null || :
sudo mkdir -p /usr/local/libexec/codegate
sudo install -m 0755 "$source_binary" "$binary"
sudo install -m 0644 "$tmp" "$plist"
sudo chown root:wheel "$plist" "$binary"
sudo launchctl bootstrap system "$plist"
echo '已启用 macOS Agent 开机自启。重启后请在登录前验证设备在线。'
