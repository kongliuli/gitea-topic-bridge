#Requires -Version 7.4
[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [ValidateNotNullOrEmpty()]
    [hashtable[]]$Services,
    [string]$BeforeRestart
)

$ErrorActionPreference = 'Stop'
if (-not $IsWindows) { throw 'Run this probe on the Windows service host.' }

function Resolve-AccountSid([string]$Name) {
    if (-not $Name) { return $null }
    if ($Name -eq 'LocalSystem') { return 'S-1-5-18' }
    $qualified = $Name -replace '^\.\\', "$env:COMPUTERNAME\"
    try {
        return [Security.Principal.NTAccount]::new($qualified).Translate([Security.Principal.SecurityIdentifier]).Value
    } catch [Security.Principal.IdentityNotMappedException] {
        return $null
    }
}

$observedAt = [DateTimeOffset]::UtcNow
$bootedAt = [DateTimeOffset](Get-CimInstance Win32_OperatingSystem).LastBootUpTime
$bootedAt = $bootedAt.ToUniversalTime()
$restart = $null
if ($BeforeRestart) {
    $baseline = Get-Content -LiteralPath $BeforeRestart -Raw | ConvertFrom-Json
    if ($baseline.schema -ne 1 -or $baseline.computer -ne $env:COMPUTERNAME -or -not $baseline.booted_at) {
        throw 'The baseline must be a probe report from this Windows host.'
    }
    $previousBoot = ([DateTimeOffset]$baseline.booted_at).ToUniversalTime()
    $restart = [ordered]@{
        previous_booted_at = $previousBoot.ToString('o')
        passed = $bootedAt -gt $previousBoot
    }
}

$names = @{}
foreach ($expected in $Services) {
    $name = [string]$expected.Name
    $ports = [int[]]@($expected.Ports)
    if ($name -cnotmatch '\A[A-Za-z][A-Za-z0-9_-]{0,79}\z' -or $names.ContainsKey($name) -or
        -not $expected.Account -or -not $ports -or ($ports.Where({ $_ -lt 1 -or $_ -gt 65535 })).Count -gt 0) {
        throw 'Supply a unique service Name, an Account and TCP Ports from 1 to 65535 for each entry.'
    }
    $names[$name] = $true
}

$listening = @(Get-NetTCPConnection -State Listen)
$seenSids = @{}
$findings = @(foreach ($expected in $Services) {
    $name = [string]$expected.Name
    $service = Get-CimInstance Win32_Service -Filter "Name = '$name'"
    $processId = [int]$service.ProcessId
    $process = $null
    $ownerSid = $null
    if ($processId -gt 0) {
        $process = Get-CimInstance Win32_Process -Filter "ProcessId = $processId"
        if ($process) {
            $owner = Invoke-CimMethod -InputObject $process -MethodName GetOwnerSid
            if ($owner.ReturnValue -eq 0) { $ownerSid = $owner.Sid }
        }
    }
    $running = $service.State -eq 'Running' -and $null -ne $process
    $automatic = $service.StartMode -eq 'Auto'
    $delayed = $false
    if ($service) {
        $registry = Get-ItemProperty -LiteralPath "HKLM:\SYSTEM\CurrentControlSet\Services\$name"
        $delayed = $registry.DelayedAutoStart -eq 1
    }
    $delayMatches = -not $expected.ContainsKey('DelayedAutoStart') -or $delayed -eq [bool]$expected.DelayedAutoStart

    $actualSid = Resolve-AccountSid $service.StartName
    $expectedSid = Resolve-AccountSid $expected.Account
    $accountMatches = $null -ne $expectedSid -and $actualSid -eq $expectedSid -and $ownerSid -eq $expectedSid
    $dedicated = $actualSid -and $actualSid -notin @('S-1-5-18', 'S-1-5-19', 'S-1-5-20') -and -not $seenSids.ContainsKey($actualSid)
    if ($actualSid) { $seenSids[$actualSid] = $true }

    $listeners = @()
    if ($running) { $listeners = @($listening.Where({ $_.OwningProcess -eq $processId })) }
    $ports = @(foreach ($port in @($expected.Ports)) {
        $bound = @($listeners.Where({ $_.LocalPort -eq $port }))
        [ordered]@{ port = $port; addresses = @($bound | ForEach-Object LocalAddress); passed = $bound.Count -gt 0 }
    })
    $offLoopback = @($listeners.Where({ -not [ipaddress]::IsLoopback([ipaddress]$_.LocalAddress) }))
    $scopeMatches = -not $expected.LoopbackOnly -or ($running -and $offLoopback.Count -eq 0)
    [ordered]@{
        name = $name
        state = $service.State
        process_id = $processId
        startup_mode = $service.StartMode
        automatic_start = $automatic
        delayed_auto_start = $delayed
        delay_matches = $delayMatches
        configured_account = $service.StartName
        configured_sid = $actualSid
        process_sid = $ownerSid
        expected_account = $expected.Account
        account_matches = $accountMatches
        dedicated_account = [bool]$dedicated
        loopback_required = [bool]$expected.LoopbackOnly
        loopback_matches = $scopeMatches
        non_loopback_listeners = @($offLoopback | Select-Object LocalAddress, LocalPort)
        ports = $ports
        passed = $running -and $automatic -and $delayMatches -and $accountMatches -and $dedicated -and
            $scopeMatches -and ($ports.Where({ -not $_.passed })).Count -eq 0
    }
})

$passed = ($null -eq $restart -or $restart.passed) -and ($findings.Where({ -not $_.passed })).Count -eq 0
[ordered]@{
    schema = 1
    computer = $env:COMPUTERNAME
    observed_at = $observedAt.ToString('o')
    booted_at = $bootedAt.ToString('o')
    restart = $restart
    services = $findings
    passed = $passed
} | ConvertTo-Json -Depth 6
if (-not $passed) { exit 1 }
exit 0
