#Requires -Version 5.1
$ErrorActionPreference = 'Stop'
$compiler = @((Join-Path $env:WINDIR 'Microsoft.NET\Framework64\v4.0.30319\csc.exe'), (Join-Path $env:WINDIR 'Microsoft.NET\Framework\v4.0.30319\csc.exe')) | Where-Object { Test-Path -LiteralPath $_ } | Select-Object -First 1
$root = Join-Path ([IO.Path]::GetTempPath()) ('codegate-background-' + [Guid]::NewGuid().ToString('N'))
$bin = Join-Path $root 'Code Gate 中文 & fixture\bin'
New-Item -ItemType Directory -Path $bin -Force | Out-Null
$launcher = Join-Path $bin 'codegate-agent-background.exe'
& $compiler /nologo /platform:anycpu /target:winexe ("/out:" + $launcher) (Join-Path $PSScriptRoot 'agent-background-launcher.cs')
if ($LASTEXITCODE -ne 0) { throw 'Launcher compile failed' }
$stream = [IO.File]::OpenRead($launcher)
$reader = [IO.BinaryReader]::new($stream)
try {
    $stream.Position = 0x3c; $pe = $reader.ReadInt32()
    $stream.Position = $pe + 24 + 68
    if ($reader.ReadUInt16() -ne 2) { throw 'Outermost launcher is not Windows GUI subsystem' }
} finally { $reader.Dispose() }
$fixtureSource = Join-Path $root 'fixture.cs'
@'
using System;using System.IO;using System.Diagnostics;using System.Runtime.InteropServices;using System.Threading;using System.Text;
class Fixture {
 [DllImport("kernel32.dll")]static extern IntPtr GetConsoleWindow();
 static int Main(string[] args) {
  Console.OutputEncoding=Encoding.UTF8;
  if(args[0]=="doctor")return 0;
  if(args[0]=="version"){Console.WriteLine("codegate-agent v0.1.26");return 0;}
  bool child=args[0]=="child";string config=args[child?1:2];
  File.WriteAllText(config+(child?".childconsole":".parent"),Process.GetCurrentProcess().Id+":"+GetConsoleWindow().ToInt64());
  if(child){Thread.Sleep(60000);return 0;}
  Console.Error.WriteLine("fixture update fallback diagnostic 中文");
  if(config.EndsWith("exit.json"))return 23;
  var p=Process.Start(new ProcessStartInfo(Process.GetCurrentProcess().MainModule.FileName,"child \""+config+"\""){UseShellExecute=false,CreateNoWindow=true});
  File.WriteAllText(config+".child",p.Id.ToString());Thread.Sleep(60000);return 0;
 }
}
'@ | Set-Content -LiteralPath $fixtureSource -Encoding UTF8
& $compiler /nologo /platform:anycpu /target:exe ("/out:" + (Join-Path $bin 'codegate-agent.exe')) $fixtureSource
if ($LASTEXITCODE -ne 0) { throw 'Fixture compile failed' }
function Wait-Check([scriptblock]$Check) {
 $deadline = [DateTime]::UtcNow.AddSeconds(12)
 while ([DateTime]::UtcNow -lt $deadline) { if (& $Check) { return }; Start-Sleep -Milliseconds 100 }
 throw ('Background process condition timed out; caller line ' + (Get-PSCallStack)[1].ScriptLineNumber)
}
$hostProcess = $null
try {
 $config = Join-Path $root '配置 & long.json'; '{}' | Set-Content -LiteralPath $config
 $hostProcess = Start-Process -FilePath $launcher -ArgumentList @('-config', ('"' + $config + '"')) -PassThru
 Wait-Check { Test-Path -LiteralPath ($config + '.childconsole') }
 $parent = (Get-Content -LiteralPath ($config + '.parent') -Raw).Split(':'); $child = (Get-Content -LiteralPath ($config + '.childconsole') -Raw).Split(':')
 if ($parent[1] -ne '0' -or $child[1] -ne '0') { throw 'Agent or descendant allocated a console' }
 $log = Join-Path $bin '..\logs\agent-background.log'
 Wait-Check { (Test-Path -LiteralPath $log) -and ((Get-Content -LiteralPath $log -Raw -Encoding UTF8) -match 'diagnostic 中文') }
 Stop-Process -Id $hostProcess.Id -Force
 Wait-Check { $null -eq (Get-Process -Id ([int]$parent[0]) -ErrorAction SilentlyContinue) -and $null -eq (Get-Process -Id ([int]$child[0]) -ErrorAction SilentlyContinue) }
 $hostProcess = $null
 $config = Join-Path $root 'exit.json'; '{}' | Set-Content -LiteralPath $config
 $hostProcess = Start-Process -FilePath $launcher -ArgumentList @('-config', ('"' + $config + '"')) -PassThru
 if (-not $hostProcess.WaitForExit(12000)) { throw 'Launcher failed to finish with Agent' }
 if ($hostProcess.ExitCode -ne 23) { throw "Agent exit code not preserved: $($hostProcess.ExitCode)" }
 # Exercise the actual repair command against an owned standard legacy task.
 # S4U avoids a password prompt in CI; repair must preserve
 # that logon mode and its trigger while changing only the executable action.
 if (Get-ScheduledTask -TaskName 'CodeGate Agent' -TaskPath '\' -ErrorAction SilentlyContinue) { throw 'Unexpected pre-existing task; refusing fixture takeover' }
 $savedLocalAppData = $env:LOCALAPPDATA
 $env:LOCALAPPDATA = Join-Path $root 'local'
 $taskInstalled = $false
 try {
  $repairConfig = Join-Path $root 'repair.json'
  @{state_dir=(Join-Path $root 'private-state')} | ConvertTo-Json | Set-Content -LiteralPath $repairConfig -Encoding UTF8
  $user = [Security.Principal.WindowsIdentity]::GetCurrent().Name
  $oldAction = New-ScheduledTaskAction -Execute (Join-Path $bin 'codegate-agent.exe') -Argument ('supervise -config "' + $repairConfig + '"')
  $principal = New-ScheduledTaskPrincipal -UserId $user -LogonType S4U -RunLevel Limited
  $trigger = New-ScheduledTaskTrigger -AtStartup
  Register-ScheduledTask -TaskName 'CodeGate Agent' -TaskPath '\' -InputObject (New-ScheduledTask -Action $oldAction -Principal $principal -Trigger $trigger) | Out-Null
  $taskInstalled = $true
  & powershell.exe -NoProfile -ExecutionPolicy Bypass -File (Join-Path $PSScriptRoot 'agent-autostart.ps1') repair
  if ($LASTEXITCODE -ne 0) { throw 'Repair command failed' }
  $repaired = Get-ScheduledTask -TaskName 'CodeGate Agent' -TaskPath '\'
  if ($repaired.Principal.LogonType -ne 'S4U' -or $repaired.Triggers.Count -ne 1 -or [IO.Path]::GetFileName($repaired.Actions.Execute) -ne 'codegate-agent-background.exe') { throw 'Repair changed principal/trigger or kept console entry' }
  Wait-Check { Test-Path -LiteralPath ($repairConfig + '.childconsole') }
  $parent=(Get-Content -LiteralPath ($repairConfig+'.parent') -Raw).Split(':');$child=(Get-Content -LiteralPath ($repairConfig+'.childconsole') -Raw).Split(':')
  if ($parent[1] -ne '0' -or $child[1] -ne '0') { throw 'Repaired scheduled task allocated a console' }
  Stop-ScheduledTask -InputObject $repaired
  Wait-Check { $null -eq (Get-Process -Id ([int]$parent[0]) -ErrorAction SilentlyContinue) -and $null -eq (Get-Process -Id ([int]$child[0]) -ErrorAction SilentlyContinue) }
 } finally {
  if ($taskInstalled) {Stop-ScheduledTask -TaskName 'CodeGate Agent' -TaskPath '\' -ErrorAction SilentlyContinue;Unregister-ScheduledTask -TaskName 'CodeGate Agent' -TaskPath '\' -Confirm:$false}
  $env:LOCALAPPDATA = $savedLocalAppData
 }
 # Verify Password task changes authenticate locally and retain logon mode. Use only
 # an ephemeral CI-owned local account; no existing user's password is read.
 $fixtureUser = 'cg-bg-' + [Guid]::NewGuid().ToString('N').Substring(0,10)
 $passwordTask = 'CodeGate Credential Fixture ' + $fixtureUser
 $accountCreated = $false
 $credentialTaskCreated = $false
 try {
  $bytes = New-Object byte[] 24
  $rng = [Security.Cryptography.RandomNumberGenerator]::Create()
  try { $rng.GetBytes($bytes) } finally { $rng.Dispose() }
  $fixturePassword = [Convert]::ToBase64String($bytes) + 'Aa1!'
  $securePassword = ConvertTo-SecureString $fixturePassword -AsPlainText -Force
  New-LocalUser -Name $fixtureUser -Password $securePassword -AccountNeverExpires -PasswordNeverExpires | Out-Null
  $accountCreated = $true
  # The fixture needs the runner's standard batch-logon/command access; this
  # isolated test account is removed below, without changing existing users.
  $adminGroup = Get-LocalGroup -SID ([Security.Principal.SecurityIdentifier]'S-1-5-32-544')
  Add-LocalGroupMember -Group $adminGroup.Name -Member $fixtureUser
  $usersGroup = Get-LocalGroup -SID ([Security.Principal.SecurityIdentifier]'S-1-5-32-545')
  $fixtureSid = (Get-LocalUser -Name $fixtureUser).SID
  if (-not (Get-LocalGroupMember -Group $usersGroup.Name | Where-Object { $_.SID -eq $fixtureSid })) { Add-LocalGroupMember -Group $usersGroup.Name -Member $fixtureUser }
  $account = $env:COMPUTERNAME + '\' + $fixtureUser
  $principal = New-ScheduledTaskPrincipal -UserId $account -LogonType Password -RunLevel Limited
  $action = New-ScheduledTaskAction -Execute (Join-Path $env:WINDIR 'System32\cmd.exe') -Argument '/c exit 0'
  Register-ScheduledTask -TaskName $passwordTask -InputObject (New-ScheduledTask -Action $action -Principal $principal) -User $account -Password $fixturePassword | Out-Null
  $credentialTaskCreated = $true
  $nextAction = New-ScheduledTaskAction -Execute (Join-Path $env:WINDIR 'System32\cmd.exe') -Argument '/c exit 23'
  $rejected = $false
  try { Set-ScheduledTask -TaskName $passwordTask -Action $nextAction -ErrorAction Stop | Out-Null } catch { $rejected = $true }
  if (-not $rejected) { throw 'Credential fixture expected Windows to require authentication' }
  Set-ScheduledTask -TaskName $passwordTask -Action $nextAction -User $account -Password $fixturePassword | Out-Null
  $fixturePassword = $null; $securePassword = $null
  if ((Get-ScheduledTask -TaskName $passwordTask).Principal.LogonType -ne 'Password') { throw 'Password logon changed during action repair' }
  Start-ScheduledTask -TaskName $passwordTask
  try { Wait-Check { (Get-ScheduledTaskInfo -TaskName $passwordTask).LastTaskResult -eq 23 } } catch {
    $taskInfo = Get-ScheduledTaskInfo -TaskName $passwordTask
    throw ('Password fixture execution failed: LastTaskResult=' + $taskInfo.LastTaskResult + '; LastRunTime=' + $taskInfo.LastRunTime)
  }
 } finally {
  $fixturePassword = $null; $securePassword = $null
  if ($credentialTaskCreated) { Stop-ScheduledTask -TaskName $passwordTask -ErrorAction SilentlyContinue; Unregister-ScheduledTask -TaskName $passwordTask -Confirm:$false }
  if ($accountCreated) { Remove-LocalUser -Name $fixtureUser }
 }
 Write-Host 'PASS GUI subsystem; no console in Agent/descendant; UTF-8 diagnostics; paths with spaces/Chinese/&; root termination kills owned tree; exit status preserved'
} finally {
 if ($null -ne $hostProcess -and -not $hostProcess.HasExited) { Stop-Process -Id $hostProcess.Id -Force }
 Remove-Item -LiteralPath $root -Recurse -Force
}
