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

function Assert-LabStatus([string]$Url, [int]$Expected) {
    $response = Invoke-WebRequest -Uri $Url -NoProxy -SkipHttpErrorCheck -TimeoutSec 5
    if ([int]$response.StatusCode -ne $Expected) { throw "Expected HTTP $Expected at $Url, got $($response.StatusCode)." }
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
    New-Service -Name $Name -BinaryPathName $BinaryPath -Credential $Credential -StartupType Manual | Out-Null
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
    Start-LabUpstream "GiteaLab$suffix" ('"{0}" --work-path "{1}" --config "{2}" web' -f $giteaApp,$giteaRoot,$giteaIni) $gitea.Credential
    Wait-LabGitea $giteaUrl $giteaLog
    $owner = New-GiteaLabUser $giteaApp $giteaFlags 'labowner' 'all'
    $reader = New-GiteaLabUser $giteaApp $giteaFlags 'labreader' 'read:repository,read:user'
    $api = @{
        Headers = @{ Authorization = "Bearer $($owner.Token)" }; ContentType = 'application/json'
        TimeoutSec = 15; NoProxy = $true
    }
    $repo = Invoke-RestMethod @api -Method Post -Uri "$giteaUrl/api/v1/user/repos" -Body '{"name":"fixture","private":true,"auto_init":true,"default_branch":"main"}'
    $null = Invoke-RestMethod @api -Method Put -Uri "$giteaUrl/api/v1/repos/labowner/fixture/collaborators/labreader" -Body '{"permission":"read"}'
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
    Start-LabUpstream "CaddyLab$suffix" ('"{0}" run --config "{1}" --adapter caddyfile' -f $caddyApp,$caddyConfig) $caddy.Credential

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
        projects = @(@{ id='lab'; name='Lab'; repositories=@(@{ id=$repo.id; owner='labowner'; name='fixture' }) })
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
    foreach ($port in @($bridgePort, $anchorPort, $giteaPort, $gatewayPort)) {
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

    $env:BRIDGE_TEST_SCM_STACK = Join-Path $clientRoot 'client fixture.json'
    @{
        gateway = $gatewayUrl; ca = Join-Path $caddyRoot 'storage/pki/authorities/local/root.crt'
        repository_id = $repo.id; password = $reader.Password; anchor_public_key = $anchorKey.Groups[1].Value
    } | ConvertTo-Json | Set-Content -LiteralPath $env:BRIDGE_TEST_SCM_STACK -Encoding utf8NoBOM
    & go test -v -timeout 2m -run '^TestSCMHostedStack$' -count=1 .
    if ($LASTEXITCODE -ne 0) { throw 'The SCM-hosted stack did not pass HTTPS acceptance.' }

    foreach ($serviceName in $services) {
        Stop-Service -Name $serviceName
        (Get-Service -Name $serviceName).WaitForStatus('Stopped', [timespan]::FromSeconds(20))
    }
    Write-Host 'All four services accepted SCM stop.'
} finally {
    foreach ($serviceName in $services) {
        Stop-Service -Name $serviceName -ErrorAction SilentlyContinue
        & $sc delete $serviceName | Out-Null
    }
    foreach ($userName in $accounts) {
        Remove-LocalUser -Name $userName -ErrorAction SilentlyContinue
    }
}
