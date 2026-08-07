[CmdletBinding(SupportsShouldProcess = $true, ConfirmImpact = 'High')]
param(
    [switch]$DeleteData,
    [ValidateSet('DELETE-REMOTE-DOCKER-DATA')]
    [string]$DataRemovalConfirmation
)

$ErrorActionPreference = 'Stop'
$managedDistroName = 'remote-docker'
$managedRelease = 'remote-docker-managed-v1'
$firewallRuleGroup = 'Remote Docker Managed Rules'
$programData = [Environment]::GetFolderPath([Environment+SpecialFolder]::CommonApplicationData)
$programFiles = [Environment]::GetFolderPath([Environment+SpecialFolder]::ProgramFiles)
$installRoot = [System.IO.Path]::GetFullPath((Join-Path $programData 'RemoteDocker'))
$agentExecutable = [System.IO.Path]::GetFullPath((Join-Path $programFiles 'Remote Docker\RemoteDockerAgent.exe'))

function Assert-Administrator {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = [Security.Principal.WindowsPrincipal]::new($identity)
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw 'Per-machine Remote Docker cleanup requires an elevated Administrator session.'
    }
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

Assert-Administrator

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

if (-not $DeleteData) {
    Write-Output "Windows Agent removed. WSL distribution '$managedDistroName' and data in '$installRoot' were preserved."
    exit 0
}

if ($DataRemovalConfirmation -ne 'DELETE-REMOTE-DOCKER-DATA') {
    throw "Data removal requires -DataRemovalConfirmation 'DELETE-REMOTE-DOCKER-DATA'."
}

$distroOutput = (& wsl.exe --list --quiet 2>&1 | Out-String) -replace "`0", ''
if ($LASTEXITCODE -ne 0) {
    throw 'Failed to list WSL distributions before data removal.'
}
$distroExists = $null -ne (($distroOutput -split "`r?`n") | Where-Object { $_.Trim() -eq $managedDistroName } | Select-Object -First 1)
if (-not $distroExists) {
    throw "Managed WSL distribution '$managedDistroName' was not found."
}
$release = (& wsl.exe --distribution $managedDistroName --exec cat /etc/remote-docker-release 2>$null | Out-String).Trim()
if ($LASTEXITCODE -ne 0 -or $release -ne $managedRelease) {
    throw "WSL distribution '$managedDistroName' does not contain the managed release marker."
}

if (Test-Path -LiteralPath $installRoot) {
    $rootItem = Get-Item -LiteralPath $installRoot -Force
    if (($rootItem.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "Refusing to remove a reparse point at '$installRoot'."
    }
    $resolvedInstallRoot = (Resolve-Path -LiteralPath $installRoot -ErrorAction Stop).Path
    if (-not [string]::Equals($resolvedInstallRoot, $installRoot, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "Resolved managed data path does not match '$installRoot'."
    }
    Assert-NoReparseTree -Path $installRoot
}

Write-Host "WSL distribution to delete: $managedDistroName"
if ($PSCmdlet.ShouldProcess("WSL distribution '$managedDistroName' and '$installRoot'", 'Permanently delete managed Docker data')) {
    & wsl.exe --unregister $managedDistroName
    if ($LASTEXITCODE -ne 0) {
        throw "Failed to unregister WSL distribution '$managedDistroName' with exit code $LASTEXITCODE."
    }
    if (Test-Path -LiteralPath $installRoot) {
        Remove-Item -LiteralPath $installRoot -Recurse -Force
    }
    if (Test-Path -LiteralPath $agentExecutable -PathType Leaf) {
        & $agentExecutable --delete-wsl-credential
        if ($LASTEXITCODE -ne 0) {
            throw "Managed WSL credential cleanup failed with exit code $LASTEXITCODE."
        }
    }
}
