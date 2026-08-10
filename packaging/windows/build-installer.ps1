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
$ProgressPreference = 'SilentlyContinue'
$GoVersion = '1.26.5'
$NsisVersion = '3.12'
$NsisPackageSha256 = '4a1bbf9987e5b9b6bda4c2433af62bb79f2d9d3bd67b392f29a069ecda8c5f64'
$NsisSha256 = '3bc2b06253a7e4957111be152ac6a536e0c7478a706e19da814038db5d706495'
$LlvmMingwVersion = '20260616'
$LlvmMingwSha256 = 'b9b68a4d276e16fa25802aaba458e4638f64b3884c290aaccdc2d87083b6ca35'
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$installerSource = Join-Path $PSScriptRoot 'installer\RemoteDocker.nsi'
$resolvedOutput = [System.IO.Path]::GetFullPath($OutputDirectory)
$workRoot = Join-Path ([System.IO.Path]::GetTempPath()) ("remote-docker-windows-setup-{0}" -f [guid]::NewGuid().ToString('N'))
$resourceObject = Join-Path $repoRoot 'cmd\remote-docker-desktop\remote-docker_windows_amd64.syso'

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

function Get-VerifiedDownload {
    param(
        [Parameter(Mandatory = $true)][uri]$Uri,
        [Parameter(Mandatory = $true)][string]$Destination,
        [Parameter(Mandatory = $true)][ValidatePattern('^[a-f0-9]{64}$')][string]$Sha256
    )

    $lastFailure = ''
    for ($attempt = 1; $attempt -le 3; $attempt++) {
        if (Test-Path -LiteralPath $Destination) {
            Remove-Item -LiteralPath $Destination -Force
        }
        try {
            Invoke-WebRequest -Uri $Uri -OutFile $Destination -MaximumRedirection 10
            $actual = (Get-FileHash -LiteralPath $Destination -Algorithm SHA256).Hash.ToLowerInvariant()
            if ($actual -eq $Sha256) {
                return
            }
            $lastFailure = "SHA-256 mismatch: expected '$Sha256', received '$actual'."
        }
        catch {
            $lastFailure = $_.Exception.Message
        }
        if ($attempt -lt 3) {
            Start-Sleep -Seconds (2 * $attempt)
        }
    }
    throw "Verified download failed after 3 attempts for '$Uri': $lastFailure"
}

function Assert-ManifestHash {
    param(
        [Parameter(Mandatory = $true)][string]$FilePath,
        [Parameter(Mandatory = $true)][string]$ManifestPath,
        [Parameter(Mandatory = $true)][string]$Description
    )

    $manifestHash = ((Get-Content -LiteralPath $ManifestPath -Raw).Trim() -split '\s+')[0]
    if ($manifestHash -notmatch '^[A-Fa-f0-9]{64}$') {
        throw "$Description checksum manifest is invalid."
    }
    $actualHash = (Get-FileHash -LiteralPath $FilePath -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actualHash -ne $manifestHash.ToLowerInvariant()) {
        throw "$Description SHA-256 verification failed."
    }
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

        $llvmArchive = Join-Path $workRoot "llvm-mingw-$LlvmMingwVersion-ucrt-x86_64.zip"
        Get-VerifiedDownload `
            -Uri "https://github.com/mstorsjo/llvm-mingw/releases/download/$LlvmMingwVersion/llvm-mingw-$LlvmMingwVersion-ucrt-x86_64.zip" `
            -Destination $llvmArchive `
            -Sha256 $LlvmMingwSha256
        $llvmRoot = Join-Path $workRoot 'llvm'
        Expand-Archive -LiteralPath $llvmArchive -DestinationPath $llvmRoot
        $compiler = (Get-ChildItem -LiteralPath $llvmRoot -Recurse -Filter 'x86_64-w64-mingw32-gcc.exe' -File | Select-Object -First 1).FullName
        $resourceCompiler = (Get-ChildItem -LiteralPath $llvmRoot -Recurse -Filter 'x86_64-w64-mingw32-windres.exe' -File | Select-Object -First 1).FullName
        if ([string]::IsNullOrWhiteSpace($compiler) -or [string]::IsNullOrWhiteSpace($resourceCompiler)) {
            throw 'Pinned LLVM-MinGW archive does not contain the required x64 compiler tools.'
        }

        $binaryOutput = if ($BuildBinariesOnly) { Join-Path $resolvedOutput 'bin' } else { Join-Path $workRoot 'bin' }
        New-Item -ItemType Directory -Path $binaryOutput -Force | Out-Null
        $iconSource = Resolve-RequiredFile (Join-Path $repoRoot 'assets\icon\app\remote-docker.ico')
        $resourceScript = Join-Path $workRoot 'remote-docker.rc'
        [System.IO.File]::WriteAllText($resourceScript, "1 ICON `"$iconSource`"`r`n", [System.Text.Encoding]::ASCII)
        Invoke-Checked -FilePath $resourceCompiler -ArgumentList @(
            '--input-format=rc', '--output-format=coff', '--input', $resourceScript, '--output', $resourceObject
        ) -Description 'Windows icon resource build'

        $previousGoos = $env:GOOS
        $previousGoarch = $env:GOARCH
        $previousCgo = $env:CGO_ENABLED
        $previousCc = $env:CC
        try {
            $env:GOOS = 'windows'
            $env:GOARCH = 'amd64'
            $env:CGO_ENABLED = '1'
            $env:CC = $compiler
            Invoke-Checked -FilePath 'go' -ArgumentList @(
                '-C', $repoRoot, 'build', '-trimpath', '-buildvcs=false', '-ldflags=-H=windowsgui -s -w -buildid=',
                '-o', (Join-Path $binaryOutput 'RemoteDocker.exe'),
                './cmd/remote-docker-desktop'
            ) -Description 'Remote Docker desktop build'

            $env:GOOS = 'linux'
            $env:GOARCH = 'amd64'
            $env:CGO_ENABLED = '0'
            $env:CC = $null
            $runtimeOutput = Join-Path $binaryOutput 'remote-docker-remote-linux-amd64'
            Invoke-Checked -FilePath 'go' -ArgumentList @(
                '-C', $repoRoot, 'build', '-trimpath', '-buildvcs=false', '-ldflags=-s -w -buildid=',
                '-o', $runtimeOutput,
                './cmd/remote-docker-remote'
            ) -Description 'Managed WSL runtime build'
            $runtimeSha256 = (Get-FileHash -LiteralPath $runtimeOutput -Algorithm SHA256).Hash.ToLowerInvariant()
            [System.IO.File]::WriteAllText(
                "$runtimeOutput.sha256",
                "$runtimeSha256  remote-docker-remote-linux-amd64`n",
                [System.Text.Encoding]::ASCII
            )
        }
        finally {
            $env:GOOS = $previousGoos
            $env:GOARCH = $previousGoarch
            $env:CGO_ENABLED = $previousCgo
            $env:CC = $previousCc
        }
        $resolvedBinaryInput = $binaryOutput
    }
    else {
        $resolvedBinaryInput = (Resolve-Path -LiteralPath $BinaryInputDirectory -ErrorAction Stop).Path
    }

    $desktopSource = Resolve-RequiredFile (Join-Path $resolvedBinaryInput 'RemoteDocker.exe')
    $runtimeSource = Resolve-RequiredFile (Join-Path $resolvedBinaryInput 'remote-docker-remote-linux-amd64')
    $runtimeChecksumSource = Resolve-RequiredFile (Join-Path $resolvedBinaryInput 'remote-docker-remote-linux-amd64.sha256')
    Assert-ManifestHash -FilePath $runtimeSource -ManifestPath $runtimeChecksumSource -Description 'Managed WSL runtime'
    if ($BuildBinariesOnly) {
        Write-Output "Created unsigned Windows binaries in '$resolvedBinaryInput'."
        exit 0
    }

    $resolvedRootfs = Resolve-RequiredFile $RootfsPath
    $rootfsChecksum = Resolve-RequiredFile "$RootfsPath.sha256"
    Assert-ManifestHash -FilePath $resolvedRootfs -ManifestPath $rootfsChecksum -Description 'Rootfs'

    $nsisPackage = Join-Path $workRoot "nsis.install-$NsisVersion.0.nupkg"
    Get-VerifiedDownload `
        -Uri "https://community.chocolatey.org/api/v2/package/nsis.install/$NsisVersion.0" `
        -Destination $nsisPackage `
        -Sha256 $NsisPackageSha256
    $nsisArchive = "$nsisPackage.zip"
    Copy-Item -LiteralPath $nsisPackage -Destination $nsisArchive
    $nsisPackageRoot = Join-Path $workRoot 'nsis-package'
    Expand-Archive -LiteralPath $nsisArchive -DestinationPath $nsisPackageRoot
    $nsisSetup = Resolve-RequiredFile (Join-Path $nsisPackageRoot "tools\nsis-$NsisVersion-setup.exe")
    $nsisSetupHash = (Get-FileHash -LiteralPath $nsisSetup -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($nsisSetupHash -ne $NsisSha256) {
        throw "Packaged NSIS SHA-256 mismatch: expected '$NsisSha256', received '$nsisSetupHash'."
    }
    $nsisRoot = Join-Path $workRoot 'nsis'
    $nsisProcess = Start-Process -FilePath $nsisSetup -ArgumentList @('/S', "/D=$nsisRoot") -Wait -PassThru -WindowStyle Hidden
    if ($nsisProcess.ExitCode -ne 0) {
        throw "Pinned NSIS installation failed with exit code $($nsisProcess.ExitCode)."
    }
    $makensis = Resolve-RequiredFile (Join-Path $nsisRoot 'makensis.exe')
    $nsisVersionOutput = (& $makensis /VERSION | Out-String).Trim()
    if ($nsisVersionOutput -ne "v$NsisVersion") {
        throw "NSIS $NsisVersion is required; found '$nsisVersionOutput'."
    }

    $setupPath = Join-Path $resolvedOutput "Remote-Docker-$Version-x64-Setup.exe"
    $defines = @(
        '/WX',
        '/INPUTCHARSET',
        'UTF8',
        "/DPRODUCT_VERSION=$Version",
        "/DOUTPUT_FILE=$setupPath",
        "/DAPP_SOURCE=$desktopSource",
        "/DICON_SOURCE=$(Resolve-RequiredFile (Join-Path $repoRoot 'assets\icon\app\remote-docker.ico'))",
        "/DROOTFS_SOURCE=$resolvedRootfs",
        "/DROOTFS_CHECKSUM_SOURCE=$rootfsChecksum",
        "/DRUNTIME_SOURCE=$runtimeSource",
        "/DRUNTIME_CHECKSUM_SOURCE=$runtimeChecksumSource",
        "/DPROBE_SOURCE=$(Resolve-RequiredFile (Join-Path $PSScriptRoot 'scripts\probe.ps1'))",
        "/DPROVISION_SOURCE=$(Resolve-RequiredFile (Join-Path $PSScriptRoot 'scripts\provision.ps1'))",
        "/DSTATUS_SOURCE=$(Resolve-RequiredFile (Join-Path $PSScriptRoot 'scripts\provision-status.ps1'))",
        "/DPATH_VALIDATION_SOURCE=$(Resolve-RequiredFile (Join-Path $PSScriptRoot 'scripts\path-validation.ps1'))",
        "/DUNINSTALL_SOURCE=$(Resolve-RequiredFile (Join-Path $PSScriptRoot 'scripts\uninstall.ps1'))",
        "/DUPDATE_SOURCE=$(Resolve-RequiredFile (Join-Path $PSScriptRoot 'install-agent.ps1'))",
        $installerSource
    )
    Push-Location (Split-Path -Parent $installerSource)
    try {
        Invoke-Checked -FilePath $makensis -ArgumentList $defines -Description 'NSIS Setup EXE build'
    }
    finally {
        Pop-Location
    }
    Resolve-RequiredFile $setupPath | Out-Null
    Write-Output "Created unsigned Windows Setup EXE '$setupPath'."
}
finally {
    if (Test-Path -LiteralPath $resourceObject -PathType Leaf) {
        Remove-Item -LiteralPath $resourceObject -Force
    }
    if (Test-Path -LiteralPath $workRoot) {
        Remove-Item -LiteralPath $workRoot -Recurse -Force
    }
}
