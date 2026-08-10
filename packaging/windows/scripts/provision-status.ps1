function Write-RemoteDockerProvisionStatus {
    param(
        [Parameter(Mandatory = $true)][string]$ProgressPath,
        [Parameter(Mandatory = $true)][string]$Phase,
        [Parameter(Mandatory = $true)][ValidateSet('started', 'completed', 'failed', 'reboot_required')][string]$State,
        [Parameter(Mandatory = $true)][string]$Message
    )

    $record = [ordered]@{
        at = [DateTimeOffset]::UtcNow.ToString('O')
        phase = $Phase
        state = $State
        message = $Message
    } | ConvertTo-Json -Compress
    Add-Content -LiteralPath $ProgressPath -Value $record -Encoding utf8
}
