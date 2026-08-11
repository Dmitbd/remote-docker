[CmdletBinding()]
param([ValidateRange(1, 300)][int]$TimeoutSeconds = 30)

$ErrorActionPreference = 'Stop'
$deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
do {
    $running = @(Get-Process -Name 'RemoteDocker' -ErrorAction SilentlyContinue)
    if ($running.Count -eq 0) { exit 0 }
    if ([DateTime]::UtcNow -ge $deadline) {
        [Console]::Error.WriteLine('Remote Docker is still running after the shutdown request.')
        exit 1
    }
    Start-Sleep -Milliseconds 100
} while ($true)
