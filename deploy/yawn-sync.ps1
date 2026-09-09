# Register a daily yawn sync with Windows Task Scheduler.
#
#   powershell -ExecutionPolicy Bypass -File deploy\yawn-sync.ps1 -WorkDir C:\yawn
#
# Remove it again with:
#   Unregister-ScheduledTask -TaskName yawn-sync -Confirm:$false

param(
    [Parameter(Mandatory = $true)][string]$WorkDir,
    [string]$Binary = "yawn.exe",
    [string]$At = "04:00",
    [string]$TaskName = "yawn-sync"
)

$ErrorActionPreference = "Stop"

if (-not (Test-Path $WorkDir)) {
    throw "work directory $WorkDir does not exist"
}

$action = New-ScheduledTaskAction -Execute $Binary -Argument "sync" -WorkingDirectory $WorkDir
$trigger = New-ScheduledTaskTrigger -Daily -At $At

# a laptop should still catch up when it was asleep, and a long download must
# not be killed halfway through
$settings = New-ScheduledTaskSettingsSet `
    -StartWhenAvailable `
    -DontStopIfGoingOnBatteries `
    -AllowStartIfOnBatteries `
    -ExecutionTimeLimit (New-TimeSpan -Hours 12) `
    -MultipleInstances IgnoreNew

Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $trigger `
    -Settings $settings -Description "yawn archive sync" -Force | Out-Null

Write-Output "registered $TaskName, daily at $At in $WorkDir"
Write-Output "run it now with: Start-ScheduledTask -TaskName $TaskName"
