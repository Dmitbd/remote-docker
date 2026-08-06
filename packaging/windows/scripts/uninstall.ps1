[CmdletBinding(SupportsShouldProcess = $true, ConfirmImpact = 'High')]
param(
    [switch]$DeleteData,
    [string]$InstallRoot = (Join-Path $env:LOCALAPPDATA 'RemoteDocker'),
    [string]$AgentExecutable = (Join-Path $env:LOCALAPPDATA 'RemoteDocker\RemoteDockerAgent.exe')
)

$ErrorActionPreference = 'Stop'
$managedDistroName = 'remote-docker'
$managedTaskName = 'RemoteDockerAgent'
$firewallRulePrefix = 'Remote Docker Managed'

Stop-ScheduledTask -TaskName $managedTaskName -ErrorAction SilentlyContinue
Unregister-ScheduledTask -TaskName $managedTaskName -Confirm:$false -ErrorAction SilentlyContinue
Remove-NetFirewallRule -DisplayName "$firewallRulePrefix SSH" -ErrorAction SilentlyContinue
Remove-NetFirewallRule -DisplayName "$firewallRulePrefix Syncthing" -ErrorAction SilentlyContinue

if (Test-Path -LiteralPath $AgentExecutable -PathType Leaf) {
    Remove-Item -LiteralPath $AgentExecutable -Force
}

if (-not $DeleteData) {
    Write-Output "Windows Agent removed. WSL distribution '$managedDistroName' and data in '$InstallRoot' were preserved."
    exit 0
}

Write-Host "WSL distribution to delete: $managedDistroName"
if ($PSCmdlet.ShouldProcess("WSL distribution '$managedDistroName' and '$InstallRoot'", 'Permanently delete managed Docker data')) {
    & wsl.exe --unregister $managedDistroName
    if ($LASTEXITCODE -ne 0) {
        throw "Failed to unregister WSL distribution '$managedDistroName' with exit code $LASTEXITCODE."
    }
    if (Test-Path -LiteralPath $InstallRoot) {
        Remove-Item -LiteralPath $InstallRoot -Recurse -Force
    }
}
