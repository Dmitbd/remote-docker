[CmdletBinding(SupportsShouldProcess = $true, ConfirmImpact = 'High')]
param(
    [Parameter(Mandatory = $true)][string]$ApplicationRoot,
    [Parameter(Mandatory = $true)][string]$DataRoot,
    [switch]$DeleteData,
    [ValidateSet('DELETE-REMOTE-DOCKER-DATA')][string]$DataRemovalConfirmation
)

$ErrorActionPreference = 'Stop'
$managedDistroName = 'remote-docker'
$managedRelease = 'remote-docker-managed-v1'
$firewallRuleGroup = 'Remote Docker Managed Rules'
$dataMarkerValue = 'remote-docker-managed-data-v1'

function Assert-Administrator {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = [Security.Principal.WindowsPrincipal]::new($identity)
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw 'Per-machine Remote Docker cleanup requires an elevated Administrator session.'
    }
}

function Assert-CanonicalRoot {
    param([Parameter(Mandatory = $true)][string]$Path, [Parameter(Mandatory = $true)][string]$Description)

    if (-not [System.IO.Path]::IsPathFullyQualified($Path)) {
        throw "$Description must be an absolute path."
    }
    $canonical = [System.IO.Path]::GetFullPath($Path)
    if (-not [string]::Equals($canonical.TrimEnd('\'), $Path.TrimEnd('\'), [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "$Description must already be canonical."
    }
    $canonical
}

function Assert-NoReparseTree {
    param([Parameter(Mandatory = $true)][string]$Path)

    if (-not (Test-Path -LiteralPath $Path)) {
        return
    }
    $pending = [System.Collections.Generic.Queue[string]]::new()
    $pending.Enqueue((Get-Item -LiteralPath $Path -Force).FullName)
    while ($pending.Count -gt 0) {
        $current = $pending.Dequeue()
        $item = Get-Item -LiteralPath $current -Force
        if (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
            throw "Refusing to remove a tree containing the reparse point '$current'."
        }
        if ($item.PSIsContainer) {
            foreach ($child in Get-ChildItem -LiteralPath $current -Force) {
                if (($child.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
                    throw "Refusing to remove a tree containing the reparse point '$($child.FullName)'."
                }
                if ($child.PSIsContainer) {
                    $pending.Enqueue($child.FullName)
                }
            }
        }
    }
}

function Invoke-Wsl {
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

Assert-Administrator
$ApplicationRoot = Assert-CanonicalRoot -Path $ApplicationRoot -Description 'Application root'
$DataRoot = Assert-CanonicalRoot -Path $DataRoot -Description 'Data root'
$desktopExecutable = Join-Path $ApplicationRoot 'RemoteDocker.exe'
$distroRoot = Join-Path $DataRoot 'wsl'
$dataMarker = Join-Path $DataRoot '.remote-docker-managed-data'

if (Test-Path -LiteralPath $desktopExecutable -PathType Leaf) {
    & $desktopExecutable --shutdown
}

foreach ($ruleName in @('RemoteDocker.Managed.SSH', 'RemoteDocker.Managed.Syncthing')) {
    $existingRule = Get-NetFirewallRule -Name $ruleName -ErrorAction SilentlyContinue
    if ($null -eq $existingRule) {
        continue
    }
    if ($existingRule.Group -ne $firewallRuleGroup) {
        throw "Refusing to remove the foreign firewall rule '$ruleName'."
    }
    Remove-NetFirewallRule -InputObject $existingRule
}

Remove-Item -LiteralPath (Join-Path $DataRoot 'installer-reboot.pending') -Force -ErrorAction SilentlyContinue
if (-not $DeleteData) {
    Write-Output "Remote Docker application removed. WSL distribution '$managedDistroName' and managed data were preserved."
    exit 0
}
if ($DataRemovalConfirmation -ne 'DELETE-REMOTE-DOCKER-DATA') {
    throw "Data removal requires -DataRemovalConfirmation 'DELETE-REMOTE-DOCKER-DATA'."
}
if (-not (Test-Path -LiteralPath $dataMarker -PathType Leaf) -or (Get-Content -LiteralPath $dataMarker -Raw).Trim() -ne $dataMarkerValue) {
    throw 'Managed data ownership marker is missing or invalid.'
}

$distroList = Invoke-Wsl -ArgumentList @('--list', '--quiet')
if ($distroList.ExitCode -ne 0) {
    throw 'Failed to list WSL distributions before data removal.'
}
$distroOutput = $distroList.Output -replace "`0", ''
$distroExists = $null -ne (($distroOutput -split "`r?`n") | Where-Object { $_.Trim() -eq $managedDistroName } | Select-Object -First 1)
if ($distroExists) {
    $releaseProbe = Invoke-Wsl -ArgumentList @('--distribution', $managedDistroName, '--user', 'root', '--exec', 'cat', '/etc/remote-docker-release')
    if ($releaseProbe.ExitCode -ne 0 -or $releaseProbe.Output.Trim() -ne $managedRelease) {
        throw "WSL distribution '$managedDistroName' does not contain the managed release marker."
    }
}
if (Test-Path -LiteralPath $distroRoot) {
    Assert-NoReparseTree -Path $distroRoot
}

if ($PSCmdlet.ShouldProcess("WSL distribution '$managedDistroName' and '$distroRoot'", 'Permanently delete managed Docker data')) {
    if ($distroExists) {
        $unregister = Invoke-Wsl -ArgumentList @('--unregister', $managedDistroName)
        if ($unregister.ExitCode -ne 0) {
            throw "Failed to unregister WSL distribution '$managedDistroName'."
        }
    }
    if (Test-Path -LiteralPath $distroRoot) {
        Assert-NoReparseTree -Path $distroRoot
        Remove-Item -LiteralPath $distroRoot -Recurse -Force
    }
    if (Test-Path -LiteralPath $desktopExecutable -PathType Leaf) {
        & $desktopExecutable --delete-wsl-credential
        if ($LASTEXITCODE -ne 0) {
            throw 'Managed WSL credential cleanup failed.'
        }
    }
    foreach ($ownedFile in @(
        'installer-progress.jsonl', 'installer.log', 'installer-reboot.pending', '.remote-docker-managed-data'
    )) {
        Remove-Item -LiteralPath (Join-Path $DataRoot $ownedFile) -Force -ErrorAction SilentlyContinue
    }
    Remove-Item -LiteralPath $DataRoot -Force -ErrorAction SilentlyContinue
}
