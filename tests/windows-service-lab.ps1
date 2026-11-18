#Requires -Version 7.4
[CmdletBinding()]
param([Parameter(Mandatory)] [string]$Executable)

$ErrorActionPreference = 'Stop'
if ($env:GITHUB_ACTIONS -ne 'true' -or $env:RUNNER_ENVIRONMENT -ne 'github-hosted') {
    throw 'Run this fixture on an ephemeral GitHub-hosted Windows VM.'
}
$principal = [Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'The Windows lab requires an elevated runner process.'
}

Add-Type -TypeDefinition @'
using System;
using System.ComponentModel;
using System.Runtime.InteropServices;

public static class LabServiceLogon
{
    [StructLayout(LayoutKind.Sequential)]
    private struct LsaAttributes {
        public int Length;
        public IntPtr RootDirectory, ObjectName;
        public int Attributes;
        public IntPtr SecurityDescriptor, SecurityQualityOfService;
    }
    [StructLayout(LayoutKind.Sequential)]
    private struct LsaString { public ushort Length, MaximumLength; public IntPtr Buffer; }
    [DllImport("advapi32.dll")] private static extern uint LsaOpenPolicy(IntPtr system, ref LsaAttributes attr, uint access, out IntPtr policy);
    [DllImport("advapi32.dll")] private static extern uint LsaAddAccountRights(IntPtr policy, byte[] sid, LsaString[] rights, uint count);
    [DllImport("advapi32.dll")] private static extern uint LsaClose(IntPtr policy);
    [DllImport("advapi32.dll")] private static extern uint LsaNtStatusToWinError(uint status);

    private static void Check(uint status)
    {
        if (status != 0) throw new Win32Exception((int)LsaNtStatusToWinError(status));
    }

    public static void Grant(byte[] sid)
    {
        var attributes = new LsaAttributes { Length = Marshal.SizeOf<LsaAttributes>() };
        // POLICY_LOOKUP_NAMES | POLICY_CREATE_ACCOUNT
        Check(LsaOpenPolicy(IntPtr.Zero, ref attributes, 0x810, out IntPtr policy));
        const string right = "SeServiceLogonRight";
        IntPtr text = Marshal.StringToHGlobalUni(right);
        try {
            var name = new LsaString {
                Length = (ushort)(right.Length * 2),
                MaximumLength = (ushort)((right.Length + 1) * 2),
                Buffer = text
            };
            Check(LsaAddAccountRights(policy, sid, new[] { name }, 1));
        } finally {
            Marshal.FreeHGlobal(text);
            LsaClose(policy);
        }
    }
}
'@

$accounts = @()
$services = @()
$sc = Join-Path $env:SystemRoot 'System32\sc.exe'
$register = Join-Path $PSScriptRoot '..\examples\Register-BridgeService.ps1'
$root = Join-Path $env:RUNNER_TEMP ('Bridge SCM lab ' + [guid]::NewGuid().ToString('N'))
$suffix = [guid]::NewGuid().ToString('N').Substring(0, 8)

function New-LabAccount([string]$Name) {
    $passwordText = 'Lab-Aa9!' + [Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes(18))
    $password = ConvertTo-SecureString $passwordText -AsPlainText -Force
    $account = New-LocalUser -Name $Name -Password $password -AccountNeverExpires
    $script:accounts += $account.Name
    $sid = [byte[]]::new($account.SID.BinaryLength)
    $account.SID.GetBinaryForm($sid, 0)
    [LabServiceLogon]::Grant($sid)
    return [pscustomobject]@{
        Account = $account
        Credential = [pscredential]::new("$env:COMPUTERNAME\$Name", $password)
    }
}

function Set-LabDirectoryAcl([string]$Path, [hashtable]$Permissions) {
    $acl = [Security.AccessControl.DirectorySecurity]::new()
    $acl.SetAccessRuleProtection($true, $false)
    $Permissions['S-1-5-18'] = 'FullControl'
    $Permissions['S-1-5-32-544'] = 'FullControl'
    foreach ($entry in $Permissions.GetEnumerator()) {
        $rule = [Security.AccessControl.FileSystemAccessRule]::new(
            [Security.Principal.SecurityIdentifier]::new($entry.Key),
            [Security.AccessControl.FileSystemRights]$entry.Value,
            'ContainerInherit,ObjectInherit', 'None', 'Allow')
        $acl.AddAccessRule($rule)
    }
    Set-Acl -LiteralPath $Path -AclObject $acl
}

function New-LabListener {
    $listener = [System.Net.Sockets.TcpListener]::new([ipaddress]::Loopback, 0)
    $listener.Start()
    return $listener
}

function Get-LabService([string]$Name) {
    Get-CimInstance Win32_Service -Filter "Name = '$Name'"
}

function Wait-LabService([string]$Name, [int]$DifferentFrom = 0) {
    $end = [DateTime]::UtcNow.AddSeconds(40)
    do {
        $state = Get-LabService $Name
        if ($state.State -eq 'Running' -and $state.ProcessId -gt 0 -and $state.ProcessId -ne $DifferentFrom) { return $state }
        Start-Sleep -Milliseconds 350
    } while ([DateTime]::UtcNow -lt $end)
    throw "Service $Name did not become Running with a new process."
}

function Assert-LabStatus([string]$Url, [int]$Expected) {
    $response = Invoke-WebRequest -Uri $Url -NoProxy -SkipHttpErrorCheck -TimeoutSec 5
    if ([int]$response.StatusCode -ne $Expected) { throw "Expected HTTP $Expected at $Url, got $($response.StatusCode)." }
}

function Assert-LabAccess([pscredential]$Credential, [string]$Path, [bool]$Allowed) {
    $encodedPath = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($Path))
    $command = @'
try {
    $path = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('FIXTURE_PATH'))
    $file = [IO.File]::OpenRead($path)
    $file.Dispose()
    exit 0
} catch [UnauthorizedAccessException] { exit 13 }
catch { exit 1 }
'@
    $encoded = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($command.Replace('FIXTURE_PATH', $encodedPath)))
    $powershell = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
    $parameters = @{
        FilePath = $powershell; Credential = $Credential; WorkingDirectory = $env:SystemRoot
        WindowStyle = 'Hidden'; Wait = $true; PassThru = $true
        ArgumentList = @('-NoProfile', '-NonInteractive', '-EncodedCommand', $encoded)
    }
    $probe = Start-Process @parameters
    $expected = if ($Allowed) { 0 } else { 13 }
    if ($probe.ExitCode -ne $expected) { throw "Unexpected file access result from $($Credential.UserName): $($probe.ExitCode)." }
}

try {
    $bridge = New-LabAccount "BridgeSvc$suffix"
    $anchor = New-LabAccount "AnchorSvc$suffix"
    $bridgeSID = $bridge.Account.SID.Value
    $anchorSID = $anchor.Account.SID.Value
    $bridgeRoot = Join-Path $root 'bridge'
    $anchorRoot = Join-Path $root 'anchor'
    New-Item -ItemType Directory -Path $root,$bridgeRoot,$anchorRoot | Out-Null
    Set-LabDirectoryAcl $root @{ $bridgeSID = 'ReadAndExecute'; $anchorSID = 'ReadAndExecute' }
    Set-LabDirectoryAcl $bridgeRoot @{ $bridgeSID = 'Modify' }
    Set-LabDirectoryAcl $anchorRoot @{ $anchorSID = 'Modify' }
    $app = Join-Path $root 'bridge lab.exe'
    Copy-Item -LiteralPath $Executable -Destination $app

    $ports = @((New-LabListener), (New-LabListener))
    try {
        $bridgePort, $anchorPort = $ports | ForEach-Object { $_.LocalEndpoint.Port }
    } finally {
        $ports | ForEach-Object { $_.Stop() }
    }
    $bridgeListen = "127.0.0.1:$bridgePort"
    $anchorListen = "127.0.0.1:$anchorPort"
    $bridgeUrl = "http://$bridgeListen"
    $anchorUrl = "http://$anchorListen"
    $config = Join-Path $bridgeRoot 'bridge config.json'
    $bridgeSecrets = Join-Path $bridgeRoot 'bridge secrets.json'
    $anchorSecrets = Join-Path $anchorRoot 'anchor secrets.json'
    @{
        public_base_url = "$bridgeUrl/bridge/"; gitea_public_url = 'http://127.0.0.1:3000/'
        gitea_api_url = 'http://127.0.0.1:3000/'; client_id = 'lab-fixture'; allow_loopback_http = $true
        database_path = Join-Path $bridgeRoot 'bridge.sqlite3'
        page_size = 5; max_pages = 2
        projects = @(@{ id='lab'; name='Lab'; repositories=@(@{ id=1; owner='fixture'; name='fixture' }) })
    } | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath $config -Encoding utf8NoBOM
    @{ oauth_client_secret = 'scm-lab-fixture' } | ConvertTo-Json | Set-Content -LiteralPath $bridgeSecrets -Encoding utf8NoBOM
    @{
        anchor_signing_seed = [Convert]::ToBase64String([Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
        anchor_bearer = [Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
    } | ConvertTo-Json | Set-Content -LiteralPath $anchorSecrets -Encoding utf8NoBOM

    $bridgeService = "BridgeLab$suffix"
    $anchorService = "AnchorLab$suffix"
    $bridgeRegistration = @{
        Name = $bridgeService; Executable = $app; Config = $config; Secrets = $bridgeSecrets
        LogFile = Join-Path $bridgeRoot 'bridge.log'; Listen = $bridgeListen; Credential = $bridge.Credential
    }
    $anchorRegistration = @{
        Name = $anchorService; Executable = $app; AnchorDatabase = Join-Path $anchorRoot 'anchor.sqlite3'
        Secrets = $anchorSecrets; LogFile = Join-Path $anchorRoot 'anchor.log'; Listen = $anchorListen; Credential = $anchor.Credential
    }
    & $register @bridgeRegistration
    $services += $bridgeService
    & $register @anchorRegistration
    $services += $anchorService

    foreach ($entry in @($bridgeRegistration, $anchorRegistration)) {
        $serviceName = $entry.Name
        $state = Get-LabService $serviceName
        $delayed = (Get-ItemProperty -LiteralPath "HKLM:\SYSTEM\CurrentControlSet\Services\$serviceName").DelayedAutoStart
        $recovery = (& $sc qfailure $serviceName) -join [Environment]::NewLine
        $startName = $state.StartName -replace '^\.\\', "$env:COMPUTERNAME\"
        $actualSID = [Security.Principal.NTAccount]::new($startName).Translate([Security.Principal.SecurityIdentifier])
        $expectedSID = [Security.Principal.NTAccount]::new($entry.Credential.UserName).Translate([Security.Principal.SecurityIdentifier])
        if ($state.State -ne 'Stopped' -or $actualSID -ne $expectedSID -or $state.StartMode -ne 'Auto' -or $delayed -ne 1 -or
            $recovery -notmatch '\b5000\b' -or $recovery -notmatch '\b30000\b' -or $recovery -notmatch '\b120000\b') {
            throw "Unexpected SCM configuration for ${serviceName}: state $($state.State), identity $($state.StartName), mode $($state.StartMode), delayed $delayed. $recovery"
        }
        try {
            Start-Service -Name $serviceName
            $null = Wait-LabService $serviceName
        } catch {
            if (Test-Path -LiteralPath $entry.LogFile) { Get-Content -LiteralPath $entry.LogFile -Tail 10 | Write-Host }
            throw
        }
    }
    Assert-LabStatus "$bridgeUrl/bridge/" 200
    Assert-LabStatus "$bridgeUrl/bridge/api/projects" 401
    Assert-LabStatus "$anchorUrl/v1/anchors" 405
    Assert-LabAccess $bridge.Credential $bridgeSecrets $true
    Assert-LabAccess $bridge.Credential $anchorSecrets $false
    Assert-LabAccess $anchor.Credential $anchorSecrets $true
    Assert-LabAccess $anchor.Credential $bridgeSecrets $false
    foreach ($port in @($bridgePort, $anchorPort)) {
        $listeners = @(Get-NetTCPConnection -State Listen -LocalPort $port)
        if ($listeners.Count -ne 1 -or $listeners[0].LocalAddress -ne '127.0.0.1') { throw "Unexpected listener on port $port." }
    }
    Write-Host 'SCM verified both service identities, delayed start, recovery settings, loopback bindings and ACL isolation.'

    $current = Get-LabService $bridgeService
    $process = Get-CimInstance Win32_Process -Filter "ProcessId = $($current.ProcessId)"
    if ($process.ExecutablePath -ne $app) { throw 'SCM returned an unexpected process for the crash fixture.' }
    Stop-Process -Id $current.ProcessId -Force
    $recovered = Wait-LabService $bridgeService $current.ProcessId
    Assert-LabStatus "$bridgeUrl/bridge/" 200
    Write-Host "SCM restarted Bridge after terminating the fixture process: $($current.ProcessId) -> $($recovered.ProcessId)."

    foreach ($serviceName in $services) {
        Stop-Service -Name $serviceName
        (Get-Service -Name $serviceName).WaitForStatus('Stopped', [timespan]::FromSeconds(20))
    }
    Write-Host 'Both services accepted SCM stop.'
} finally {
    foreach ($serviceName in $services) {
        Stop-Service -Name $serviceName -ErrorAction SilentlyContinue
        & $sc delete $serviceName | Out-Null
    }
    foreach ($userName in $accounts) {
        Remove-LocalUser -Name $userName -ErrorAction SilentlyContinue
    }
}
