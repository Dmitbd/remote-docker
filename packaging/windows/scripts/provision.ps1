[CmdletBinding()]
param(
    [switch]$ConfirmProvisioning,
    [Parameter(Mandatory = $true)][string]$ApplicationRoot,
    [Parameter(Mandatory = $true)][string]$DataRoot,
    [Parameter(Mandatory = $true)][string]$ProgressPath,
    [Parameter(Mandatory = $true)][string]$LogPath,
    [ValidateRange(1024, 65535)][int]$SshBridgePort = 49222,
    [ValidateRange(1024, 65535)][int]$SyncthingBridgePort = 49220
)

$ErrorActionPreference = 'Stop'
$managedDistroName = 'remote-docker'
$managedRelease = 'remote-docker-managed-v1'
$managedMetadata = '{"schema_version":1,"managed_by":"remote-docker"}'
$firewallRuleGroup = 'Remote Docker Managed Rules'

. (Join-Path $PSScriptRoot 'provision-status.ps1')
. (Join-Path $PSScriptRoot 'path-validation.ps1')

function Assert-Administrator {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = [Security.Principal.WindowsPrincipal]::new($identity)
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw 'Provisioning requires an elevated Administrator session.'
    }
}

function Assert-ManagedDirectory {
    param([Parameter(Mandatory = $true)][string]$Path)

    if (-not (Test-Path -LiteralPath $Path -PathType Container)) {
        throw "Managed directory was not found at '$Path'."
    }
    $item = Get-Item -LiteralPath $Path -Force
    if (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "Refusing to use a reparse point at '$Path'."
    }
    $resolved = (Resolve-Path -LiteralPath $Path -ErrorAction Stop).Path
    if (-not [string]::Equals($resolved.TrimEnd('\'), $Path.TrimEnd('\'), [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "Resolved managed directory does not match '$Path'."
    }
}

function Write-InstallLog {
    param([Parameter(Mandatory = $true)][string]$Message)

    $line = '[{0}] {1}' -f [DateTimeOffset]::UtcNow.ToString('O'), $Message
    Add-Content -LiteralPath $LogPath -Value $line -Encoding utf8
}

function Invoke-External {
    param(
        [Parameter(Mandatory = $true)][string]$FilePath,
        [Parameter(Mandatory = $true)][string[]]$ArgumentList,
        [Parameter(Mandatory = $true)][string]$Description,
        [switch]$IgnoreFailure
    )

    Write-InstallLog -Message "$Description started."
    & $FilePath @ArgumentList *> $null
    $exitCode = $LASTEXITCODE
    if ($exitCode -ne 0 -and -not $IgnoreFailure) {
        throw "$Description failed with exit code $exitCode."
    }
    Write-InstallLog -Message "$Description completed with exit code $exitCode."
    $exitCode
}

function Invoke-WslCapture {
    param([Parameter(Mandatory = $true)][string[]]$ArgumentList)

    $previousErrorActionPreference = $ErrorActionPreference
    try {
        $ErrorActionPreference = 'Continue'
        $output = (& wsl.exe @ArgumentList 2>$null | Out-String)
        $exitCode = $LASTEXITCODE
    }
    finally {
        $ErrorActionPreference = $previousErrorActionPreference
    }
    [PSCustomObject]@{ Output = $output; ExitCode = $exitCode }
}

function Test-ManagedDistro {
    $list = Invoke-WslCapture -ArgumentList @('--list', '--quiet')
    if ($list.ExitCode -ne 0) {
        return $false
    }
    $names = $list.Output -replace "`0", ''
    $exists = $null -ne (($names -split "`r?`n") | Where-Object { $_.Trim() -eq $managedDistroName } | Select-Object -First 1)
    if (-not $exists) {
        return $false
    }
    $releaseProbe = Invoke-WslCapture -ArgumentList @('--distribution', $managedDistroName, '--exec', 'cat', '/etc/remote-docker-release')
    if ($releaseProbe.ExitCode -ne 0 -or $releaseProbe.Output.Trim() -ne $managedRelease) {
        throw "WSL distribution name '$managedDistroName' is already used by an unmanaged distribution."
    }
    return $true
}

$logReady = $false
$progressReady = $false

try {
    if (-not $ConfirmProvisioning) {
        throw 'No changes were made. The installer must explicitly confirm provisioning.'
    }

    Assert-Administrator
    $ApplicationRoot = Assert-RemoteDockerCanonicalPath -Path $ApplicationRoot -Description 'Application root'
    $DataRoot = Assert-RemoteDockerCanonicalPath -Path $DataRoot -Description 'Data root'
    $ProgressPath = Assert-RemoteDockerCanonicalPath -Path $ProgressPath -Description 'Progress path'
    $LogPath = Assert-RemoteDockerCanonicalPath -Path $LogPath -Description 'Log path'
    if ([string]::Equals($ApplicationRoot.TrimEnd('\'), $DataRoot.TrimEnd('\'), [System.StringComparison]::OrdinalIgnoreCase)) {
        throw 'Application and data roots must be different.'
    }
    Assert-ManagedDirectory -Path $ApplicationRoot
    if (-not (Test-Path -LiteralPath $DataRoot)) {
        New-Item -ItemType Directory -Path $DataRoot -Force | Out-Null
    }
    Assert-ManagedDirectory -Path $DataRoot
    if ((Split-Path -Parent $ProgressPath) -ne $DataRoot -or (Split-Path -Parent $LogPath) -ne $DataRoot) {
        throw 'Installer status and log files must be direct children of the selected data root.'
    }
    if (-not (Test-Path -LiteralPath $LogPath -PathType Leaf)) {
        Set-Content -LiteralPath $LogPath -Value '' -Encoding utf8
    }
    $logReady = $true
    Write-InstallLog -Message 'Provisioning started.'
    Set-Content -LiteralPath $ProgressPath -Value '' -Encoding utf8
    $progressReady = $true
    $dataMarker = Join-Path $DataRoot '.remote-docker-managed-data'
    $dataMarkerValue = 'remote-docker-managed-data-v1'
    if (Test-Path -LiteralPath $dataMarker -PathType Leaf) {
        if ((Get-Content -LiteralPath $dataMarker -Raw).Trim() -ne $dataMarkerValue) {
            throw 'The selected data root contains an invalid ownership marker.'
        }
    }
    else {
        Set-Content -LiteralPath $dataMarker -Value $dataMarkerValue -Encoding ascii
    }

    $desktopExecutable = Join-Path $ApplicationRoot 'RemoteDocker.exe'
    $rootfsPath = Join-Path $ApplicationRoot 'assets\remote-docker-rootfs.tar.zst'
    $rootfsChecksumPath = "$rootfsPath.sha256"
    $distroRoot = Join-Path $DataRoot 'wsl'
    if (-not (Test-Path -LiteralPath $desktopExecutable -PathType Leaf)) {
        throw "Remote Docker executable was not found at '$desktopExecutable'."
    }

    Write-RemoteDockerProvisionStatus -ProgressPath $ProgressPath -Phase 'preflight' -State 'started' -Message 'Checking Windows components.'
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
        Write-RemoteDockerProvisionStatus -ProgressPath $ProgressPath -Phase 'preflight' -State 'reboot_required' -Message 'Windows components require a reboot.'
        Write-InstallLog -Message 'Windows components enabled; reboot required.'
        [Environment]::Exit(3010)
    }

    if ($null -eq (Get-Command 'wsl.exe' -ErrorAction SilentlyContinue)) {
        throw 'WSL features are enabled, but wsl.exe is unavailable.'
    }
    & wsl.exe --version *> $null
    if ($LASTEXITCODE -ne 0) {
        Invoke-External -FilePath 'wsl.exe' -ArgumentList @('--update', '--web-download') -Description 'WSL update' | Out-Null
    }
    Write-RemoteDockerProvisionStatus -ProgressPath $ProgressPath -Phase 'preflight' -State 'completed' -Message 'Windows is ready.'

    Write-RemoteDockerProvisionStatus -ProgressPath $ProgressPath -Phase 'wsl' -State 'started' -Message 'Preparing managed WSL environment.'
    $distroExists = Test-ManagedDistro
    if (-not $distroExists) {
        if (-not (Test-Path -LiteralPath $distroRoot)) {
            New-Item -ItemType Directory -Path $distroRoot -Force | Out-Null
        }
        Assert-ManagedDirectory -Path $DataRoot
        Assert-ManagedDirectory -Path $distroRoot
        if (-not (Test-Path -LiteralPath $rootfsChecksumPath -PathType Leaf)) {
            throw 'Rootfs checksum manifest was not found.'
        }
        $rootfsSha256 = ((Get-Content -LiteralPath $rootfsChecksumPath -Raw).Trim() -split '\s+')[0]
        if ($rootfsSha256 -notmatch '^[A-Fa-f0-9]{64}$') {
            throw 'Rootfs checksum manifest is invalid.'
        }
        $resolvedRootfs = (Resolve-Path -LiteralPath $rootfsPath -ErrorAction Stop).Path
        if (-not [string]::Equals($resolvedRootfs, $rootfsPath, [System.StringComparison]::OrdinalIgnoreCase)) {
            throw 'Resolved rootfs path does not match the packaged path.'
        }
        $actualHash = (Get-FileHash -LiteralPath $resolvedRootfs -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($actualHash -ne $rootfsSha256.ToLowerInvariant()) {
            throw 'Rootfs SHA-256 verification failed.'
        }
        Invoke-External -FilePath 'wsl.exe' -ArgumentList @(
            '--import', $managedDistroName, $distroRoot, $resolvedRootfs, '--version', '2'
        ) -Description 'Managed WSL import' | Out-Null
    }

    $firstBoot = @(
        'set -eu'
        'install -d -m 0755 /etc/remote-docker'
        'systemctl --root=/ disable docker.service containerd.service ssh.socket ssh.service syncthing@remote-docker.service remote-docker.target >/dev/null 2>&1 || true'
        "printf '%s\n' '$managedMetadata' > /etc/remote-docker/managed.json.tmp"
        'chmod 0644 /etc/remote-docker/managed.json.tmp'
        'mv -f /etc/remote-docker/managed.json.tmp /etc/remote-docker/managed.json'
    ) -join "`n"
    Invoke-External -FilePath 'wsl.exe' -ArgumentList @(
        '--distribution', $managedDistroName, '--user', 'root', '--exec', '/bin/sh', '-c', $firstBoot
    ) -Description 'Managed WSL first boot' | Out-Null
    Write-RemoteDockerProvisionStatus -ProgressPath $ProgressPath -Phase 'wsl' -State 'completed' -Message 'Managed WSL environment is ready.'

    Write-RemoteDockerProvisionStatus -ProgressPath $ProgressPath -Phase 'docker' -State 'started' -Message 'Installing the managed Docker helper.'
    Write-InstallLog -Message 'Managed WSL identity preparation started.'
    $identityPreparation = Start-Process `
        -FilePath $desktopExecutable `
        -ArgumentList @('--prepare-wsl') `
        -Wait `
        -PassThru
    if ($identityPreparation.ExitCode -ne 0) {
        throw "Managed WSL identity preparation failed with exit code $($identityPreparation.ExitCode)."
    }
    Write-InstallLog -Message "Managed WSL identity preparation completed with exit code $($identityPreparation.ExitCode)."
    Invoke-External -FilePath 'wsl.exe' -ArgumentList @(
        '--distribution', $managedDistroName, '--user', 'root', '--exec',
        '/usr/local/bin/remote-docker-remote', 'runtime-status'
    ) -Description 'Managed WSL health check' | Out-Null
    Write-RemoteDockerProvisionStatus -ProgressPath $ProgressPath -Phase 'docker' -State 'completed' -Message 'Docker environment is ready and stopped.'

    Write-RemoteDockerProvisionStatus -ProgressPath $ProgressPath -Phase 'firewall' -State 'started' -Message 'Restricting access to the private local network.'
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
            -Program $desktopExecutable `
            -Protocol TCP `
            -LocalPort $rule.Port `
            -Profile Private `
            -RemoteAddress LocalSubnet | Out-Null
    }
    Write-RemoteDockerProvisionStatus -ProgressPath $ProgressPath -Phase 'firewall' -State 'completed' -Message 'Private network access is configured.'

    Invoke-External -FilePath 'wsl.exe' -ArgumentList @(
        '--distribution', $managedDistroName, '--user', 'root', '--exec', '/usr/bin/systemctl', 'stop', 'remote-docker.target'
    ) -Description 'Managed runtime stop' -IgnoreFailure | Out-Null
    Invoke-External -FilePath 'wsl.exe' -ArgumentList @('--terminate', $managedDistroName) -Description 'Managed WSL shutdown' -IgnoreFailure | Out-Null
    Write-RemoteDockerProvisionStatus -ProgressPath $ProgressPath -Phase 'complete' -State 'completed' -Message 'Remote Docker is installed and stopped.'
    Write-InstallLog -Message 'Provisioning completed successfully.'
    [ordered]@{ state = 'ready'; distro = $managedDistroName; data_root = $DataRoot } | ConvertTo-Json -Compress
}
catch {
    $reason = $_.Exception.Message -replace '[\r\n]+', ' '
    if ($logReady) {
        try {
            Write-InstallLog -Message "Provisioning failed: $reason"
        }
        catch {}
    }
    if ($progressReady) {
        try {
            Write-RemoteDockerProvisionStatus -ProgressPath $ProgressPath -Phase 'complete' -State 'failed' -Message $reason
        }
        catch {}
    }
    [Console]::Error.WriteLine($reason)
    exit 1
}
