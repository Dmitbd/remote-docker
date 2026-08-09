[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$CandidatePath,
    [Parameter(Mandatory = $true)][ValidatePattern('^[A-Fa-f0-9]{64}$')][string]$ExpectedSha256,
    [Parameter(Mandatory = $true)][ValidatePattern('^[0-9A-Za-z][0-9A-Za-z.-]{0,63}$')][string]$Version,
    [Parameter(Mandatory = $true)][string]$ApplicationRoot
)

$ErrorActionPreference = 'Stop'

. (Join-Path $PSScriptRoot 'path-validation.ps1')

function Assert-Administrator {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = [Security.Principal.WindowsPrincipal]::new($identity)
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw 'Updating Remote Docker requires an elevated Administrator session.'
    }
}

function Assert-FreeReleaseBinary {
    param([Parameter(Mandatory = $true)][string]$Path, [Parameter(Mandatory = $true)][string]$Sha256)

    $actualHash = (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actualHash -ne $Sha256.ToLowerInvariant()) {
        throw "SHA-256 verification failed for '$Path'."
    }
    $signature = Get-AuthenticodeSignature -LiteralPath $Path
    if ($signature.Status -ne [System.Management.Automation.SignatureStatus]::NotSigned) {
        throw "The free release update channel accepts only explicitly unsigned Remote Docker binaries."
    }
}

function Assert-NoReparseTree {
    param([Parameter(Mandatory = $true)][string]$Path)

    if (-not (Test-Path -LiteralPath $Path)) { return }
    $pending = [System.Collections.Generic.Queue[string]]::new()
    $pending.Enqueue((Get-Item -LiteralPath $Path -Force).FullName)
    while ($pending.Count -gt 0) {
        $current = $pending.Dequeue()
        $item = Get-Item -LiteralPath $current -Force
        if (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
            throw "Refusing to update through a reparse point at '$current'."
        }
        if ($item.PSIsContainer) {
            foreach ($child in Get-ChildItem -LiteralPath $current -Force) {
                if (($child.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
                    throw "Refusing to update through a reparse point at '$($child.FullName)'."
                }
                if ($child.PSIsContainer) { $pending.Enqueue($child.FullName) }
            }
        }
    }
}

Assert-Administrator
$ApplicationRoot = Assert-RemoteDockerCanonicalPath -Path $ApplicationRoot -Description 'Application root'
$resolvedInstallRoot = (Resolve-Path -LiteralPath $ApplicationRoot -ErrorAction Stop).Path
if (-not [string]::Equals($resolvedInstallRoot, $ApplicationRoot, [System.StringComparison]::OrdinalIgnoreCase)) {
    throw 'Resolved application root does not match the selected install path.'
}
$installRootItem = Get-Item -LiteralPath $ApplicationRoot -Force
if (($installRootItem.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
    throw 'Refusing to update through a reparse point at the application root.'
}

$resolvedCandidate = (Resolve-Path -LiteralPath $CandidatePath -ErrorAction Stop).Path
$activePath = Join-Path $ApplicationRoot 'RemoteDocker.exe'
if (-not (Test-Path -LiteralPath $activePath -PathType Leaf)) {
    throw 'The active RemoteDocker.exe was not found.'
}
Assert-FreeReleaseBinary -Path $resolvedCandidate -Sha256 $ExpectedSha256
if ((Get-AuthenticodeSignature -LiteralPath $activePath).Status -ne [System.Management.Automation.SignatureStatus]::NotSigned) {
    throw 'The installed binary does not belong to the free unsigned release channel.'
}
& $activePath --shutdown

$updatesRoot = Join-Path $ApplicationRoot '.updates'
$stagingRoot = Join-Path $updatesRoot $Version
$stagedPath = Join-Path $stagingRoot 'RemoteDocker.exe'
$backupPath = Join-Path $stagingRoot 'RemoteDocker.exe.previous'
if (-not (Test-Path -LiteralPath $updatesRoot)) {
    New-Item -ItemType Directory -Path $updatesRoot -Force | Out-Null
}
Assert-NoReparseTree -Path $updatesRoot

try {
    if (Test-Path -LiteralPath $stagingRoot) {
        Assert-NoReparseTree -Path $stagingRoot
        Remove-Item -LiteralPath $stagingRoot -Recurse -Force
    }
    New-Item -ItemType Directory -Path $stagingRoot -Force | Out-Null
    Copy-Item -LiteralPath $resolvedCandidate -Destination $stagedPath
    Assert-FreeReleaseBinary -Path $stagedPath -Sha256 $ExpectedSha256
    if (-not [string]::Equals(
        [System.IO.Path]::GetPathRoot($activePath),
        [System.IO.Path]::GetPathRoot($stagedPath),
        [System.StringComparison]::OrdinalIgnoreCase
    )) {
        throw 'The verified staging path must be on the same volume as Remote Docker.'
    }
    [System.IO.File]::Replace($stagedPath, $activePath, $backupPath, $true)
    Write-Output "Updated RemoteDocker.exe to version $Version."
}
finally {
    if (Test-Path -LiteralPath $stagingRoot) {
        Assert-NoReparseTree -Path $stagingRoot
        Remove-Item -LiteralPath $stagingRoot -Recurse -Force
    }
}
