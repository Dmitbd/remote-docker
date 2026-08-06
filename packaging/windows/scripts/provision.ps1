[CmdletBinding()]
param(
    [switch]$ConfirmProvisioning,
    [string]$RootfsPath = (Join-Path $PSScriptRoot '..\..\..\dist\remote-docker-rootfs.tar.zst'),
    [string]$RootfsSha256 = '',
    [string]$InstallRoot = (Join-Path $env:LOCALAPPDATA 'RemoteDocker'),
    [string]$AgentExecutable = (Join-Path $env:LOCALAPPDATA 'RemoteDocker\RemoteDockerAgent.exe'),
    [ValidateRange(1024, 65535)]
    [int]$SshBridgePort = 49222,
    [ValidateRange(1024, 65535)]
    [int]$SyncthingBridgePort = 49220
)

$ErrorActionPreference = 'Stop'
$managedDistroName = 'remote-docker'
$managedRelease = 'remote-docker-managed-v1'
$managedTaskName = 'RemoteDockerAgent'
$firewallRulePrefix = 'Remote Docker Managed'

function Assert-Administrator {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = [Security.Principal.WindowsPrincipal]::new($identity)
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw 'Provisioning requires an elevated Administrator session.'
    }
}

function Invoke-External {
    param(
        [Parameter(Mandatory = $true)]
        [string]$FilePath,
        [Parameter(Mandatory = $true)]
        [string[]]$ArgumentList,
        [Parameter(Mandatory = $true)]
        [string]$Description
    )

    & $FilePath @ArgumentList
    if ($LASTEXITCODE -ne 0) {
        throw "$Description failed with exit code $LASTEXITCODE."
    }
}

function Test-ManagedDistro {
    $names = (& wsl.exe --list --quiet 2>&1 | Out-String) -replace "`0", ''
    $exists = $null -ne (($names -split "`r?`n") | Where-Object { $_.Trim() -eq $managedDistroName } | Select-Object -First 1)
    if (-not $exists) {
        return $false
    }

    $release = (& wsl.exe --distribution $managedDistroName --exec cat /etc/remote-docker-release 2>$null | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or $release -ne $managedRelease) {
        throw "WSL distribution name '$managedDistroName' is already used by an unmanaged distribution."
    }
    return $true
}

if (-not $ConfirmProvisioning) {
    throw 'No changes were made. Re-run with -ConfirmProvisioning after reviewing the provisioning plan.'
}

Assert-Administrator

$requiredFeatures = @('Microsoft-Windows-Subsystem-Linux', 'VirtualMachinePlatform')
$disabledFeatures = @()
foreach ($featureName in $requiredFeatures) {
    $feature = Get-WindowsOptionalFeature -Online -FeatureName $featureName
    if ($feature.State -ne 'Enabled') {
        $disabledFeatures += $featureName
    }
}

if ($disabledFeatures.Count -gt 0) {
    foreach ($featureName in $disabledFeatures) {
        Enable-WindowsOptionalFeature -Online -FeatureName $featureName -All -NoRestart | Out-Null
    }
    [ordered]@{
        state = 'reboot_required'
        enabled_features = $disabledFeatures
    } | ConvertTo-Json -Compress
    exit 0
}

if ($null -eq (Get-Command 'wsl.exe' -ErrorAction SilentlyContinue)) {
    throw 'WSL features are enabled, but wsl.exe is unavailable. Install the current WSL runtime and retry.'
}

& wsl.exe --version *> $null
if ($LASTEXITCODE -ne 0) {
    Invoke-External -FilePath 'wsl.exe' -ArgumentList @('--update', '--web-download') -Description 'WSL update'
}

$distroExists = Test-ManagedDistro
$distroRoot = Join-Path $InstallRoot 'wsl'

if (-not $distroExists) {
    New-Item -ItemType Directory -Path $distroRoot -Force | Out-Null

    if ([string]::IsNullOrWhiteSpace($RootfsSha256)) {
        $manifestPath = "$RootfsPath.sha256"
        if (-not (Test-Path -LiteralPath $manifestPath -PathType Leaf)) {
            throw "Rootfs checksum manifest was not found at '$manifestPath'."
        }
        $manifestValue = ((Get-Content -LiteralPath $manifestPath -Raw).Trim() -split '\s+')[0]
        if ($manifestValue -notmatch '^[A-Fa-f0-9]{64}$') {
            throw "Rootfs checksum manifest at '$manifestPath' is invalid."
        }
        $RootfsSha256 = $manifestValue
    }
    elseif ($RootfsSha256 -notmatch '^[A-Fa-f0-9]{64}$') {
        throw 'RootfsSha256 must contain exactly 64 hexadecimal characters.'
    }

    $resolvedRootfs = (Resolve-Path -LiteralPath $RootfsPath).Path
    $actualHash = (Get-FileHash -LiteralPath $resolvedRootfs -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actualHash -ne $RootfsSha256.ToLowerInvariant()) {
        throw "Rootfs SHA-256 mismatch. Expected $($RootfsSha256.ToLowerInvariant()), got $actualHash."
    }

    Invoke-External -FilePath 'wsl.exe' -ArgumentList @(
        '--import', $managedDistroName, $distroRoot, $resolvedRootfs, '--version', '2'
    ) -Description 'Managed WSL import'

    $firstBoot = @'
set -eu
install -d -m 0755 /etc/remote-docker
printf '%s\n' '{"schema_version":1,"managed_by":"remote-docker"}' > /etc/remote-docker/managed.json
systemctl daemon-reload
systemctl enable docker.service remote-docker-remote.service syncthing@remote-docker.service
systemctl start docker.service remote-docker-remote.service syncthing@remote-docker.service
'@
    Invoke-External -FilePath 'wsl.exe' -ArgumentList @(
        '--distribution', $managedDistroName, '--user', 'root', '--exec', '/bin/sh', '-c', $firstBoot
    ) -Description 'Managed WSL first boot'
}

Invoke-External -FilePath 'wsl.exe' -ArgumentList @(
    '--distribution', $managedDistroName, '--exec', '/usr/local/bin/remote-docker-remote', 'health'
) -Description 'Managed WSL health check'

$firewallRules = @(
    @{ Name = "$firewallRulePrefix SSH"; Port = $SshBridgePort },
    @{ Name = "$firewallRulePrefix Syncthing"; Port = $SyncthingBridgePort }
)
foreach ($rule in $firewallRules) {
    Remove-NetFirewallRule -DisplayName $rule.Name -ErrorAction SilentlyContinue
    New-NetFirewallRule `
        -DisplayName $rule.Name `
        -Direction Inbound `
        -Action Allow `
        -Protocol TCP `
        -LocalPort $rule.Port `
        -Profile Private `
        -RemoteAddress LocalSubnet | Out-Null
}

if (-not (Test-Path -LiteralPath $AgentExecutable -PathType Leaf)) {
    throw "Windows Agent executable was not found at '$AgentExecutable'."
}
$taskAction = New-ScheduledTaskAction -Execute $AgentExecutable -Argument '--background'
$taskTrigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME
$taskPrincipal = New-ScheduledTaskPrincipal -UserId $env:USERNAME -LogonType Interactive -RunLevel Limited
Register-ScheduledTask `
    -TaskName $managedTaskName `
    -Action $taskAction `
    -Trigger $taskTrigger `
    -Principal $taskPrincipal `
    -Description 'Starts the Remote Docker Windows bridge for the signed-in user.' `
    -Force | Out-Null

[ordered]@{
    state = $(if ($distroExists) { 'already-ready' } else { 'ready' })
    distro = $managedDistroName
    ssh_bridge_port = $SshBridgePort
    syncthing_bridge_port = $SyncthingBridgePort
} | ConvertTo-Json -Compress
