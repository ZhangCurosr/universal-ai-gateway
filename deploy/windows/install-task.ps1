<#
.SYNOPSIS
    Register Universal AI Gateway as a scheduled task that starts at boot (spec/12 section 5.2).

.DESCRIPTION
    The M1 binary is not a Windows service, so Task Scheduler runs
    "<InstallDir>\bin\gateway.exe serve --config <InstallDir>\gateway.yaml" as SYSTEM at startup
    and restarts it every minute if it exits.

    Run this script from an elevated ("Run as administrator") PowerShell.
    When running as SYSTEM, the directories of data_dir and master_key_file in gateway.yaml
    must be writable by SYSTEM.
    The BOOTSTRAP ADMIN TOKEN is printed only once, on the first start: run
    "gateway.exe serve --config ..." once in the foreground to record it, then register the task.

    This file is intentionally ASCII-only: Windows PowerShell 5.1 reads BOM-less UTF-8 scripts
    with the ANSI code page, and non-ASCII text would break parsing.

.PARAMETER InstallDir
    Installation directory (default C:\uaig). Must contain bin\gateway.exe and gateway.yaml.

.PARAMETER TaskName
    Scheduled task name (default UniversalAIGateway).

.EXAMPLE
    .\install-task.ps1 -InstallDir D:\uaig
#>
[CmdletBinding()]
param(
    [string]$InstallDir = 'C:\uaig',
    [string]$TaskName = 'UniversalAIGateway'
)

$ErrorActionPreference = 'Stop'

# 1. Administrator check: registering a task with -User SYSTEM requires elevation.
$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    [Console]::Error.WriteLine('install-task.ps1: administrator rights are required. Right-click PowerShell, choose "Run as administrator", and try again.')
    exit 1
}

# 2. Check the installation directory.
$exe = Join-Path $InstallDir 'bin\gateway.exe'
$config = Join-Path $InstallDir 'gateway.yaml'
foreach ($path in @($exe, $config)) {
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        [Console]::Error.WriteLine("install-task.ps1: file not found: $path")
        exit 1
    }
}

# 3. Validate the config first, so we never register a task that fails and restarts forever.
& $exe check-config --config $config
if ($LASTEXITCODE -ne 0) {
    [Console]::Error.WriteLine('install-task.ps1: config validation failed; the scheduled task was not registered.')
    exit 1
}

# 4. Register (or replace) the scheduled task.
$action = New-ScheduledTaskAction -Execute $exe -Argument "serve --config `"$config`"" -WorkingDirectory $InstallDir
$trigger = New-ScheduledTaskTrigger -AtStartup
$settings = New-ScheduledTaskSettingsSet -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit ([TimeSpan]::Zero)
Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger -Settings $settings -User 'SYSTEM' -RunLevel Highest -Force | Out-Null

Write-Output "Registered scheduled task '$TaskName': runs $exe at startup."
Write-Output "Start now:  Start-ScheduledTask -TaskName '$TaskName'"
Write-Output "Uninstall:  Unregister-ScheduledTask -TaskName '$TaskName' -Confirm:`$false"
exit 0
