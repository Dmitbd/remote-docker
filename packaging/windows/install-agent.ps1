[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [string]$CandidatePath,
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[A-Fa-f0-9]{64}$')]
    [string]$ExpectedSha256,
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[A-Fa-f0-9]{40}$')]
    [string]$ExpectedSignerThumbprint,
    [Parameter(Mandatory = $true)]
    [ValidatePattern('^[0-9A-Za-z][0-9A-Za-z.-]{0,63}$')]
    [string]$Version,
    [Parameter(Mandatory = $true)]
    [ValidateSet('RemoteDockerAgent.exe', 'RemoteDockerTray.exe')]
    [string]$TargetName
)

$ErrorActionPreference = 'Stop'

function Assert-Administrator {
    $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
    $principal = [Security.Principal.WindowsPrincipal]::new($identity)
    if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw 'Updating a per-machine Remote Docker binary requires an elevated Administrator session.'
    }
}

function Assert-VerifiedBinary {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Sha256,
        [Parameter(Mandatory = $true)][string]$SignerThumbprint
    )

    $actualHash = (Get-FileHash -LiteralPath $Path -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actualHash -ne $Sha256.ToLowerInvariant()) {
        throw "SHA-256 verification failed for '$Path'."
    }

    $signature = Get-AuthenticodeSignature -LiteralPath $Path
    if ($signature.Status -ne [System.Management.Automation.SignatureStatus]::Valid) {
        throw "Authenticode verification failed for '$Path'."
    }
    if ($null -eq $signature.SignerCertificate) {
        throw "Authenticode signer information is missing for '$Path'."
    }
    $actualThumbprint = $signature.SignerCertificate.Thumbprint.Replace(' ', '').ToUpperInvariant()
    if ($actualThumbprint -ne $SignerThumbprint.Replace(' ', '').ToUpperInvariant()) {
        throw "Authenticode signer verification failed for '$Path'."
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
            throw "Refusing to update through a reparse point at '$current'."
        }
        if ($item.PSIsContainer) {
            foreach ($child in Get-ChildItem -LiteralPath $current -Force) {
                if (($child.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
                    throw "Refusing to update through a reparse point at '$($child.FullName)'."
                }
                if ($child.PSIsContainer) {
                    $pending.Enqueue($child.FullName)
                }
            }
        }
    }
}

Assert-Administrator

$resolvedCandidate = (Resolve-Path -LiteralPath $CandidatePath -ErrorAction Stop).Path
if (-not (Test-Path -LiteralPath $resolvedCandidate -PathType Leaf)) {
    throw "Update candidate is not a file: '$CandidatePath'."
}

$programFiles = [Environment]::GetFolderPath([Environment+SpecialFolder]::ProgramFiles)
$installRoot = [System.IO.Path]::GetFullPath((Join-Path $programFiles 'Remote Docker'))
$resolvedInstallRoot = (Resolve-Path -LiteralPath $installRoot -ErrorAction Stop).Path
if (-not [string]::Equals($resolvedInstallRoot, $installRoot, [System.StringComparison]::OrdinalIgnoreCase)) {
    throw "Resolved install path does not match the packaged path '$installRoot'."
}
$installRootItem = Get-Item -LiteralPath $resolvedInstallRoot -Force
if (($installRootItem.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
    throw "Refusing to update through a reparse point at '$installRoot'."
}
$activePath = Join-Path $resolvedInstallRoot $TargetName
if (-not (Test-Path -LiteralPath $activePath -PathType Leaf)) {
    throw "The active packaged binary was not found at '$activePath'."
}
$activeItem = Get-Item -LiteralPath $activePath -Force
if (($activeItem.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
    throw "Refusing to replace a reparse point at '$activePath'."
}
$activeSignature = Get-AuthenticodeSignature -LiteralPath $activePath
if ($activeSignature.Status -ne [System.Management.Automation.SignatureStatus]::Valid -or
    $null -eq $activeSignature.SignerCertificate) {
    throw "The active packaged binary at '$activePath' does not have a valid Authenticode signer."
}
$activeThumbprint = $activeSignature.SignerCertificate.Thumbprint.Replace(' ', '').ToUpperInvariant()
if ($activeThumbprint -ne $ExpectedSignerThumbprint.Replace(' ', '').ToUpperInvariant()) {
    throw 'The expected update signer does not match the active packaged binary signer.'
}

Assert-VerifiedBinary `
    -Path $resolvedCandidate `
    -Sha256 $ExpectedSha256 `
    -SignerThumbprint $ExpectedSignerThumbprint

$updatesRoot = Join-Path $installRoot '.updates'
$stagingRoot = Join-Path $updatesRoot $Version
$stagedPath = Join-Path $stagingRoot $TargetName
$backupPath = Join-Path $stagingRoot "$TargetName.previous"

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
    Assert-NoReparseTree -Path $stagingRoot
    Copy-Item -LiteralPath $resolvedCandidate -Destination $stagedPath

    Assert-VerifiedBinary `
        -Path $stagedPath `
        -Sha256 $ExpectedSha256 `
        -SignerThumbprint $ExpectedSignerThumbprint

    $activeVolume = [System.IO.Path]::GetPathRoot($activePath)
    $stagingVolume = [System.IO.Path]::GetPathRoot($stagedPath)
    if (-not [string]::Equals($activeVolume, $stagingVolume, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw 'The verified staging path must be on the same volume as the active binary.'
    }

    [System.IO.File]::Replace($stagedPath, $activePath, $backupPath, $true)
    Write-Output "Updated $TargetName to version $Version."
}
finally {
    if (Test-Path -LiteralPath $stagingRoot) {
        Assert-NoReparseTree -Path $stagingRoot
        Remove-Item -LiteralPath $stagingRoot -Recurse -Force
    }
}
