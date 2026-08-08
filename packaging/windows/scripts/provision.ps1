[CmdletBinding()]
param(
    [switch]$ConfirmProvisioning,
    [ValidateRange(1024, 65535)]
    [int]$SshBridgePort = 49222,
    [ValidateRange(1024, 65535)]
    [int]$SyncthingBridgePort = 49220
)

$ErrorActionPreference = 'Stop'
$managedDistroName = 'remote-docker'
$managedRelease = 'remote-docker-managed-v1'
$managedMetadata = '{"schema_version":1,"managed_by":"remote-docker"}'
$firewallRuleGroup = 'Remote Docker Managed Rules'
$programData = [Environment]::GetFolderPath([Environment+SpecialFolder]::CommonApplicationData)
$programFiles = [Environment]::GetFolderPath([Environment+SpecialFolder]::ProgramFiles)
$installRoot = [System.IO.Path]::GetFullPath((Join-Path $programData 'RemoteDocker'))
$agentExecutable = [System.IO.Path]::GetFullPath((Join-Path $programFiles 'Remote Docker\RemoteDockerAgent.exe'))
$rootfsPath = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\assets\remote-docker-rootfs.tar.zst'))
$rootfsChecksumPath = "$rootfsPath.sha256"

function Assert-Administrator {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = [Security.Principal.WindowsPrincipal]::new($identity)
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw 'Provisioning requires an elevated Administrator session.'
    }
}

function Assert-NoReparseDirectory {
    param([Parameter(Mandatory = $true)][string]$Path)

    if (-not (Test-Path -LiteralPath $Path -PathType Container)) {
        throw "Managed directory was not found at '$Path'."
    }
    $item = Get-Item -LiteralPath $Path -Force
    if (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "Refusing to use a reparse point at '$Path'."
    }
    $resolved = (Resolve-Path -LiteralPath $Path -ErrorAction Stop).Path
    $canonical = [System.IO.Path]::GetFullPath($Path)
    if (-not [string]::Equals($resolved, $canonical, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "Resolved managed directory does not match '$canonical'."
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
$firstBootRequired = -not $distroExists
$distroRoot = Join-Path $installRoot 'wsl'

if ($distroExists) {
    $installedMetadataOutput = & wsl.exe --distribution $managedDistroName --user root --exec cat /etc/remote-docker/managed.json 2>$null
    $metadataExitCode = $LASTEXITCODE
    $installedMetadata = ($installedMetadataOutput | Out-String).Trim()
    $firstBootRequired = $metadataExitCode -ne 0 -or $installedMetadata -ne $managedMetadata
}

if (-not (Test-Path -LiteralPath $installRoot)) {
    New-Item -ItemType Directory -Path $installRoot -Force | Out-Null
}
Assert-NoReparseDirectory -Path $installRoot

if (-not $distroExists) {
    if (-not (Test-Path -LiteralPath $distroRoot)) {
        New-Item -ItemType Directory -Path $distroRoot -Force | Out-Null
    }
    Assert-NoReparseDirectory -Path $distroRoot

    if (-not (Test-Path -LiteralPath $rootfsChecksumPath -PathType Leaf)) {
        throw "Rootfs checksum manifest was not found at '$rootfsChecksumPath'."
    }
    $rootfsSha256 = ((Get-Content -LiteralPath $rootfsChecksumPath -Raw).Trim() -split '\s+')[0]
    if ($rootfsSha256 -notmatch '^[A-Fa-f0-9]{64}$') {
        throw "Rootfs checksum manifest at '$rootfsChecksumPath' is invalid."
    }

    $resolvedRootfs = (Resolve-Path -LiteralPath $rootfsPath -ErrorAction Stop).Path
    if (-not [string]::Equals($resolvedRootfs, $rootfsPath, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "Resolved rootfs path does not match the packaged path '$rootfsPath'."
    }
    $actualHash = (Get-FileHash -LiteralPath $resolvedRootfs -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actualHash -ne $rootfsSha256.ToLowerInvariant()) {
        throw "Rootfs SHA-256 mismatch. Expected $($rootfsSha256.ToLowerInvariant()), got $actualHash."
    }

    Invoke-External -FilePath 'wsl.exe' -ArgumentList @(
        '--import', $managedDistroName, $distroRoot, $resolvedRootfs, '--version', '2'
    ) -Description 'Managed WSL import'
}

if ($firstBootRequired) {
    $firstBoot = @(
        'set -eu'
        'install -d -m 0755 /etc/remote-docker'
        'systemctl daemon-reload'
        'systemctl disable ssh.socket ssh.service syncthing@remote-docker.service remote-docker.target >/dev/null 2>&1 || true'
        "printf '%s\n' '$managedMetadata' > /etc/remote-docker/managed.json.tmp"
        'chmod 0644 /etc/remote-docker/managed.json.tmp'
        'mv -f /etc/remote-docker/managed.json.tmp /etc/remote-docker/managed.json'
    ) -join "`n"
    Invoke-External -FilePath 'wsl.exe' -ArgumentList @(
        '--distribution', $managedDistroName, '--user', 'root', '--exec', '/bin/sh', '-c', $firstBoot
    ) -Description 'Managed WSL first boot'
}

if (-not (Test-Path -LiteralPath $agentExecutable -PathType Leaf)) {
    throw "Windows Agent executable was not found at '$agentExecutable'."
}

Invoke-External -FilePath $agentExecutable -ArgumentList @('--prepare-wsl') -Description 'Managed WSL identity preparation'
Invoke-External -FilePath 'wsl.exe' -ArgumentList @(
    '--distribution', $managedDistroName, '--user', 'root', '--exec',
    '/usr/local/bin/remote-docker-remote', 'runtime-status'
) -Description 'Managed WSL health check'

$firewallRules = @(
    @{ Name = 'RemoteDocker.Managed.SSH'; DisplayName = 'Remote Docker Managed SSH'; Port = $SshBridgePort },
    @{ Name = 'RemoteDocker.Managed.Syncthing'; DisplayName = 'Remote Docker Managed Syncthing'; Port = $SyncthingBridgePort }
)
foreach ($rule in $firewallRules) {
    $existingRule = Get-NetFirewallRule -Name $rule.Name -ErrorAction SilentlyContinue
    if ($null -ne $existingRule) {
        if ($existingRule.Group -ne $firewallRuleGroup) {
            throw "Refusing to replace the foreign firewall rule '$($rule.Name)'."
        }
        Remove-NetFirewallRule -InputObject $existingRule
    }
    New-NetFirewallRule `
        -Name $rule.Name `
        -DisplayName $rule.DisplayName `
        -Group $firewallRuleGroup `
        -Direction Inbound `
        -Action Allow `
        -Program $agentExecutable `
        -Protocol TCP `
        -LocalPort $rule.Port `
        -Profile Private `
        -RemoteAddress LocalSubnet | Out-Null
}

[ordered]@{
    state = $(if ($distroExists) { 'already-ready' } else { 'ready' })
    distro = $managedDistroName
    ssh_bridge_port = $SshBridgePort
    syncthing_bridge_port = $SyncthingBridgePort
} | ConvertTo-Json -Compress
