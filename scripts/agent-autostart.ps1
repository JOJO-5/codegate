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
    [ValidateSet('install', 'repair', 'status', 'uninstall')]
    [string]$Action = 'install',

    [string]$AgentPath = (Join-Path $PSScriptRoot '..\bin\codegate-agent.exe'),
    [string]$ConfigPath = (Join-Path $env:APPDATA 'CodeGate\agent.json')
)

$ErrorActionPreference = 'Stop'
$taskName = 'CodeGate Agent'
$taskPath = '\'
$task = Get-ScheduledTask -TaskName $taskName -TaskPath $taskPath -ErrorAction SilentlyContinue

function New-BackgroundLauncher([string]$Destination) {
    $compiler = @(
        (Join-Path $env:WINDIR 'Microsoft.NET\Framework64\v4.0.30319\csc.exe'),
        (Join-Path $env:WINDIR 'Microsoft.NET\Framework\v4.0.30319\csc.exe')
    ) | Where-Object { Test-Path -LiteralPath $_ -PathType Leaf } | Select-Object -First 1
    if (-not $compiler) { throw '未找到 Windows .NET Framework 编译器，无法生成无窗口启动器。' }
    $sourceFile = Join-Path $PSScriptRoot 'agent-background-launcher.cs'
    if (-not (Test-Path -LiteralPath $sourceFile -PathType Leaf)) { throw '缺少 agent-background-launcher.cs，请下载完整的新版服务脚本。' }
    & $compiler /nologo /optimize+ /platform:anycpu /target:winexe ("/out:" + $Destination) $sourceFile
    if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $Destination -PathType Leaf)) { throw '无窗口启动器编译失败。' }
}

switch ($Action) {
    'status' {
        if ($null -eq $task) {
            Write-Host 'CodeGate Agent 开机任务尚未安装。'
            exit 0
        }
        $info = Get-ScheduledTaskInfo -InputObject $task
        Write-Host "任务状态: $($task.State)"
        Write-Host "运行账号: $($task.Principal.UserId)"
        Write-Host "登录方式: $($task.Principal.LogonType)（Password 为非交互后台运行）"
        Write-Host "启动入口: $($task.Actions.Execute)"
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

if ($Action -eq 'repair') {
    if ($null -eq $task) { throw '未找到 CodeGate Agent 任务，请先运行 install。' }
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $taskUser = $task.Principal.UserId
    $taskSid = if ($taskUser -match '^S-1-') { $taskUser } else { ([Security.Principal.NTAccount]::new($taskUser)).Translate([Security.Principal.SecurityIdentifier]).Value }
    if ($taskSid -ne $identity.User.Value) { throw '只能修复当前账号自己的 Agent 任务，未修改其他账号的任务。' }
    if ($task.Actions.Count -ne 1) { throw '旧任务包含多个动作，请使用 install 重新安装标准任务。' }
    $oldAction = $task.Actions[0]
    $name = [IO.Path]::GetFileName($oldAction.Execute)
    if ($name -eq 'codegate-agent.exe') {
        $match = [regex]::Match($oldAction.Arguments, '^supervise\s+-config\s+"([^"\r\n]+)"$')
        $AgentPath = $oldAction.Execute
    } elseif ($name -eq 'codegate-agent-background.exe') {
        $match = [regex]::Match($oldAction.Arguments, '^-config\s+"([^"\r\n]+)"$')
        $AgentPath = Join-Path ([IO.Path]::GetDirectoryName($oldAction.Execute)) 'codegate-agent.exe'
    } else { throw '旧任务不是标准 Agent 入口，请使用 install 重新安装。' }
    if (-not $match.Success) { throw '不能安全读取旧任务的配置路径，请使用 install 重新安装。' }
    $ConfigPath = $match.Groups[1].Value
}

if (-not (Test-Path -LiteralPath $AgentPath -PathType Leaf)) {
    throw "Agent 程序不存在: $AgentPath。请先在仓库根目录构建 bin\codegate-agent.exe。"
}
if (-not (Test-Path -LiteralPath $ConfigPath -PathType Leaf)) {
    throw "配置文件不存在: $ConfigPath。请先配置并完成 pair。"
}

$source = (Resolve-Path -LiteralPath $AgentPath).ProviderPath
$config = (Resolve-Path -LiteralPath $ConfigPath).ProviderPath
$configData = Get-Content -LiteralPath $config -Raw -Encoding UTF8 | ConvertFrom-Json
if ($configData.state_dir -and -not [IO.Path]::IsPathRooted($configData.state_dir)) {
    throw 'state_dir 必须是绝对路径，避免计划任务启动时生成另一把设备密钥。'
}
if ($env:CODEGATE_STATE_DIR) {
    throw '请把 CODEGATE_STATE_DIR 改写到 agent.json 的绝对 state_dir 路径后重新配对。'
}

# Keep the executable at a stable path, independent of the source checkout.
$installDir = Join-Path $env:LOCALAPPDATA 'CodeGate\bin'
$installedAgent = Join-Path $installDir 'codegate-agent.exe'
$installedLauncher = Join-Path $installDir 'codegate-agent-background.exe'
New-Item -ItemType Directory -Path $installDir -Force | Out-Null
$bootstrapTicket = Join-Path $installDir 'background-bootstrap.json'
if ($Action -eq 'repair' -and (Test-Path -LiteralPath $bootstrapTicket -PathType Leaf)) {
    $bootstrap = Get-Content -LiteralPath $bootstrapTicket -Raw -Encoding UTF8 | ConvertFrom-Json
    $candidate = [IO.Path]::GetFullPath([string]$bootstrap.path)
    $expected = [string]$bootstrap.sha256
    if ($expected -notmatch '^[a-f0-9]{64}$' -or
        -not [string]::Equals([IO.Path]::GetDirectoryName($candidate), $installDir, [StringComparison]::OrdinalIgnoreCase) -or
        [IO.Path]::GetFileName($candidate) -ne ('codegate-agent-supervisor-' + $expected + '.exe') -or
        (Get-FileHash -LiteralPath $candidate -Algorithm SHA256).Hash.ToLowerInvariant() -ne $expected) {
        throw '暂存守护进程校验失败，未停止原任务。请重新获取新版 Windows 安装命令。'
    }
    $source = $candidate
}
$sourceVersion = (& $source version | Out-String).Trim()
if ($LASTEXITCODE -ne 0 -or $sourceVersion -notmatch '^codegate-agent (v[0-9]+\.[0-9]+\.[0-9]+)$' -or [Version]($Matches[1].Substring(1)) -lt [Version]'0.1.17') {
    throw '本机守护进程过旧或版本不明，请先运行新版 Server 提供的 Windows 安装命令以暂存新版程序。'
}
& $source doctor -config $config
if ($LASTEXITCODE -ne 0) { throw 'Agent 自检失败，未停止原任务。请先修复配置或终端环境。' }

# Compile before stopping a working Agent. Failure leaves its task intact.
$stagedLauncher = Join-Path $installDir ('launcher-' + [Guid]::NewGuid().ToString('N') + '.exe')
try { New-BackgroundLauncher $stagedLauncher } catch { Remove-Item -LiteralPath $stagedLauncher -ErrorAction SilentlyContinue; throw }

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

Move-Item -LiteralPath $stagedLauncher -Destination $installedLauncher -Force
$scheduledAction = New-ScheduledTaskAction -Execute $installedLauncher -Argument "-config `"$config`"" -WorkingDirectory $installDir

if ($Action -eq 'repair') {
    # Changing only the action retains the existing account, stored credential,
    # triggers and permissions. No Windows password is read or requested.
    Set-ScheduledTask -TaskName $taskName -TaskPath $taskPath -Action $scheduledAction | Out-Null
    Start-ScheduledTask -TaskName $taskName -TaskPath $taskPath
    Write-Host '已切换为无窗口入口；原登录方式、配置及设备身份保留。'
    Write-Host "后台诊断日志: $(Join-Path $installDir '..\logs\agent-background.log')"
    exit 0
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
    $trigger = New-ScheduledTaskTrigger -AtStartup
    $settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit (New-TimeSpan -Seconds 0) -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) -MultipleInstances IgnoreNew -StartWhenAvailable -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
    # Password logon is non-interactive. 'Hidden' only hides a task in the UI
    # and does not prevent console windows, so never rely on that setting.
    $principal = New-ScheduledTaskPrincipal -UserId $currentUser -LogonType Password -RunLevel Limited
    $definition = New-ScheduledTask -Action $scheduledAction -Trigger $trigger -Settings $settings -Principal $principal -Description 'CodeGate Agent (non-interactive background startup)'
    Register-ScheduledTask -TaskName $taskName -TaskPath $taskPath -InputObject $definition -User $currentUser -Password $plainPassword -Force | Out-Null
    $registered = Get-ScheduledTask -TaskName $taskName -TaskPath $taskPath
    if ($registered.Principal.LogonType -ne 'Password') {
        throw '任务未以非交互后台方式注册，未启动 Agent。请检查计划任务登录方式。'
    }
}
finally {
    $plainPassword = $null
    $credential = $null
}

Start-ScheduledTask -TaskName $taskName -TaskPath $taskPath
Write-Host '开机任务已安装并启动，无窗口入口在登录桌面前也会尝试连接 Server。'
Write-Host "后台诊断日志: $(Join-Path $installDir '..\logs\agent-background.log')"
Write-Host '查询状态: powershell.exe -NoProfile -ExecutionPolicy Bypass -File .\scripts\agent-autostart.ps1 status'
Write-Host '请在 Server 的设备页确认 Agent 已上线；首次还需在这台机器上验证一次锁屏/重启场景。'
