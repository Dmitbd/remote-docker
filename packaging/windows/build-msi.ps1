[CmdletBinding()]
param(
    [ValidatePattern('^[0-9]+\.[0-9]+\.[0-9]+$')]
    [string]$Version = '0.1.0',
    [string]$OutputDirectory = (Join-Path $PSScriptRoot '..\..\dist\windows'),
    [string]$RootfsPath = (Join-Path $PSScriptRoot '..\..\dist\remote-docker-rootfs.tar.zst'),
    [string]$BinaryInputDirectory = '',
    [switch]$BuildBinariesOnly
)

$ErrorActionPreference = 'Stop'
$GoVersion = '1.26.5'
$WixVersion = '6.0.2'
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$wixSource = Join-Path $PSScriptRoot 'RemoteDocker.wxs'
$nugetConfig = Join-Path $PSScriptRoot 'nuget.config'
$resolvedOutput = [System.IO.Path]::GetFullPath($OutputDirectory)
$binaryOutput = Join-Path $resolvedOutput 'bin'
$workRoot = Join-Path ([System.IO.Path]::GetTempPath()) ("remote-docker-windows-package-{0}" -f [guid]::NewGuid().ToString('N'))

function Invoke-Checked {
    param(
        [Parameter(Mandatory = $true)][string]$FilePath,
        [Parameter(Mandatory = $true)][string[]]$ArgumentList,
        [Parameter(Mandatory = $true)][string]$Description
    )

    & $FilePath @ArgumentList
    if ($LASTEXITCODE -ne 0) {
        throw "$Description failed with exit code $LASTEXITCODE."
    }
}

function Resolve-RequiredFile {
    param([Parameter(Mandatory = $true)][string]$Path)

    $resolved = (Resolve-Path -LiteralPath $Path -ErrorAction Stop).Path
    if (-not (Test-Path -LiteralPath $resolved -PathType Leaf)) {
        throw "Required package input is not a file: '$Path'."
    }
    $resolved
}

try {
    New-Item -ItemType Directory -Path $resolvedOutput -Force | Out-Null
    New-Item -ItemType Directory -Path $workRoot -Force | Out-Null

    if ([string]::IsNullOrWhiteSpace($BinaryInputDirectory)) {
        if ($null -eq (Get-Command 'go' -ErrorAction SilentlyContinue)) {
            throw 'Go is required to build the Windows package binaries.'
        }
        $goVersionOutput = (& go version | Out-String).Trim()
        if ($goVersionOutput -notmatch "^go version go$([regex]::Escape($GoVersion)) ") {
            throw "Go $GoVersion is required; found '$goVersionOutput'."
        }

        New-Item -ItemType Directory -Path $binaryOutput -Force | Out-Null
        $previousGoos = $env:GOOS
        $previousGoarch = $env:GOARCH
        $previousCgo = $env:CGO_ENABLED
        try {
            $env:GOOS = 'windows'
            $env:GOARCH = 'amd64'
            $env:CGO_ENABLED = '0'
            Invoke-Checked -FilePath 'go' -ArgumentList @(
                '-C', $repoRoot, 'build', '-trimpath', '-buildvcs=false', '-ldflags=-s -w -buildid=',
                '-o', (Join-Path $binaryOutput 'RemoteDockerAgent.exe'),
                './cmd/remote-docker-agent'
            ) -Description 'Windows Agent build'
            Invoke-Checked -FilePath 'go' -ArgumentList @(
                '-C', $repoRoot, 'build', '-trimpath', '-buildvcs=false', '-ldflags=-s -w -buildid=',
                '-o', (Join-Path $binaryOutput 'RemoteDockerTray.exe'),
                './cmd/remote-docker-tray'
            ) -Description 'Windows Tray build'
        }
        finally {
            $env:GOOS = $previousGoos
            $env:GOARCH = $previousGoarch
            $env:CGO_ENABLED = $previousCgo
        }
        $resolvedBinaryInput = $binaryOutput
    }
    else {
        $resolvedBinaryInput = (Resolve-Path -LiteralPath $BinaryInputDirectory -ErrorAction Stop).Path
    }

    $agentSource = Resolve-RequiredFile (Join-Path $resolvedBinaryInput 'RemoteDockerAgent.exe')
    $traySource = Resolve-RequiredFile (Join-Path $resolvedBinaryInput 'RemoteDockerTray.exe')
    if ($BuildBinariesOnly) {
        Write-Output "Created unsigned Windows binaries in '$resolvedBinaryInput'."
        exit 0
    }

    if ($null -eq (Get-Command 'dotnet' -ErrorAction SilentlyContinue)) {
        throw '.NET SDK 6 or later is required to run the pinned WiX tool.'
    }
    $resolvedRootfs = Resolve-RequiredFile $RootfsPath
    $rootfsChecksum = Resolve-RequiredFile "$RootfsPath.sha256"
    $manifestHash = ((Get-Content -LiteralPath $rootfsChecksum -Raw).Trim() -split '\s+')[0]
    if ($manifestHash -notmatch '^[A-Fa-f0-9]{64}$') {
        throw "Rootfs checksum manifest is invalid: '$rootfsChecksum'."
    }
    $actualRootfsHash = (Get-FileHash -LiteralPath $resolvedRootfs -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actualRootfsHash -ne $manifestHash.ToLowerInvariant()) {
        throw 'Rootfs SHA-256 verification failed before MSI packaging.'
    }

    $toolRoot = Join-Path $workRoot 'tools'
    New-Item -ItemType Directory -Path $toolRoot -Force | Out-Null
    Invoke-Checked -FilePath 'dotnet' -ArgumentList @(
        'tool', 'install', '--tool-path', $toolRoot, 'wix', '--version', $WixVersion,
        '--configfile', $nugetConfig
    ) -Description 'Pinned WiX installation'
    $wix = Join-Path $toolRoot 'wix.exe'
    $actualWixVersion = (& $wix --version | Out-String).Trim()
    if ($actualWixVersion -notmatch "^$([regex]::Escape($WixVersion))(?:\+|$)") {
        throw "WiX $WixVersion is required; found '$actualWixVersion'."
    }

    $msiPath = Join-Path $resolvedOutput "Remote-Docker-Agent-$Version-x64.msi"
    Invoke-Checked -FilePath $wix -ArgumentList @(
        'build', $wixSource, '-arch', 'x64',
        '-d', "ProductVersion=$Version",
        '-d', "AgentSource=$agentSource",
        '-d', "TraySource=$traySource",
        '-d', "RootfsSource=$resolvedRootfs",
        '-d', "RootfsChecksumSource=$rootfsChecksum",
        '-d', "ProbeScriptSource=$(Join-Path $PSScriptRoot 'scripts\probe.ps1')",
        '-d', "ProvisionScriptSource=$(Join-Path $PSScriptRoot 'scripts\provision.ps1')",
        '-d', "UninstallScriptSource=$(Join-Path $PSScriptRoot 'scripts\uninstall.ps1')",
        '-d', "UpdateScriptSource=$(Join-Path $PSScriptRoot 'install-agent.ps1')",
        '-out', $msiPath
    ) -Description 'MSI build'

    Write-Output "Created unsigned development package '$msiPath'."
}
finally {
    if (Test-Path -LiteralPath $workRoot) {
        Remove-Item -LiteralPath $workRoot -Recurse -Force
    }
}
