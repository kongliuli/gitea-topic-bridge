[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [string]$Database,
    [Parameter(Mandatory)]
    [string]$Destination,
    [string]$Snapshots
)

$ErrorActionPreference = 'Stop'

function Assert-AbsoluteLocalPath([string]$Path) {
    if ($Path -notmatch '^[A-Za-z]:[\\/]' -or $Path.Contains('%')) {
        throw 'Use an absolute local drive path.'
    }
    $normalized = [IO.Path]::GetFullPath($Path)
    $drive = [IO.DriveInfo]::new([IO.Path]::GetPathRoot($normalized))
    if ($drive.DriveType -ne [IO.DriveType]::Fixed) { throw 'Use a fixed local drive.' }
    return $normalized
}

function Get-PlainItem([string]$Path) {
    $item = Get-Item -LiteralPath $Path -Force
    $ancestor = $item
    while ($null -ne $ancestor) {
        if ($ancestor.Attributes -band [IO.FileAttributes]::ReparsePoint) {
            throw "Reparse point is not supported: $($ancestor.FullName)"
        }
        if ($ancestor -is [IO.DirectoryInfo]) { $ancestor = $ancestor.Parent }
        else { $ancestor = $ancestor.Directory }
    }
    return $item
}

$Database = Assert-AbsoluteLocalPath $Database
$Destination = Assert-AbsoluteLocalPath $Destination
$databaseItem = Get-PlainItem $Database
if ($databaseItem.PSIsContainer) { throw 'Database must be a file.' }
$target = Get-PlainItem $Destination
if (-not $target.PSIsContainer -or @(Get-ChildItem -LiteralPath $Destination -Force).Count -ne 0) {
    throw 'Destination must be a pre-created, empty, ACL-protected directory.'
}

$files = @{}
$files['state.sqlite3'] = $Database
if ($Snapshots) {
    $Snapshots = Assert-AbsoluteLocalPath $Snapshots
    $source = Get-PlainItem $Snapshots
    if (-not $source.PSIsContainer) { throw 'Snapshots must be a directory.' }
    $rootPrefix = $source.FullName.TrimEnd('\') + '\'
    if (($target.FullName.TrimEnd('\') + '\').StartsWith($rootPrefix, [StringComparison]::OrdinalIgnoreCase)) {
        throw 'Destination cannot be inside the snapshot directory.'
    }
    foreach ($receipt in Get-ChildItem -LiteralPath $Snapshots -Force) {
        if (-not $receipt.PSIsContainer -or ($receipt.Attributes -band [IO.FileAttributes]::ReparsePoint) -or
            $receipt.Name -cnotmatch '^[A-Za-z0-9_-]{43}$') {
            throw "Unexpected receipt snapshot directory: $($receipt.Name)"
        }
        foreach ($blob in Get-ChildItem -LiteralPath $receipt.FullName -Force) {
            if ($blob.PSIsContainer -or ($blob.Attributes -band [IO.FileAttributes]::ReparsePoint) -or $blob.Name -cnotmatch '^[1-8]\.blob$') {
                throw "Unexpected receipt snapshot file: $($blob.Name)"
            }
            $files[(Join-Path 'snapshots' (Join-Path $receipt.Name $blob.Name))] = $blob.FullName
        }
    }
}

function Assert-NoSidecars {
    foreach ($suffix in '-wal', '-shm', '-journal') {
        if (Test-Path -LiteralPath ($Database + $suffix)) {
            throw 'SQLite sidecars are present; close the application cleanly before copying.'
        }
    }
}

Assert-NoSidecars
if ($Snapshots) { [IO.Directory]::CreateDirectory((Join-Path $Destination 'snapshots')) | Out-Null }
foreach ($name in $files.Keys) {
    $outputPath = Join-Path $Destination $name
    [IO.Directory]::CreateDirectory([IO.Path]::GetDirectoryName($outputPath)) | Out-Null
    $inputStream = [IO.File]::Open($files[$name], [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::Read)
    try {
        $outputStream = [IO.File]::Open($outputPath, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write, [IO.FileShare]::None)
        try {
            $inputStream.CopyTo($outputStream)
            $outputStream.Flush($true)
        } finally { $outputStream.Dispose() }
    } finally { $inputStream.Dispose() }
}
Assert-NoSidecars
Write-Output "Copied $($files.Count) files to $Destination. Verify the copy before use."
