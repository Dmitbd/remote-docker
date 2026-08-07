BeforeAll {
    $ErrorActionPreference = 'Stop'

    $repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
    $windowsPackaging = Join-Path $repoRoot 'packaging\windows'
    $wixSource = Join-Path $windowsPackaging 'RemoteDocker.wxs'
    $buildScript = Join-Path $windowsPackaging 'build-msi.ps1'
    $updateScript = Join-Path $windowsPackaging 'install-agent.ps1'
    $nugetConfig = Join-Path $windowsPackaging 'nuget.config'
    $provisionScript = Join-Path $windowsPackaging 'scripts\provision.ps1'
    $uninstallScript = Join-Path $windowsPackaging 'scripts\uninstall.ps1'
    $ciWorkflow = Join-Path $repoRoot '.github\workflows\ci.yml'
    $releaseWorkflow = Join-Path $repoRoot '.github\workflows\release.yml'
    $testScript = Join-Path $PSScriptRoot 'windows_package.Tests.ps1'

    function Read-RepositoryFile {
        param([Parameter(Mandatory = $true)][string]$Path)

        (Get-Content -LiteralPath $Path -Raw).Replace("`r`n", "`n")
    }
}

Describe 'Windows package contract' {
    It 'contains every public package contract file' {
        @(
            $wixSource,
            $buildScript,
            $updateScript,
            $nugetConfig,
            $provisionScript,
            $uninstallScript,
            $ciWorkflow,
            $releaseWorkflow
        ) | ForEach-Object {
            $_ | Should -Exist
        }
    }

    It 'defines a per-machine MSI containing only owned package inputs' {
        $wix = Read-RepositoryFile $wixSource

        $wix | Should -Match '<Package\s[^>]*Scope="perMachine"'
        $wix | Should -Match '<StandardDirectory\s+Id="ProgramFiles64Folder"'
        $wix | Should -Match '\$\(var\.AgentSource\)'
        $wix | Should -Match '\$\(var\.TraySource\)'
        $wix | Should -Match '\$\(var\.RootfsSource\)'
        $wix | Should -Match '\$\(var\.ProbeScriptSource\)'
        $wix | Should -Match '\$\(var\.ProvisionScriptSource\)'
        $wix | Should -Match '\$\(var\.UninstallScriptSource\)'
        $wix | Should -Match '\$\(var\.UpdateScriptSource\)'
        $wix | Should -Not -Match 'RemoveFolderEx|wsl\.exe|--unregister|ProgramData\\RemoteDocker|\.docker|workspace|pairing'
    }

    It 'registers separate exact-path Agent and Tray startup values for interactive users' {
        $wix = Read-RepositoryFile $wixSource

        $wix | Should -Match 'Root="HKLM"[^>]+Key="Software\\Microsoft\\Windows\\CurrentVersion\\Run"[^>]+Name="RemoteDockerAgent"[^>]+Value="&quot;\[INSTALLFOLDER\]RemoteDockerAgent\.exe&quot;"'
        $wix | Should -Match 'Root="HKLM"[^>]+Key="Software\\Microsoft\\Windows\\CurrentVersion\\Run"[^>]+Name="RemoteDockerTray"[^>]+Value="&quot;\[INSTALLFOLDER\]RemoteDockerTray\.exe&quot;"'
        $wix | Should -Not -Match 'ServiceInstall|LocalSystem|RunAs|powershell(?:\.exe)?[^<]*(?:RemoteDockerAgent|RemoteDockerTray)'
        ([regex]::Matches($wix, 'Software\\Microsoft\\Windows\\CurrentVersion\\Run')).Count | Should -Be 2
    }

    It 'keeps provisioning machine-scoped and firewall access Private-only and program-scoped' {
        $script = Read-RepositoryFile $provisionScript
        $distroPathCheck = $script.IndexOf('Assert-NoReparseDirectory -Path $distroRoot')
        $import = $script.IndexOf("'--import'")

        $script | Should -Match 'GetFolderPath\(\[Environment\+SpecialFolder\]::CommonApplicationData\)'
        $script | Should -Match 'GetFolderPath\(\[Environment\+SpecialFolder\]::ProgramFiles\)'
        $script | Should -Match 'Join-Path \$programData ''RemoteDocker'''
        $script | Should -Match 'Join-Path \$programFiles ''Remote Docker\\RemoteDockerAgent\.exe'''
        $script | Should -Match 'Invoke-External\s+-FilePath\s+\$agentExecutable\s+-ArgumentList\s+@\(''--prepare-wsl''\)'
        $script | Should -Match '''/usr/local/bin/remote-docker-remote'',\s*''runtime-status'''
        $script | Should -Not -Match 'syncthing generate --home=/var/lib/remote-docker|ssh-keygen'
        $script | Should -Match 'Join-Path \$PSScriptRoot ''\.\.\\assets\\remote-docker-rootfs\.tar\.zst'''
        $script | Should -Match '-Profile\s+Private'
        $script | Should -Match '-Program\s+\$agentExecutable'
        $script | Should -Match '-Protocol\s+TCP'
        $script | Should -Match '-RemoteAddress\s+LocalSubnet'
        $script | Should -Match "Name\s*=\s*'RemoteDocker\.Managed\.SSH'"
        $script | Should -Match "Name\s*=\s*'RemoteDocker\.Managed\.Syncthing'"
        $script | Should -Match '-Group\s+\$firewallRuleGroup'
        $script | Should -Not -Match 'Remove-NetFirewallRule\s+-DisplayName'
        $script | Should -Match 'function\s+Assert-NoReparseDirectory'
        $distroPathCheck | Should -BeGreaterThan -1
        $import | Should -BeGreaterThan $distroPathCheck
        $script | Should -Not -Match '\[string\]\$(?:RootfsPath|RootfsSha256|InstallRoot|AgentExecutable)'
        $script | Should -Not -Match '-Profile\s+(?:Public|Domain|Any)|New-ScheduledTask|Register-ScheduledTask|RunLevel\s+Highest'
    }

    It 'preserves data by default and requires an exact high-friction deletion phrase' {
        $script = Read-RepositoryFile $uninstallScript
        $normalExit = $script.IndexOf('if (-not $DeleteData)')
        $unregister = $script.IndexOf('--unregister')
        $treeCheck = $script.IndexOf('Assert-NoReparseTree -Path $installRoot')
        $recursiveDelete = $script.IndexOf('Remove-Item -LiteralPath $installRoot -Recurse -Force')

        $script | Should -Match 'GetFolderPath\(\[Environment\+SpecialFolder\]::CommonApplicationData\)'
        $script | Should -Match 'Join-Path \$programData ''RemoteDocker'''
        $script | Should -Match 'RemoteDockerAgent\.exe'
        $script | Should -Match '--delete-wsl-credential'
        $script | Should -Match 'ValidateSet\(''DELETE-REMOTE-DOCKER-DATA''\)'
        $script | Should -Match '\$DataRemovalConfirmation\s+-ne\s+''DELETE-REMOTE-DOCKER-DATA'''
        $script | Should -Not -Match '\[string\]\$InstallRoot'
        $script | Should -Match 'remote-docker-managed-v1'
        $script | Should -Match "'RemoteDocker\.Managed\.SSH'"
        $script | Should -Match "'RemoteDocker\.Managed\.Syncthing'"
        $script | Should -Match 'Get-NetFirewallRule\s+-Name\s+\$ruleName'
        $script | Should -Not -Match 'Remove-NetFirewallRule\s+-DisplayName'
        $script | Should -Match 'Resolve-Path\s+-LiteralPath\s+\$installRoot'
        $script | Should -Match 'FileAttributes\]::ReparsePoint'
        $script | Should -Match 'function\s+Assert-NoReparseTree'
        $script | Should -Match 'StringComparison\]::OrdinalIgnoreCase'
        $normalExit | Should -BeGreaterThan -1
        $unregister | Should -BeGreaterThan $normalExit
        $treeCheck | Should -BeGreaterThan $normalExit
        $recursiveDelete | Should -BeGreaterThan $treeCheck
        $script | Should -Not -Match 'ScheduledTask'
        $script | Should -Not -Match 'Remove-Item[^\n]*(?:\$HOME|\$env:USERPROFILE|\\Users\\|\*|\?)'
    }

    It 'verifies hash and Authenticode before an atomic same-volume replacement' {
        $script = Read-RepositoryFile $updateScript
        $hashCheck = $script.IndexOf('Get-FileHash')
        $signatureCheck = $script.IndexOf('Get-AuthenticodeSignature')
        $signerCheck = $script.IndexOf('SignerCertificate.Thumbprint')
        $replace = $script.IndexOf('[System.IO.File]::Replace')
        $stagingTreeCheck = $script.IndexOf('Assert-NoReparseTree -Path $stagingRoot')
        $stagingDelete = $script.IndexOf('Remove-Item -LiteralPath $stagingRoot -Recurse -Force')

        $script | Should -Match 'ValidateSet\(''RemoteDockerAgent\.exe'', ''RemoteDockerTray\.exe''\)'
        $script | Should -Match 'ValidatePattern\(''\^\[A-Fa-f0-9\]\{64\}\$''\)'
        $script | Should -Match 'ValidatePattern\(''\^\[A-Fa-f0-9\]\{40\}\$''\)'
        $script | Should -Match '(?m)^[^\n]*\[string\]\$ExpectedSha256' -Because 'the expected digest must be caller supplied'
        $script | Should -Match '(?m)^[^\n]*\[string\]\$ExpectedSignerThumbprint' -Because 'the expected signer identity must be caller supplied'
        $script | Should -Match 'Status\s+-ne\s+\[System\.Management\.Automation\.SignatureStatus\]::Valid'
        $script | Should -Match 'SignerCertificate\.Thumbprint\.Replace\(''\s'',\s*''''\)\.ToUpperInvariant\(\)'
        $script | Should -Match 'Get-AuthenticodeSignature\s+-LiteralPath\s+\$activePath'
        $script | Should -Match '\$activeThumbprint\s+-ne\s+\$ExpectedSignerThumbprint\.Replace'
        $script | Should -Match 'GetFolderPath\(\[Environment\+SpecialFolder\]::ProgramFiles\)'
        $script | Should -Match 'Join-Path\s+\$programFiles\s+''Remote Docker'''
        $script | Should -Match 'Join-Path\s+\$installRoot\s+''\.updates'''
        $script | Should -Not -Match '\[string\]\$InstallRoot'
        $script | Should -Match 'FileAttributes\]::ReparsePoint'
        $script | Should -Match 'function\s+Assert-NoReparseTree'
        $script | Should -Match 'GetPathRoot'
        $script | Should -Match '(?s)finally\s*\{.*Remove-Item\s+-LiteralPath\s+\$stagingRoot\s+-Recurse\s+-Force'
        $hashCheck | Should -BeGreaterThan -1
        $signatureCheck | Should -BeGreaterThan $hashCheck
        $signerCheck | Should -BeGreaterThan $signatureCheck
        $replace | Should -BeGreaterThan $signerCheck
        $stagingTreeCheck | Should -BeGreaterThan -1
        $stagingDelete | Should -BeGreaterThan $stagingTreeCheck
        $script | Should -Not -Match 'Write-(?:Host|Output|Verbose|Debug)[^\n]*(?:token|secret|password|private.?key|certificate)'
    }

    It 'pins package tools and emits unsigned development artifacts by default' {
        $script = Read-RepositoryFile $buildScript

        $script | Should -Match '\$GoVersion\s*=\s*''1\.26\.5'''
        $script | Should -Match '\$WixVersion\s*=\s*''6\.0\.2'''
        $script | Should -Match '''tool'',\s*''install''[\s\S]+''--version'',\s*\$WixVersion'
        $script | Should -Match '''--configfile'',\s*\$nugetConfig'
        $script | Should -Match '\$env:GOOS\s*=\s*''windows'''
        $script | Should -Match '\$env:GOARCH\s*=\s*''amd64'''
        $script | Should -Match '''-C'',\s*\$repoRoot,\s*''build'''
        $script | Should -Match 'cmd/remote-docker-agent'
        $script | Should -Match 'cmd/remote-docker-tray'
        $script | Should -Not -Match 'SignTool|certificate|password|token|secret'
        $nuget = Read-RepositoryFile $nugetConfig
        $nuget | Should -Match '<add key="signatureValidationMode" value="require"'
        $nuget | Should -Match 'fingerprint="D95336DD2022934D80E3F3A4F938DD66EC7076BBBA680F76C11F2B54B346D61D"'
    }

    It 'keeps pull-request CI targeted and its artifacts unsigned' {
        $workflow = Read-RepositoryFile $ciWorkflow

        $workflow | Should -Match '(?m)^\s*pull_request:'
        $workflow | Should -Match 'windows-latest'
        $workflow | Should -Match 'tests/integration/windows_package\.Tests\.ps1'
        $workflow | Should -Match 'packaging/windows/build-msi\.ps1'
        $workflow | Should -Match 'Install-Module\s+Pester\s+-RequiredVersion\s+6\.0\.0'
        $workflow | Should -Match 'actions/checkout@v7'
        $workflow | Should -Match 'actions/setup-go@v7'
        $workflow | Should -Match 'actions/upload-artifact@v7'
        $workflow | Should -Match 'actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1'
        $workflow | Should -Match 'actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e'
        $workflow | Should -Match 'actions/upload-artifact@043fb46d1a93c77aae656e7c1c64a875d1fc6a0a'
        $workflow | Should -Match 'actions/download-artifact@37930b1c2abaa49bbe596cd826c3c89aef350131'
        $workflow | Should -Not -Match 'SignTool|WINDOWS_SIGNING|notary|release\s+create'
    }

    It 'publishes only verified signed tag artifacts with checksums and SBOMs' {
        $workflow = Read-RepositoryFile $releaseWorkflow

        $workflow | Should -Match '(?m)^\s*tags:\s*\n\s*-\s*''v\*'''
        $workflow | Should -Match "github\.repository\s*==\s*'Dmitbd/remote-docker'"
        $workflow | Should -Match 'refs/tags/v'
        $workflow | Should -Match 'git\s+cat-file\s+-t'
        $workflow | Should -Match 'git/tags/\$tagObject'
        $workflow | Should -Match 'verification\.verified'
        $workflow | Should -Match 'WINDOWS_SIGNING_CERTIFICATE_BASE64:\s*\$\{\{\s*secrets\.'
        $workflow | Should -Match 'Get-AuthenticodeSignature'
        $workflow | Should -Match 'Get-FileHash[^\n]+SHA256'
        $workflow | Should -Match 'syft@v1\.50\.0'
        $workflow | Should -Match 'REMOTE_DOCKER_APP_SIGN_IDENTITY'
        $workflow | Should -Match 'REMOTE_DOCKER_INSTALLER_SIGN_IDENTITY'
        $workflow | Should -Match 'notarytool\s+store-credentials'
        $workflow | Should -Match 'stapler\s+validate'
        $workflow | Should -Match 'needs:\s*\[windows-release, macos-release\]'
        $workflow | Should -Match 'gh\s+release\s+create'
        $workflow | Should -Match '(?s)Get-AuthenticodeSignature.*Get-FileHash.*syft@v1\.50\.0.*gh\s+release\s+create'
        $workflow | Should -Not -Match '(?m)^\s*(?:echo|Write-Host).*(?:WINDOWS_SIGNING|password|secret|private.?key|token)'
    }

    It 'keeps public package content generic' {
        $publicFiles = @(
            $wixSource,
            $buildScript,
            $updateScript,
            $nugetConfig,
            $provisionScript,
            $uninstallScript,
            $ciWorkflow,
            $releaseWorkflow,
            $testScript
        )
        $internalPattern = 'S' + 'ber|Co' + 'work|Mid' + 'gard|Ygg' + 'drasil'

        foreach ($file in $publicFiles) {
            if (Test-Path -LiteralPath $file -PathType Leaf) {
                (Read-RepositoryFile $file) | Should -Not -Match $internalPattern
            }
        }
    }
}
