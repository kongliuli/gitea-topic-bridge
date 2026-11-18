#Requires -Version 7.4
[CmdletBinding()]
param(
    [Parameter(Mandatory)] [string]$Executable,
    [Parameter(Mandatory)] [string]$GiteaExecutable,
    [Parameter(Mandatory)] [string]$CaddyExecutable
)

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
using System.IO;
using System.Runtime.InteropServices;
using System.Security.Principal;
using Microsoft.Win32.SafeHandles;

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
    [DllImport("advapi32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern bool LogonUser(string user, string domain, string password, int logonType, int provider, out SafeAccessTokenHandle token);

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

    public static bool CanOpen(string user, string domain, string password, string path, FileAccess mode)
    {
        // LOGON32_LOGON_SERVICE with the default provider exercises the service account token.
        if (!LogonUser(user, domain, password, 5, 0, out SafeAccessTokenHandle token))
            throw new Win32Exception(Marshal.GetLastWin32Error());
        using (token) {
            return WindowsIdentity.RunImpersonated(token, () => {
                try {
                    using (File.Open(path, FileMode.Open, mode, FileShare.ReadWrite)) {}
                    return true;
                } catch (UnauthorizedAccessException) {
                    return false;
                }
            });
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

function Stop-LabStack([string[]]$StartOrder, [int[]]$ListenPorts) {
    for ($i = $StartOrder.Count - 1; $i -ge 0; $i--) {
        $service = $StartOrder[$i]
        $processId = (Get-LabService $service).ProcessId
        $process = Get-Process -Id $processId
        try {
            $null = $process.Handle
            Stop-Service -Name $service
            (Get-Service -Name $service).WaitForStatus('Stopped', [timespan]::FromSeconds(20))
            if (-not $process.WaitForExit(40000)) { throw "Service $service reported Stopped but process $processId did not exit." }
        } finally {
            $process.Dispose()
        }
    }
    foreach ($port in $ListenPorts) {
        if (@(Get-NetTCPConnection -State Listen -LocalPort $port -ErrorAction SilentlyContinue).Count -ne 0) {
            throw "Port $port remained in Listen after all services stopped."
        }
    }
    Write-Host 'All four services accepted SCM stop and released their listeners.'
}

function Assert-LabStatus([string]$Url, [int]$Expected) {
    $response = Invoke-WebRequest -Uri $Url -NoProxy -SkipHttpErrorCheck -TimeoutSec 5
    if ([int]$response.StatusCode -ne $Expected) { throw "Expected HTTP $Expected at $Url, got $($response.StatusCode)." }
}

function Copy-LabFiles([string]$Source, [string]$Destination, [string[]]$Names) {
    foreach ($name in $Names) {
        Copy-Item -LiteralPath (Join-Path $Source $name) -Destination $Destination -Recurse -Force
    }
}

function Start-CheckedBridgeService([hashtable]$Registration) {
    & $register @Registration
    $serviceName = $Registration.Name
    $script:services += $serviceName
    $state = Get-LabService $serviceName
    $delayed = (Get-ItemProperty -LiteralPath "HKLM:\SYSTEM\CurrentControlSet\Services\$serviceName").DelayedAutoStart
    $recovery = (& $sc qfailure $serviceName) -join [Environment]::NewLine
    $startName = $state.StartName -replace '^\.\\', "$env:COMPUTERNAME\"
    $actualSID = [Security.Principal.NTAccount]::new($startName).Translate([Security.Principal.SecurityIdentifier])
    $expectedSID = [Security.Principal.NTAccount]::new($Registration.Credential.UserName).Translate([Security.Principal.SecurityIdentifier])
    if ($state.State -ne 'Stopped' -or $actualSID -ne $expectedSID -or $state.StartMode -ne 'Auto' -or $delayed -ne 1 -or
        $recovery -notmatch '\b5000\b' -or $recovery -notmatch '\b30000\b' -or $recovery -notmatch '\b120000\b') {
        throw "Unexpected SCM configuration for ${serviceName}: state $($state.State), identity $($state.StartName), mode $($state.StartMode), delayed $delayed. $recovery"
    }
    try {
        Start-Service -Name $serviceName
        $null = Wait-LabService $serviceName
    } catch {
        if (Test-Path -LiteralPath $Registration.LogFile) { Get-Content -LiteralPath $Registration.LogFile -Tail 10 | Write-Host }
        throw
    }
}

function Start-LabUpstream([string]$Name, [string]$BinaryPath, [pscredential]$Credential) {
    New-Service -Name $Name -BinaryPathName $BinaryPath -Credential $Credential -StartupType Automatic | Out-Null
    $script:services += $Name
    Start-Service -Name $Name
    $state = Wait-LabService $Name
    $startName = $state.StartName -replace '^\.\\', "$env:COMPUTERNAME\"
    $identity = [Security.Principal.NTAccount]::new($startName).Translate([Security.Principal.SecurityIdentifier])
    $expected = [Security.Principal.NTAccount]::new($Credential.UserName).Translate([Security.Principal.SecurityIdentifier])
    if ($identity -ne $expected) { throw "Unexpected account for $Name." }
}

function Wait-LabGitea([string]$Url, [string]$LogPath) {
    $end = [DateTime]::UtcNow.AddSeconds(30)
    do {
        try {
            if ((Invoke-WebRequest -NoProxy -TimeoutSec 2 -Uri "$Url/api/v1/version").StatusCode -eq 200) { return }
        } catch {}
        Start-Sleep -Milliseconds 300
    } while ([DateTime]::UtcNow -lt $end)
    if (Test-Path -LiteralPath $LogPath) { Get-Content -LiteralPath $LogPath -Tail 20 | Write-Host }
    throw 'Gitea did not start serving the local API.'
}

function New-GiteaLabUser([string]$Binary, [string[]]$Flags, [string]$Name, [string]$Scopes) {
    $arguments = $Flags + @('admin','user','create','--username',$Name,'--email',"$Name@example.invalid",
        '--random-password','--must-change-password=false','--access-token','--access-token-name','scm-lab','--access-token-scopes',$Scopes)
    if ($Name -eq 'labowner') { $arguments += '--admin' }
    $output = (& $Binary @arguments 2>&1) -join [Environment]::NewLine
    if ($LASTEXITCODE -ne 0) { throw "Gitea CLI could not create $Name." }
    $token = [regex]::Match($output, '(?i)access token was successfully created[.:]+\s+([a-f0-9]{40})')
    $password = [regex]::Match($output, "generated random password is '([^'\r\n]+)'")
    if (-not $token.Success -or -not $password.Success) { throw "Gitea CLI did not return fixture credentials for $Name." }
    return @{ Token = $token.Groups[1].Value; Password = $password.Groups[1].Value }
}

function Assert-LabAccess([pscredential]$Credential, [string]$Path, [bool]$Allowed, [System.IO.FileAccess]$Mode = 'Read') {
    $networkCredential = $Credential.GetNetworkCredential()
    $result = [LabServiceLogon]::CanOpen($networkCredential.UserName, $networkCredential.Domain, $networkCredential.Password, $Path, $Mode)
    if ($result -ne $Allowed) { throw "Unexpected $Mode access to $Path for $($Credential.UserName): $result." }
}

try {
    $bridge = New-LabAccount "BridgeSvc$suffix"
    $anchor = New-LabAccount "AnchorSvc$suffix"
    $gitea = New-LabAccount "GiteaSvc$suffix"
    $caddy = New-LabAccount "CaddySvc$suffix"
    $bridgeSID = $bridge.Account.SID.Value
    $anchorSID = $anchor.Account.SID.Value
    $bridgeRoot = Join-Path $root 'bridge'
    $anchorRoot = Join-Path $root 'anchor'
    $bridgeSettings = Join-Path $root 'bridge-config'
    $anchorSettings = Join-Path $root 'anchor-config'
    $inbox = Join-Path $root 'inbox'
    $clientRoot = Join-Path $root 'client'
    $giteaRoot = Join-Path $root 'gitea'
    $caddyRoot = Join-Path $root 'caddy'
    $giteaSID = $gitea.Account.SID.Value
    $caddySID = $caddy.Account.SID.Value
    New-Item -ItemType Directory -Path $root,$bridgeRoot,$anchorRoot,$bridgeSettings,$anchorSettings,$inbox,$giteaRoot,$caddyRoot,$clientRoot | Out-Null
    Set-LabDirectoryAcl $root @{ $bridgeSID = 'ReadAndExecute'; $anchorSID = 'ReadAndExecute'; $giteaSID = 'ReadAndExecute'; $caddySID = 'ReadAndExecute' }
    Set-LabDirectoryAcl $bridgeRoot @{ $bridgeSID = 'Modify' }
    Set-LabDirectoryAcl $anchorRoot @{ $anchorSID = 'Modify' }
    Set-LabDirectoryAcl $bridgeSettings @{ $bridgeSID = 'ReadAndExecute' }
    Set-LabDirectoryAcl $anchorSettings @{ $anchorSID = 'ReadAndExecute' }
    Set-LabDirectoryAcl $inbox @{ $bridgeSID = 'ReadAndExecute' }
    Set-LabDirectoryAcl $clientRoot @{}
    Set-LabDirectoryAcl $giteaRoot @{ $giteaSID = 'Modify' }
    Set-LabDirectoryAcl $caddyRoot @{ $caddySID = 'Modify' }
    $app = Join-Path $root 'bridge lab.exe'
    Copy-Item -LiteralPath $Executable -Destination $app
    $giteaApp = Join-Path $root 'gitea lab.exe'
    $caddyApp = Join-Path $root 'caddy lab.exe'
    Copy-Item -LiteralPath $GiteaExecutable -Destination $giteaApp
    Copy-Item -LiteralPath $CaddyExecutable -Destination $caddyApp

    $ports = @((New-LabListener), (New-LabListener), (New-LabListener), (New-LabListener))
    try {
        $bridgePort, $anchorPort, $giteaPort, $gatewayPort = $ports | ForEach-Object { $_.LocalEndpoint.Port }
    } finally {
        $ports | ForEach-Object { $_.Stop() }
    }
    $bridgeListen = "127.0.0.1:$bridgePort"
    $anchorListen = "127.0.0.1:$anchorPort"
    $bridgeUrl = "http://$bridgeListen"
    $anchorUrl = "http://$anchorListen"
    $giteaUrl = "http://127.0.0.1:$giteaPort"
    $gatewayAddress = "127.0.0.1:$gatewayPort"
    $gatewayUrl = "https://$gatewayAddress"
    $giteaIni = Join-Path $giteaRoot 'app.ini'
    $giteaPath = $giteaRoot.Replace('\','/')
    $giteaLog = Join-Path $giteaRoot 'log/gitea.log'
    $gitExecutable = (Get-Command git.exe).Source.Replace('\','/')
    @"
[server]
HTTP_ADDR = 127.0.0.1
HTTP_PORT = $giteaPort
ROOT_URL = $gatewayUrl/
PUBLIC_URL_DETECTION = never
LOCAL_ROOT_URL = $giteaUrl/
DISABLE_SSH = true
SSH_ROOT_PATH = $giteaPath/ssh
SSH_CREATE_AUTHORIZED_KEYS_FILE = false
SSH_CREATE_AUTHORIZED_PRINCIPALS_FILE = false
OFFLINE_MODE = true
APP_DATA_PATH = $giteaPath/data
[database]
DB_TYPE = sqlite3
PATH = $giteaPath/data/gitea.db
[repository]
ROOT = $giteaPath/repos
[security]
INSTALL_LOCK = true
[service]
DISABLE_REGISTRATION = true
[actions]
ENABLED = false
[migrations]
EGRESS_MODE = strict
ALLOWED_HOST_LIST = github.com:443
[git]
PATH = $gitExecutable
[log]
MODE = file
ROOT_PATH = $giteaPath/log
LEVEL = warn
"@ | Set-Content -LiteralPath $giteaIni -Encoding utf8NoBOM
    $giteaTemplate = Join-Path $giteaRoot 'custom/templates/custom'
    New-Item -ItemType Directory -Path $giteaTemplate | Out-Null
    Copy-Item -LiteralPath (Join-Path $PSScriptRoot '../examples/gitea/extra_links.tmpl') -Destination $giteaTemplate
    $giteaFlags = @('--work-path', $giteaRoot, '--config', $giteaIni)
    $giteaService = "GiteaLab$suffix"
    Start-LabUpstream $giteaService ('"{0}" --work-path "{1}" --config "{2}" web' -f $giteaApp,$giteaRoot,$giteaIni) $gitea.Credential
    Wait-LabGitea $giteaUrl $giteaLog
    $owner = New-GiteaLabUser $giteaApp $giteaFlags 'labowner' 'all'
    $reader = New-GiteaLabUser $giteaApp $giteaFlags 'labreader' 'read:repository,read:user'
    $developer = New-GiteaLabUser $giteaApp $giteaFlags 'labdeveloper' 'write:repository,read:user'
    $api = @{
        Headers = @{ Authorization = "Bearer $($owner.Token)" }; ContentType = 'application/json'
        TimeoutSec = 15; NoProxy = $true
    }
    $repo = Invoke-RestMethod @api -Method Post -Uri "$giteaUrl/api/v1/user/repos" -Body '{"name":"fixture","private":true,"auto_init":true,"default_branch":"main"}'
    $pilot = Invoke-RestMethod @api -Method Post -Uri "$giteaUrl/api/v1/user/repos" -Body '{"name":"pilot","private":true,"default_branch":"main"}'
    $null = Invoke-RestMethod @api -Method Put -Uri "$giteaUrl/api/v1/repos/labowner/fixture/collaborators/labreader" -Body '{"permission":"read"}'
    $null = Invoke-RestMethod @api -Method Put -Uri "$giteaUrl/api/v1/repos/labowner/pilot/collaborators/labreader" -Body '{"permission":"read"}'
    $null = Invoke-RestMethod @api -Method Put -Uri "$giteaUrl/api/v1/repos/labowner/pilot/collaborators/labdeveloper" -Body '{"permission":"write"}'
    $oauthBody = @{ name='Bridge SCM lab'; confidential_client=$true; redirect_uris=@("$gatewayUrl/bridge/auth/callback") }
    $oauth = Invoke-RestMethod @api -Method Post -Uri "$giteaUrl/api/v1/user/applications/oauth2" -Body ($oauthBody | ConvertTo-Json -Compress)
    $issuer = Invoke-RestMethod -NoProxy -TimeoutSec 10 -Uri "$giteaUrl/api/v1/user" -Headers @{ Authorization = "Bearer $($reader.Token)" }
    $caddyPath = $caddyRoot.Replace('\','/')
    $caddyConfig = Join-Path $caddyRoot 'Caddyfile'
    $caddyGlobal = @"
{
    admin off
    persist_config off
    skip_install_trust
    auto_https disable_redirects
    default_bind 127.0.0.1
    servers {
        protocols h1 h2
    }
    storage file_system {
        root "$caddyPath/storage"
    }
}
"@
    $caddySite = Get-Content -Raw -LiteralPath (Join-Path $PSScriptRoot '../examples/Caddyfile')
    ($caddyGlobal + [Environment]::NewLine + $caddySite.Replace('git.office.example:8443', $gatewayAddress).
        Replace('127.0.0.1:3000', "127.0.0.1:$giteaPort").Replace('127.0.0.1:3001', $bridgeListen)) |
        Set-Content -LiteralPath $caddyConfig -Encoding utf8NoBOM
    $caddyService = "CaddyLab$suffix"
    Start-LabUpstream $caddyService ('"{0}" run --config "{1}" --adapter caddyfile' -f $caddyApp,$caddyConfig) $caddy.Credential

    $config = Join-Path $bridgeSettings 'bridge config.json'
    $bridgeSecrets = Join-Path $bridgeSettings 'bridge secrets.json'
    $anchorSecrets = Join-Path $anchorSettings 'anchor secrets.json'
    $snapshots = Join-Path $bridgeRoot 'snapshots'
    $repoInbox = Join-Path $inbox ([string]$repo.id)
    New-Item -ItemType Directory -Path $snapshots,$repoInbox | Out-Null
    $artifact = Join-Path $repoInbox 'artifact.txt'
    Set-Content -LiteralPath $artifact -Encoding utf8NoBOM -NoNewline -Value 'SCM inbox fixture'
    $anchorBearer = [Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
    @{
        anchor_signing_seed = [Convert]::ToBase64String([Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
        anchor_bearer = $anchorBearer
    } | ConvertTo-Json | Set-Content -LiteralPath $anchorSecrets -Encoding utf8NoBOM
    $anchorService = "AnchorLab$suffix"
    $anchorRegistration = @{
        Name = $anchorService; Executable = $app; AnchorDatabase = Join-Path $anchorRoot 'anchor.sqlite3'
        Secrets = $anchorSecrets; LogFile = Join-Path $anchorRoot 'anchor.log'; Listen = $anchorListen; Credential = $anchor.Credential
    }
    Start-CheckedBridgeService $anchorRegistration
    $anchorKey = [regex]::Match((Get-Content -Raw -LiteralPath $anchorRegistration.LogFile), 'independent anchor Ed25519 public key: ([A-Za-z0-9+/]{43}=)')
    if (-not $anchorKey.Success) { throw 'The isolated Anchor did not report its public key.' }
    @{
        public_base_url = "$gatewayUrl/bridge/"; gitea_public_url = "$gatewayUrl/"
        gitea_api_url = "$giteaUrl/"; client_id = $oauth.client_id
        database_path = Join-Path $bridgeRoot 'bridge.sqlite3'
        page_size = 5; max_pages = 2
        projects = @(
            @{ id='lab'; name='Lab'; repositories=@(@{ id=$repo.id; owner='labowner'; name='fixture' }) }
            @{ id='pilot'; name='Pilot'; repositories=@(@{ id=$pilot.id; owner='labowner'; name='pilot' }) }
        )
        receipts = @{
            artifact_root = $inbox; snapshot_root = $snapshots; issuers = @($issuer.id)
            anchor = @{ url = "$anchorUrl/v1/anchors"; public_key = $anchorKey.Groups[1].Value }
        }
    } | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $config -Encoding utf8NoBOM
    @{
        oauth_client_secret = $oauth.client_secret; anchor_bearer = $anchorBearer
        receipt_signing_seed = [Convert]::ToBase64String([Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
    } | ConvertTo-Json | Set-Content -LiteralPath $bridgeSecrets -Encoding utf8NoBOM

    $bridgeService = "BridgeLab$suffix"
    $bridgeRegistration = @{
        Name = $bridgeService; Executable = $app; Config = $config; Secrets = $bridgeSecrets
        LogFile = Join-Path $bridgeRoot 'bridge.log'; Listen = $bridgeListen; Credential = $bridge.Credential
    }
    Start-CheckedBridgeService $bridgeRegistration
    Assert-LabStatus "$bridgeUrl/bridge/" 200
    Assert-LabStatus "$bridgeUrl/bridge/api/projects" 401
    Assert-LabStatus "$anchorUrl/v1/anchors" 405
    Assert-LabAccess $bridge.Credential $bridgeSecrets $true
    Assert-LabAccess $bridge.Credential $anchorSecrets $false
    Assert-LabAccess $anchor.Credential $anchorSecrets $true
    Assert-LabAccess $anchor.Credential $bridgeSecrets $false
    Assert-LabAccess $bridge.Credential $config $false 'Write'
    Assert-LabAccess $bridge.Credential $artifact $true
    Assert-LabAccess $bridge.Credential $artifact $false 'Write'
    Assert-LabAccess $bridge.Credential (Join-Path $giteaRoot 'data/gitea.db') $false
    Assert-LabAccess $bridge.Credential $anchorRegistration.AnchorDatabase $false
    $listenPorts = @($bridgePort, $anchorPort, $giteaPort, $gatewayPort)
    $hostProbe = Join-Path $PSScriptRoot '../examples/Test-BridgeWindowsServices.ps1'
    $hostServices = @(
        @{ Name=$giteaService; Account=$gitea.Credential.UserName; Ports=$giteaPort; LoopbackOnly=$true }
        @{ Name=$bridgeService; Account=$bridge.Credential.UserName; Ports=$bridgePort; LoopbackOnly=$true; DelayedAutoStart=$true }
        @{ Name=$caddyService; Account=$caddy.Credential.UserName; Ports=$gatewayPort; LoopbackOnly=$true }
        @{ Name=$anchorService; Account=$anchor.Credential.UserName; Ports=$anchorPort; LoopbackOnly=$true; DelayedAutoStart=$true }
    )
    $hostReport = & $hostProbe -Services $hostServices
    if ($LASTEXITCODE -ne 0 -or -not ($hostReport | ConvertFrom-Json).passed) {
        $hostReport | Write-Host
        throw 'The Windows host probe rejected the four configured services.'
    }
    $baseline = Join-Path $clientRoot 'host-before.json'
    Set-Content -LiteralPath $baseline -Value $hostReport -Encoding utf8NoBOM
    $wrongServices = @($hostServices | ForEach-Object { $_.Clone() })
    $wrongServices[0].Account = $caddy.Credential.UserName
    $wrongServices[1].Ports = $anchorPort
    Set-Service -Name $caddyService -StartupType Manual
    try {
        $deniedReport = & $hostProbe -Services $wrongServices -BeforeRestart $baseline
        $deniedExitCode = $LASTEXITCODE
    } finally {
        Set-Service -Name $caddyService -StartupType Automatic
    }
    $denied = $deniedReport | ConvertFrom-Json
    if ($deniedExitCode -ne 1 -or $denied.passed -or $denied.restart.passed -or
        $denied.services[0].account_matches -or $denied.services[1].ports[0].passed -or $denied.services[2].automatic_start) {
        $deniedReport | Write-Host
        throw 'The Windows host probe did not reject an unchanged boot, incorrect account, unowned port and manual startup.'
    }
    Write-Host 'Windows host probe verified real service accounts, process ownership and automatic startup settings; it rejected an unchanged boot, incorrect account, unowned port and manual startup.'

    $current = Get-LabService $bridgeService
    $process = Get-CimInstance Win32_Process -Filter "ProcessId = $($current.ProcessId)"
    if ($process.ExecutablePath -ne $app) { throw 'SCM returned an unexpected process for the crash fixture.' }
    Stop-Process -Id $current.ProcessId -Force
    $recovered = Wait-LabService $bridgeService $current.ProcessId
    Assert-LabStatus "$bridgeUrl/bridge/" 200
    Write-Host "SCM restarted Bridge after terminating the fixture process: $($current.ProcessId) -> $($recovered.ProcessId)."

    $env:BRIDGE_TEST_SCM_STACK = Join-Path $clientRoot 'client fixture.json'
    $caPath = Join-Path $caddyRoot 'storage/pki/authorities/local/root.crt'
    $clientFixture = @{
        gateway = $gatewayUrl; ca = $caPath
        repository_id = $repo.id; password = $reader.Password; anchor_public_key = $anchorKey.Groups[1].Value
        anchor_url = "$anchorUrl/v1/anchors"; anchor_bearer = $anchorBearer
        gitea_api = $giteaUrl; owner_token = $owner.Token; owner_password = $owner.Password
        developer_token = $developer.Token; developer_password = $developer.Password; pilot_id = $pilot.id
    }
    $clientFixture | ConvertTo-Json | Set-Content -LiteralPath $env:BRIDGE_TEST_SCM_STACK -Encoding utf8NoBOM
    & go test -v -timeout 2m -run '^TestSCMHostedStack$' -count=1 .
    if ($LASTEXITCODE -ne 0) { throw 'The SCM-hosted stack did not pass HTTPS acceptance.' }
    & go test -v -timeout 2m -run '^TestSCMServicePullMirror$' -count=1 .
    if ($LASTEXITCODE -ne 0) { throw 'The SCM-hosted Gitea mirror did not pass the outbound and read-only checks.' }

    $startOrder = @($giteaService, $anchorService, $bridgeService, $caddyService)
    $caHash = (Get-FileHash -LiteralPath $caPath).Hash
    $originalProcesses = @{}
    foreach ($serviceName in $startOrder) {
        $originalProcesses[$serviceName] = (Wait-LabService $serviceName).ProcessId
    }
    Stop-LabStack $startOrder $listenPorts
    foreach ($serviceName in $startOrder) {
        Start-Service -Name $serviceName
        $null = Wait-LabService $serviceName $originalProcesses[$serviceName]
        if ($serviceName -eq $giteaService) { Wait-LabGitea $giteaUrl $giteaLog }
    }
    if ((Get-FileHash -LiteralPath $caPath).Hash -ne $caHash) { throw 'Caddy CA changed after the SCM restart.' }
    Write-Host 'All four services started again with new processes and the original Caddy CA.'
    & go test -v -timeout 2m -run '^TestSCMHostedStackAfterRestart$' -count=1 .
    if ($LASTEXITCODE -ne 0) { throw 'The restarted SCM stack did not retain its original data.' }

    $restoreListeners = @((New-LabListener), (New-LabListener), (New-LabListener))
    try {
        $restoredGiteaPort, $restoredBridgePort, $restoredAnchorPort = $restoreListeners | ForEach-Object { $_.LocalEndpoint.Port }
    } finally {
        $restoreListeners | ForEach-Object { $_.Stop() }
    }
    Stop-LabStack $startOrder $listenPorts

    $giteaDatabase = Join-Path $giteaRoot 'data/gitea.db'
    foreach ($sidecar in '-wal','-shm','-journal') {
        if (Test-Path -LiteralPath ($giteaDatabase + $sidecar)) { throw 'Gitea left SQLite sidecars after shutdown.' }
    }
    $bridgeDatabase = Join-Path $bridgeRoot 'bridge.sqlite3'
    $sourcePaths = @($giteaDatabase, $bridgeDatabase, $anchorRegistration.AnchorDatabase, $caPath)
    $originalHashes = Get-FileHash -LiteralPath $sourcePaths

    $backupRoot = Join-Path $root 'backup'
    $restoreRoot = Join-Path $root 'restored'
    New-Item -ItemType Directory -Path $backupRoot,$restoreRoot | Out-Null
    Set-LabDirectoryAcl $backupRoot @{}
    $parts = @(
        @{ Name='gitea'; Source=$giteaRoot; Files=@('app.ini','repos','data','custom'); Permissions=@{ $giteaSID = 'Modify' } }
        @{ Name='caddy'; Source=$caddyRoot; Files=@('Caddyfile','storage'); Permissions=@{ $caddySID = 'Modify' } }
        @{ Name='bridge-config'; Source=$bridgeSettings; Files=@('bridge config.json','bridge secrets.json'); Permissions=@{ $bridgeSID = 'ReadAndExecute' } }
        @{ Name='anchor-config'; Source=$anchorSettings; Files=@('anchor secrets.json'); Permissions=@{ $anchorSID = 'ReadAndExecute' } }
        @{ Name='inbox'; Source=$inbox; Files=@([string]$repo.id); Permissions=@{ $bridgeSID = 'ReadAndExecute' } }
        @{ Name='bridge'; Permissions=@{ $bridgeSID = 'Modify' } }
        @{ Name='anchor'; Permissions=@{ $anchorSID = 'Modify' } }
    )
    foreach ($part in $parts) {
        $backupPart = Join-Path $backupRoot $part.Name
        $restoredPart = Join-Path $restoreRoot $part.Name
        New-Item -ItemType Directory -Path $backupPart,$restoredPart | Out-Null
        Set-LabDirectoryAcl $restoredPart $part.Permissions
        if ($part.Files) {
            Copy-LabFiles $part.Source $backupPart $part.Files
            Copy-LabFiles $backupPart $restoredPart $part.Files
        }
    }

    $copy = Join-Path $PSScriptRoot '../examples/Copy-StoppedDatabase.ps1'
    & $copy -Database $bridgeDatabase -Snapshots $snapshots -Destination (Join-Path $backupRoot 'bridge')
    & $copy -Database $anchorRegistration.AnchorDatabase -Destination (Join-Path $backupRoot 'anchor')
    & $copy -Database (Join-Path $backupRoot 'bridge/state.sqlite3') -Snapshots (Join-Path $backupRoot 'bridge/snapshots') -Destination (Join-Path $restoreRoot 'bridge')
    & $copy -Database (Join-Path $backupRoot 'anchor/state.sqlite3') -Destination (Join-Path $restoreRoot 'anchor')

    $restoredBridgeDatabase = Join-Path $restoreRoot 'bridge/state.sqlite3'
    $restoredSnapshots = Join-Path $restoreRoot 'bridge/snapshots'
    $restoredAnchorDatabase = Join-Path $restoreRoot 'anchor/state.sqlite3'
    $evidence = Get-Content -Raw -LiteralPath (Join-Path $clientRoot 'restart-evidence.json') | ConvertFrom-Json
    $report = & $app -audit-restored-db $restoredBridgeDatabase -audit-snapshots $restoredSnapshots `
        -trusted-receipt-key $evidence.Receipt.IssuerKey -trusted-anchor-key $anchorKey.Groups[1].Value -audit-anchor-db $restoredAnchorDatabase
    if ($LASTEXITCODE -ne 0) { throw 'The offline audit could not verify the restored copies.' }
    $audit = $report | ConvertFrom-Json
    if ($audit.topics -ne 1 -or $audit.receipts -ne 1 -or $audit.artifacts -ne 1 -or
        $audit.verified_anchors -ne 1 -or $audit.pending_anchors -ne 0 -or $audit.unanchored_receipts -ne 0) {
        throw "Unexpected restored audit counts: $report"
    }
    Write-Host "Restored database, snapshot and independent ledger passed the offline audit: $report"

    $restoredGiteaRoot = Join-Path $restoreRoot 'gitea'
    $restoredGiteaIni = Join-Path $restoredGiteaRoot 'app.ini'
    $restoredGiteaUrl = "http://127.0.0.1:$restoredGiteaPort"
    (Get-Content -Raw -LiteralPath $restoredGiteaIni).Replace($giteaPath, $restoredGiteaRoot.Replace('\','/')).
        Replace("HTTP_PORT = $giteaPort", "HTTP_PORT = $restoredGiteaPort").Replace($giteaUrl, $restoredGiteaUrl) |
        Set-Content -LiteralPath $restoredGiteaIni -Encoding utf8NoBOM
    & $giteaApp --work-path $restoredGiteaRoot --config $restoredGiteaIni admin regenerate hooks
    if ($LASTEXITCODE -ne 0) { throw 'Could not regenerate Git hooks for the restored Gitea.' }

    $restoredGiteaService = "GiteaRestore$suffix"
    Start-LabUpstream $restoredGiteaService ('"{0}" --work-path "{1}" --config "{2}" web' -f $giteaApp,$restoredGiteaRoot,$restoredGiteaIni) $gitea.Credential
    Wait-LabGitea $restoredGiteaUrl (Join-Path $restoredGiteaRoot 'log/gitea.log')
    $restoredAnchorUrl = "http://127.0.0.1:$restoredAnchorPort/v1/anchors"
    $restoredAnchor = @{
        Name = "AnchorRestore$suffix"; Executable = $app; AnchorDatabase = $restoredAnchorDatabase
        Secrets = Join-Path $restoreRoot 'anchor-config/anchor secrets.json'; LogFile = Join-Path $restoreRoot 'anchor/anchor.log'
        Listen = "127.0.0.1:$restoredAnchorPort"; Credential = $anchor.Credential
    }
    Start-CheckedBridgeService $restoredAnchor
    $restoredBridge = @{
        Name = "BridgeRestore$suffix"; Executable = $app; Config = Join-Path $restoreRoot 'bridge-config/bridge config.json'
        Secrets = Join-Path $restoreRoot 'bridge-config/bridge secrets.json'; LogFile = Join-Path $restoreRoot 'bridge/bridge.log'
        Listen = "127.0.0.1:$restoredBridgePort"; Credential = $bridge.Credential
    }
    $settings = Get-Content -Raw -LiteralPath $restoredBridge.Config | ConvertFrom-Json
    $settings.gitea_api_url = "$restoredGiteaUrl/"
    $settings.database_path = $restoredBridgeDatabase
    $settings.receipts.artifact_root = Join-Path $restoreRoot 'inbox'
    $settings.receipts.snapshot_root = $restoredSnapshots
    $settings.receipts.anchor.url = $restoredAnchorUrl
    $settings | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $restoredBridge.Config -Encoding utf8NoBOM
    Start-CheckedBridgeService $restoredBridge
    Assert-LabAccess $bridge.Credential $restoredAnchorDatabase $false
    Assert-LabAccess $bridge.Credential (Join-Path $restoreRoot 'gitea/data/gitea.db') $false
    $restoredArtifact = Join-Path $settings.receipts.artifact_root (Join-Path ([string]$repo.id) 'artifact.txt')
    Assert-LabAccess $bridge.Credential $restoredArtifact $true
    Assert-LabAccess $bridge.Credential $restoredArtifact $false 'Write'

    $restoredCaddyRoot = Join-Path $restoreRoot 'caddy'
    $restoredCaddyConfig = Join-Path $restoredCaddyRoot 'Caddyfile'
    (Get-Content -Raw -LiteralPath $restoredCaddyConfig).Replace($caddyPath, $restoredCaddyRoot.Replace('\','/')).
        Replace("127.0.0.1:$giteaPort", "127.0.0.1:$restoredGiteaPort").Replace($bridgeListen, $restoredBridge.Listen) |
        Set-Content -LiteralPath $restoredCaddyConfig -Encoding utf8NoBOM
    $restoredCaddyService = "CaddyRestore$suffix"
    Start-LabUpstream $restoredCaddyService ('"{0}" run --config "{1}" --adapter caddyfile' -f $caddyApp,$restoredCaddyConfig) $caddy.Credential
    $clientFixture.ca = Join-Path $restoredCaddyRoot 'storage/pki/authorities/local/root.crt'
    if ((Get-FileHash -LiteralPath $clientFixture.ca).Hash -ne $caHash) { throw 'The restored gateway CA differs from the original.' }
    $clientFixture.gitea_api = $restoredGiteaUrl
    $clientFixture.anchor_url = $restoredAnchorUrl
    $env:BRIDGE_TEST_SCM_STACK = Join-Path $clientRoot 'restored client fixture.json'
    $clientFixture | ConvertTo-Json | Set-Content -LiteralPath $env:BRIDGE_TEST_SCM_STACK -Encoding utf8NoBOM
    & go test -v -timeout 2m -run '^TestSCMHostedStackAfterRestore$' -count=1 .
    if ($LASTEXITCODE -ne 0) { throw 'The restored SCM stack did not pass end-to-end acceptance.' }

    $restoreStartOrder = @($restoredGiteaService, $restoredAnchor.Name, $restoredBridge.Name, $restoredCaddyService)
    Stop-LabStack $restoreStartOrder @($restoredGiteaPort, $restoredBridgePort, $restoredAnchorPort, $gatewayPort)
    foreach ($original in $originalHashes) {
        if ((Get-FileHash -LiteralPath $original.Path).Hash -ne $original.Hash) {
            throw "The cold restore modified the source file $($original.Path)."
        }
    }
    Write-Host 'Restored all four services from backup into new directories; the stopped source databases and CA were unchanged.'
} finally {
    foreach ($serviceName in $services) {
        Stop-Service -Name $serviceName -ErrorAction SilentlyContinue
        & $sc delete $serviceName | Out-Null
    }
    foreach ($userName in $accounts) {
        Remove-LocalUser -Name $userName -ErrorAction SilentlyContinue
    }
}
