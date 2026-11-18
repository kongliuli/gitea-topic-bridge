[CmdletBinding(SupportsShouldProcess)]
param(
    [Parameter(Mandatory)]
    [ValidatePattern('^[A-Za-z][A-Za-z0-9_-]{0,79}$')]
    [string]$Name,
    [Parameter(Mandatory)]
    [string]$Executable,
    [Parameter(Mandatory, ParameterSetName='Bridge')]
    [string]$Config,
    [Parameter(Mandatory, ParameterSetName='Anchor')]
    [string]$AnchorDatabase,
    [Parameter(Mandatory)]
    [string]$Secrets,
    [Parameter(Mandatory)]
    [string]$LogFile,
    [Parameter(Mandatory)]
    [string]$Listen,
    [Parameter(ParameterSetName='Bridge')]
    [string]$HookListen,
    [Parameter(Mandatory)]
    [pscredential]$Credential
)

$ErrorActionPreference = 'Stop'

function Get-LocalFilePath([string]$Path, [bool]$MustExist) {
    if ($Path -notmatch '^[A-Za-z]:[\\/]' -or $Path.Contains('"') -or $Path.Contains('%') -or
        $Path -match '[\x00-\x1f]' -or $Path.EndsWith('\') -or $Path.EndsWith('/')) {
        throw 'Specify absolute, drive-qualified file paths without quotes or environment expansion.'
    }
    $resolved = [IO.Path]::GetFullPath($Path)
    if ($MustExist -and -not (Test-Path -LiteralPath $resolved -PathType Leaf)) {
        throw "Required file does not exist: $resolved"
    }
    if (-not (Test-Path -LiteralPath (Split-Path -LiteralPath $resolved) -PathType Container)) {
        throw "Parent directory does not exist: $resolved"
    }
    return $resolved
}

function Assert-Loopback([string]$Address) {
    if ($Address -notmatch '^(127\.0\.0\.1|\[::1\]):([0-9]{1,5})$' -or [int]$Matches[2] -lt 1 -or [int]$Matches[2] -gt 65535) {
        throw 'Use an explicit 127.0.0.1 or [::1] address with a port from 1 to 65535.'
    }
}

Assert-Loopback $Listen
if ($HookListen) {
    Assert-Loopback $HookListen
    if ($HookListen -eq $Listen) { throw 'Webhook and web listeners must use different addresses.' }
}
if ($Credential.UserName -match '^(?:LocalSystem|SYSTEM|(?:NT AUTHORITY|\.)\\(?:SYSTEM|LocalSystem|LocalService|NetworkService))$') {
    throw 'Use a dedicated low-privilege Windows account.'
}
if (Get-Service -Name $Name -ErrorAction SilentlyContinue) { throw "Service already exists: $Name" }
$Executable = Get-LocalFilePath $Executable $true
if ([IO.Path]::GetExtension($Executable) -ne '.exe') { throw 'Executable must be a Windows .exe file.' }
$Secrets = Get-LocalFilePath $Secrets $true
$LogFile = Get-LocalFilePath $LogFile $false
$arguments = @('"-service"', ('"{0}"' -f $Name), '"-listen"', ('"{0}"' -f $Listen),
    '"-secrets"', ('"{0}"' -f $Secrets), '"-log-file"', ('"{0}"' -f $LogFile))
if ($PSCmdlet.ParameterSetName -eq 'Anchor') {
    $AnchorDatabase = Get-LocalFilePath $AnchorDatabase $false
    $arguments += @('"-anchor-db"', ('"{0}"' -f $AnchorDatabase))
} else {
    $Config = Get-LocalFilePath $Config $true
    $arguments += @('"-config"', ('"{0}"' -f $Config))
    if ($HookListen) { $arguments += @('"-hook-listen"', ('"{0}"' -f $HookListen)) }
}
$binaryPath = ('"{0}" ' -f $Executable) + ($arguments -join ' ')
if (-not $PSCmdlet.ShouldProcess($Name, 'Register stopped Bridge service with delayed start and recovery')) { return }
$identity = [Security.Principal.WindowsIdentity]::GetCurrent()
if (-not ([Security.Principal.WindowsPrincipal]$identity).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'Run registration in an elevated PowerShell on the deployment host.'
}

$sc = Join-Path $env:SystemRoot 'System32\sc.exe'
New-Service -Name $Name -BinaryPathName $binaryPath -Credential $Credential -StartupType Manual | Out-Null
try {
    & $sc failure $Name 'reset=' 86400 'actions=' 'restart/5000/restart/30000/restart/120000'
    if ($LASTEXITCODE -ne 0) { throw 'Could not configure recovery delays.' }
    & $sc failureflag $Name 1
    if ($LASTEXITCODE -ne 0) { throw 'Could not configure recovery on nonzero service exit.' }
    & $sc config $Name 'start=' delayed-auto
    if ($LASTEXITCODE -ne 0) { throw 'Could not configure delayed automatic startup.' }
} catch {
    $registrationError = $_
    & $sc delete $Name
    if ($LASTEXITCODE -ne 0) { Write-Warning "Could not remove the incomplete registration for $Name." }
    throw $registrationError
}
Get-Service -Name $Name
