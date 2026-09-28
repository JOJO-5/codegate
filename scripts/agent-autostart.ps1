#Requires -Version 5.1
<#
.SYNOPSIS
  Install, inspect, or remove the CodeGate Agent startup task.
.DESCRIPTION
  The task runs at system startup under the current user's stored credentials.
  It can run before desktop sign-in and reuses that user's config and device key.
#>
param(
    [Parameter(Position = 0)]
    [ValidateSet('install', 'status', 'uninstall')]
    [string]$Action = 'install',

    [string]$AgentPath = (Join-Path $PSScriptRoot '..\bin\codegate-agent.exe'),
    [string]$ConfigPath = (Join-Path $env:APPDATA 'CodeGate\agent.json')
)

$ErrorActionPreference = 'Stop'
$taskName = 'CodeGate Agent'
$taskPath = '\'
$task = Get-ScheduledTask -TaskName $taskName -TaskPath $taskPath -ErrorAction SilentlyContinue

switch ($Action) {
    'status' {
        if ($null -eq $task) {
            Write-Host 'CodeGate Agent 开机任务尚未安装。'
            exit 0
        }
        $info = Get-ScheduledTaskInfo -InputObject $task
        Write-Host "任务状态: $($task.State)"
        Write-Host "运行账号: $($task.Principal.UserId)"
        Write-Host "上次运行: $($info.LastRunTime)"
        Write-Host "上次结果: $($info.LastTaskResult)"
        exit 0
    }

    'uninstall' {
        if ($null -eq $task) {
            Write-Host 'CodeGate Agent 开机任务尚未安装。'
            exit 0
        }
        Stop-ScheduledTask -InputObject $task -ErrorAction SilentlyContinue
        Unregister-ScheduledTask -TaskName $taskName -TaskPath $taskPath -Confirm:$false
        Write-Host '已移除开机任务；设备配置和私钥仍保留在用户目录。'
        exit 0
    }
}

if (-not (Test-Path -LiteralPath $AgentPath -PathType Leaf)) {
    throw "Agent 程序不存在: $AgentPath。请先在仓库根目录构建 bin\codegate-agent.exe。"
}
if (-not (Test-Path -LiteralPath $ConfigPath -PathType Leaf)) {
    throw "配置文件不存在: $ConfigPath。请先配置并完成 pair。"
}

$source = (Resolve-Path -LiteralPath $AgentPath).ProviderPath
$config = (Resolve-Path -LiteralPath $ConfigPath).ProviderPath
$configData = Get-Content -LiteralPath $config -Raw | ConvertFrom-Json
if ($configData.state_dir -and -not [IO.Path]::IsPathRooted($configData.state_dir)) {
    throw 'state_dir 必须是绝对路径，避免计划任务启动时生成另一把设备密钥。'
}
if ($env:CODEGATE_STATE_DIR) {
    throw '请把 CODEGATE_STATE_DIR 改写到 agent.json 的绝对 state_dir 路径后重新配对。'
}
& $source doctor -config $config
if ($LASTEXITCODE -ne 0) {
    throw 'Agent 自检失败，请先修复配置或终端环境。'
}

# Keep the executable at a stable path, independent of the source checkout.
$installDir = Join-Path $env:LOCALAPPDATA 'CodeGate\bin'
$installedAgent = Join-Path $installDir 'codegate-agent.exe'
New-Item -ItemType Directory -Path $installDir -Force | Out-Null

if ($null -ne $task) {
    Stop-ScheduledTask -InputObject $task -ErrorAction SilentlyContinue
    for ($i = 0; $i -lt 20; $i++) {
        $task = Get-ScheduledTask -TaskName $taskName -TaskPath $taskPath
        if ($task.State -ne 'Running') { break }
        Start-Sleep -Milliseconds 500
    }
    if ($task.State -eq 'Running') {
        throw '旧任务仍在运行，未替换 Agent。请停止任务后重试。'
    }
}
if (-not [string]::Equals($source, $installedAgent, [StringComparison]::OrdinalIgnoreCase)) {
    Copy-Item -LiteralPath $source -Destination $installedAgent -Force
}

$currentUser = [Security.Principal.WindowsIdentity]::GetCurrent().Name
$credential = Get-Credential -UserName $currentUser -Message '请输入当前 Windows 账号的密码，以便开机且未登录时运行 Agent（PIN 不适用）'
if ($null -eq $credential) {
    throw '已取消安装。'
}
if (-not [string]::Equals($credential.UserName, $currentUser, [StringComparison]::OrdinalIgnoreCase)) {
    throw "必须使用当前账号 $currentUser，才能复用已经配对的设备身份。"
}

$plainPassword = $credential.GetNetworkCredential().Password
try {
    $scheduledAction = New-ScheduledTaskAction -Execute $installedAgent -Argument "run -config `"$config`"" -WorkingDirectory $installDir
    $trigger = New-ScheduledTaskTrigger -AtStartup
    $settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit (New-TimeSpan -Seconds 0) -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) -MultipleInstances IgnoreNew -StartWhenAvailable -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
    Register-ScheduledTask -TaskName $taskName -TaskPath $taskPath -Action $scheduledAction -Trigger $trigger -Settings $settings -User $currentUser -Password $plainPassword -RunLevel Limited -Description 'CodeGate Agent (starts before desktop sign-in)' -Force | Out-Null
}
finally {
    $plainPassword = $null
    $credential = $null
}

Start-ScheduledTask -TaskName $taskName -TaskPath $taskPath
Write-Host '开机任务已安装并启动。登录桌面前也会尝试连接 Server。'
Write-Host '查询状态: powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\agent-autostart.ps1 status'
Write-Host '请在 Server 的设备页确认 Agent 已上线；首次还需在这台机器上验证一次锁屏/重启场景。'
