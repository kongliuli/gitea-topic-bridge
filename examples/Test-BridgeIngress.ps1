#Requires -Version 7.4
[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [uri]$BridgeUrl,
    [Parameter(Mandatory)]
    [ValidateSet('Allowed', 'Denied')]
    [string]$ExpectedAccess,
    [Parameter(Mandatory)]
    [ValidateNotNullOrEmpty()]
    [string]$SourceLabel,
    [Parameter(Mandatory)]
    [ipaddress[]]$BackendAddress,
    [ValidateRange(1, 65535)]
    [int[]]$BackendPorts = @(3000, 3001),
    [ipaddress[]]$IngressAddress,
    [string]$TrustedCAFile,
    [ValidateRange(1, 30)]
    [int]$TimeoutSeconds = 3
)

$ErrorActionPreference = 'Stop'
if (-not $BridgeUrl.IsAbsoluteUri -or $BridgeUrl.Scheme -ne 'https' -or $BridgeUrl.AbsolutePath -cne '/bridge/' -or
    $BridgeUrl.UserInfo -or $BridgeUrl.Query -or $BridgeUrl.Fragment) {
    throw 'Use the canonical HTTPS URL ending in /bridge/, with no credential, query or fragment.'
}
$denied = $ExpectedAccess -eq 'Denied'
if ($denied -and -not $IngressAddress) { throw 'Supply the previously verified ingress IP addresses for the denied client.' }
if ($denied -and $TrustedCAFile) { throw 'The denied-client check uses TCP and does not take a CA file.' }
if (-not $denied -and $IngressAddress) { throw 'The allowed-client check uses the canonical URL and client DNS.' }
if (-not $BackendAddress -or -not $BackendPorts) { throw 'Supply the backend addresses and ports to check.' }

function Test-ClosedTcpPort([ipaddress]$Address, [int]$Port) {
    $client = [System.Net.Sockets.TcpClient]::new($Address.AddressFamily)
    $timeout = [System.Threading.CancellationTokenSource]::new([timespan]::FromSeconds($TimeoutSeconds))
    try {
        [void]$client.ConnectAsync($Address, $Port, $timeout.Token).AsTask().GetAwaiter().GetResult()
        return 'connected'
    } catch {
        $exception = $_.Exception.GetBaseException()
        if ($exception -is [OperationCanceledException]) { return 'timeout' }
        if ($exception -is [System.Net.Sockets.SocketException]) {
            switch ($exception.SocketErrorCode) {
                ConnectionRefused  { return 'refused' }
                TimedOut           { return 'timeout' }
                HostUnreachable    { return 'host_unreachable' }
                NetworkUnreachable { return 'network_unreachable' }
                AccessDenied       { return 'access_denied' }
            }
        }
        return 'probe_error'
    } finally {
        $timeout.Dispose()
        $client.Dispose()
    }
}

function New-TcpCheck([string]$Name, [ipaddress]$Address, [int]$Port) {
    $outcome = Test-ClosedTcpPort $Address $Port
    return [pscustomobject]@{
        check = $Name
        address = $Address.ToString()
        port = $Port
        result = $outcome
        passed = $outcome -ne 'connected' -and $outcome -ne 'probe_error'
    }
}

$checks = [System.Collections.Generic.List[object]]::new()
if ($denied) {
    foreach ($address in $IngressAddress) {
        $checks.Add((New-TcpCheck 'ingress' $address $BridgeUrl.Port))
    }
} else {
    $handler = [System.Net.Http.SocketsHttpHandler]::new()
    $handler.AllowAutoRedirect = $false
    $handler.UseCookies = $false
    $handler.UseProxy = $false
    $client = [System.Net.Http.HttpClient]::new($handler)
    $client.Timeout = [timespan]::FromSeconds($TimeoutSeconds)
    try {
        if ($TrustedCAFile) {
            $certificates = [System.Security.Cryptography.X509Certificates.X509Certificate2Collection]::new()
            $certificates.ImportFromPemFile((Resolve-Path -LiteralPath $TrustedCAFile).ProviderPath)
            if ($certificates.Count -eq 0) { throw 'The supplied PEM file contains no trusted certificates.' }
            $policy = [System.Security.Cryptography.X509Certificates.X509ChainPolicy]::new()
            $policy.TrustMode = [System.Security.Cryptography.X509Certificates.X509ChainTrustMode]::CustomRootTrust
            $policy.CustomTrustStore.AddRange($certificates)
            $policy.RevocationMode = [System.Security.Cryptography.X509Certificates.X509RevocationMode]::NoCheck
            $handler.SslOptions.CertificateChainPolicy = $policy
        }
        $paths = @(
            @{ path='/bridge/'; method='GET'; status=200; name='bridge_page' }
            @{ path='/bridge/api/projects'; method='GET'; status=401; name='anonymous_api' }
            @{ path='/internal/hooks/gitea'; method='POST'; status=404; name='internal_webhook' }
        )
        foreach ($probe in $paths) {
            $uri = [uri]::new($BridgeUrl, $probe.path)
            $method = [System.Net.Http.HttpMethod]::new($probe.method)
            $request = [System.Net.Http.HttpRequestMessage]::new($method, $uri)
            $status = $null
            $failure = $null
            try {
                $response = $client.SendAsync($request, [System.Net.Http.HttpCompletionOption]::ResponseHeadersRead).GetAwaiter().GetResult()
                try { $status = [int]$response.StatusCode } finally { $response.Dispose() }
            } catch {
                $failure = $_.Exception.GetBaseException().GetType().Name
            } finally {
                $request.Dispose()
            }
            $checks.Add([pscustomobject]@{
                check = $probe.name
                url = $uri.AbsoluteUri
                method = $probe.method
                expected_status = $probe.status
                status = $status
                error_type = $failure
                passed = $status -eq $probe.status
            })
        }
    } finally {
        $client.Dispose()
    }
}
foreach ($address in $BackendAddress) {
    foreach ($port in $BackendPorts) {
        $checks.Add((New-TcpCheck 'backend' $address $port))
    }
}
$passed = @($checks.Where({ -not $_.passed })).Count -eq 0
$trustSource = if ($denied) { $null } elseif ($TrustedCAFile) { 'custom_ca' } else { 'system' }
[pscustomobject]@{
    observed_at = [DateTimeOffset]::UtcNow.ToString('o')
    source = $SourceLabel
    public_url = $BridgeUrl.AbsoluteUri
    expected_access = $ExpectedAccess.ToLowerInvariant()
    tls_trust = $trustSource
    passed = $passed
    checks = $checks.ToArray()
} | ConvertTo-Json -Depth 4
if (-not $passed) { exit 1 }
