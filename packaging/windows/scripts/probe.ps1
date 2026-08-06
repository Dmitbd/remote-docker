$ErrorActionPreference = 'SilentlyContinue'

$managedDistroName = 'remote-docker'
$managedMarker = 'remote-docker-managed-v1'

$windowsBuild = [Environment]::OSVersion.Version.Build

$virtualizationEnabled = $false
$processor = Get-CimInstance -ClassName Win32_Processor | Select-Object -First 1
if ($null -ne $processor) {
    $virtualizationEnabled = [bool]$processor.VirtualizationFirmwareEnabled
}

$wslInstalled = $null -ne (Get-Command 'wsl.exe' -ErrorAction SilentlyContinue)
$wsl2Ready = $false
$distroExists = $false
$markerMatches = $false

if ($wslInstalled) {
    $statusOutput = & wsl.exe --status 2>&1
    $statusExitCode = $LASTEXITCODE
    $versionOutput = & wsl.exe --version 2>&1
    $versionExitCode = $LASTEXITCODE
    $wsl2Ready = ($statusExitCode -eq 0) -and ($versionExitCode -eq 0)

    $distroOutput = & wsl.exe --list --quiet 2>&1
    if ($LASTEXITCODE -eq 0) {
        $distroNames = (($distroOutput | Out-String) -replace "`0", '') -split "`r?`n"
        $distroExists = $null -ne ($distroNames | Where-Object { $_.Trim() -eq $managedDistroName } | Select-Object -First 1)
    }

    if ($distroExists) {
        $markerOutput = & wsl.exe --distribution $managedDistroName --exec cat /etc/remote-docker-release 2>$null
        if ($LASTEXITCODE -eq 0) {
            $markerMatches = (($markerOutput | Out-String).Trim() -eq $managedMarker)
        }
    }
}

$freeBytes = [uint64]0
$systemDrive = $env:SystemDrive
if ([string]::IsNullOrWhiteSpace($systemDrive)) {
    $systemDrive = 'C:'
}
$disk = Get-CimInstance -ClassName Win32_LogicalDisk -Filter "DeviceID='$systemDrive'"
if ($null -ne $disk -and $null -ne $disk.FreeSpace) {
    $freeBytes = [uint64]$disk.FreeSpace
}

$firewallCapability = $null -ne (Get-Command 'New-NetFirewallRule' -ErrorAction SilentlyContinue)

$result = [ordered]@{
    windows_build = [int]$windowsBuild
    virtualization_enabled = [bool]$virtualizationEnabled
    wsl_installed = [bool]$wslInstalled
    wsl2_ready = [bool]$wsl2Ready
    distro = [ordered]@{
        exists = [bool]$distroExists
        marker_matches = [bool]$markerMatches
    }
    free_bytes = [uint64]$freeBytes
    firewall_capability = [bool]$firewallCapability
}

$result | ConvertTo-Json -Depth 3 -Compress
exit 0
