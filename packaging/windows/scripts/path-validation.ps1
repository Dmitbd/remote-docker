function Test-RemoteDockerFullyQualifiedPath {
    [CmdletBinding()]
    [OutputType([bool])]
    param([Parameter(Mandatory = $true)][string]$Path)

    if ([string]::IsNullOrWhiteSpace($Path)) {
        return $false
    }
    try {
        $root = [System.IO.Path]::GetPathRoot($Path)
        [void][System.IO.Path]::GetFullPath($Path)
    }
    catch {
        return $false
    }
    if ([string]::IsNullOrWhiteSpace($root) -or $root -eq '\' -or $root -match '^[A-Za-z]:$') {
        return $false
    }
    return $true
}

function Assert-RemoteDockerCanonicalPath {
    [CmdletBinding()]
    [OutputType([string])]
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [Parameter(Mandatory = $true)][string]$Description
    )

    if (-not (Test-RemoteDockerFullyQualifiedPath -Path $Path)) {
        throw "$Description must be an absolute path."
    }
    $canonical = [System.IO.Path]::GetFullPath($Path)
    if (-not [string]::Equals($canonical.TrimEnd('\'), $Path.TrimEnd('\'), [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "$Description must already be canonical."
    }
    $canonical
}
