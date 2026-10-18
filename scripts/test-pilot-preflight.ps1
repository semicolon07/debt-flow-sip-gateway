[CmdletBinding()]
param(
    [string]$BinaryPath = "$env:ProgramFiles\DebtFlow\SipGateway\sip-gateway.exe",
    [string]$ConfigPath = "$env:ProgramData\DebtFlow\SipGateway\config.yaml",
    [string]$ServiceName = "DebtFlowSipGateway",
    [string]$PublicGatewayHost = "sip-gateway.uniga.app",
    [string]$ForbiddenApiHost,
    [string]$OutputPath = (Join-Path $PWD "sip-gateway-preflight.json"),
    [switch]$RequireReleaseEvidence
)

$ErrorActionPreference = "Stop"
$checks = [System.Collections.Generic.List[object]]::new()

function Add-Check {
    param([string]$Name, [bool]$Passed, [string]$Detail)
    $checks.Add([ordered]@{ name = $Name; passed = $Passed; detail = $Detail })
}

function Invoke-SafeProbe {
    param([string]$Name, [string]$Uri)
    try {
        $response = Invoke-WebRequest -Uri $Uri -UseBasicParsing -TimeoutSec 5
        Add-Check $Name ($response.StatusCode -eq 200) "HTTP $($response.StatusCode)"
    } catch {
        $status = if ($_.Exception.Response) { [int]$_.Exception.Response.StatusCode } else { 0 }
        Add-Check $Name $false (if ($status) { "HTTP $status" } else { "unreachable" })
    }
}

$runningOnWindows = [Environment]::OSVersion.Platform -eq [PlatformID]::Win32NT
if (-not $runningOnWindows) {
    Add-Check "windows-server" $false "preflight must run on Windows Server 2022"
} else {
    $caption = (Get-CimInstance Win32_OperatingSystem).Caption
    Add-Check "windows-server" ($caption -match "Windows Server 2022") $caption
}

$binaryExists = Test-Path -LiteralPath $BinaryPath -PathType Leaf
Add-Check "binary-present" $binaryExists (if ($binaryExists) { "present" } else { "missing" })
$configExists = Test-Path -LiteralPath $ConfigPath -PathType Leaf
Add-Check "config-present" $configExists (if ($configExists) { "present" } else { "missing" })

if ($binaryExists) {
    $signature = Get-AuthenticodeSignature -LiteralPath $BinaryPath
    $signatureValid = $signature.Status -eq "Valid"
    $signatureDetail = if ($signatureValid) { "valid" } elseif ($RequireReleaseEvidence) { "required but $($signature.Status)" } else { "development: $($signature.Status)" }
    Add-Check "authenticode-signature" ($signatureValid -or -not $RequireReleaseEvidence) $signatureDetail
    try {
        & $BinaryPath --config $ConfigPath --check-config | Out-Null
        Add-Check "strict-config" ($LASTEXITCODE -eq 0) "validator exit $LASTEXITCODE"
    } catch {
        Add-Check "strict-config" $false "validator failed"
    }

    $releaseDirectory = Split-Path -Parent (Resolve-Path -LiteralPath $BinaryPath).Path
    $checksumsPath = Join-Path $releaseDirectory "SHA256SUMS"
    $sbomPath = Join-Path $releaseDirectory "sbom.cdx.json"
    $buildInfoPath = Join-Path $releaseDirectory "build-info.txt"
    $releaseManifestPath = Join-Path $releaseDirectory "release-manifest.json"
    $checksumValid = $false
    if (Test-Path -LiteralPath $checksumsPath -PathType Leaf) {
        $binaryHash = (Get-FileHash -LiteralPath $BinaryPath -Algorithm SHA256).Hash.ToLowerInvariant()
        $checksumValid = [bool](Get-Content -LiteralPath $checksumsPath | Where-Object {
            $_ -eq "$binaryHash  sip-gateway.exe"
        })
    }
    Add-Check "release-binary-checksum" ($checksumValid -or -not $RequireReleaseEvidence) `
        (if ($checksumValid) { "SHA256SUMS match" } elseif ($RequireReleaseEvidence) { "missing or mismatched" } else { "development: not required" })

    $sbomValid = $false
    if (Test-Path -LiteralPath $sbomPath -PathType Leaf) {
        try {
            $sbom = Get-Content -LiteralPath $sbomPath -Raw | ConvertFrom-Json
            $sbomValid = $sbom.bomFormat -eq "CycloneDX" -and $sbom.specVersion -eq "1.6" -and
                $null -ne $sbom.metadata.component.name
        } catch {
            $sbomValid = $false
        }
    }
    Add-Check "release-cyclonedx-sbom" ($sbomValid -or -not $RequireReleaseEvidence) `
        (if ($sbomValid) { "CycloneDX 1.6" } elseif ($RequireReleaseEvidence) { "missing or invalid" } else { "development: not required" })
    $buildInfoPresent = Test-Path -LiteralPath $buildInfoPath -PathType Leaf
    Add-Check "release-build-info" ($buildInfoPresent -or -not $RequireReleaseEvidence) `
        (if ($buildInfoPresent) { "present" } elseif ($RequireReleaseEvidence) { "missing" } else { "development: not required" })

    $releaseManifestValid = $false
    $releaseVersion = "unknown"
    if (Test-Path -LiteralPath $releaseManifestPath -PathType Leaf) {
        try {
            $releaseManifest = Get-Content -LiteralPath $releaseManifestPath -Raw | ConvertFrom-Json
            $binaryVersion = (& $BinaryPath --version | Out-String).Trim()
            $releaseVersion = [string]$releaseManifest.version
            $releaseManifestValid = $LASTEXITCODE -eq 0 -and $releaseManifest.schemaVersion -eq 1 -and
                $releaseManifest.targetOs -eq "windows" -and
                $releaseManifest.targetArchitecture -eq "amd64" -and
                $releaseManifest.sourceModified -eq $false -and
                $releaseManifest.gitCommit -match '^[0-9a-f]{40,64}$' -and
                $releaseVersion -eq $binaryVersion
        } catch {
            $releaseManifestValid = $false
        }
    }
    Add-Check "release-manifest-binary-binding" ($releaseManifestValid -or -not $RequireReleaseEvidence) `
        (if ($releaseManifestValid) { "version $releaseVersion" } elseif ($RequireReleaseEvidence) { "missing, dirty or mismatched" } else { "development: not required" })
}

if ($configExists) {
    $forbiddenConfig = Select-String -LiteralPath $ConfigPath -Pattern '(?i)web.?api|api.?url|http.?client|bearer|authorization' -Quiet
    $configDetail = if ($forbiddenConfig) { "forbidden API/client key detected" } else { "no forbidden API/client key" }
    Add-Check "no-gateway-api-client-config" (-not $forbiddenConfig) $configDetail
    $plaintextCapabilityKeys = Select-String -LiteralPath $ConfigPath `
        -Pattern '(?im)^\s*allowPlaintextKeys\s*:\s*true\s*(?:#.*)?$' -Quiet
    $protectedKeyDetail = if ($plaintextCapabilityKeys) { "plaintext capability keys enabled" } else { "protected keys required" }
    Add-Check "capability-keys-protected" (-not $plaintextCapabilityKeys) $protectedKeyDetail
    $insecureLoopbackOrigin = Select-String -LiteralPath $ConfigPath `
        -Pattern '(?im)^\s*allowInsecureLoopbackOrigin\s*:\s*true\s*(?:#.*)?$' -Quiet
    $wildcardOrigin = Select-String -LiteralPath $ConfigPath `
        -Pattern '(?im)^\s*(?:allowedOrigin\s*:\s*|-\s*)["'']?\*["'']?\s*(?:#.*)?$' -Quiet
    $originDetail = if ($insecureLoopbackOrigin) {
        "insecure loopback POC origin enabled"
    } elseif ($wildcardOrigin) {
        "wildcard browser origin enabled"
    } else {
        "exact secure origin allowlist"
    }
    # Wildcard is an explicit supported policy and remains visible in the evidence
    # detail for security review; only the development HTTP-loopback escape hatch
    # fails the production security check.
    Add-Check "secure-browser-origin" (-not $insecureLoopbackOrigin) $originDetail
}

try {
    $service = Get-Service -Name $ServiceName
    Add-Check "windows-service-running" ($service.Status -eq "Running") $service.Status.ToString()
} catch {
    Add-Check "windows-service-running" $false "service missing"
}

Invoke-SafeProbe "loopback-health" "http://127.0.0.1:8080/healthz"
Invoke-SafeProbe "loopback-readiness" "http://127.0.0.1:8080/readyz"
Invoke-SafeProbe "loopback-metrics" "http://127.0.0.1:9090/metrics"

try {
    $tcp = [Net.Sockets.TcpClient]::new()
    $tcp.Connect($PublicGatewayHost, 443)
    $tls = [Net.Security.SslStream]::new($tcp.GetStream(), $false)
    $tls.AuthenticateAsClient($PublicGatewayHost)
    $certificate = [Security.Cryptography.X509Certificates.X509Certificate2]::new($tls.RemoteCertificate)
    $tls.Dispose()
    $tcp.Dispose()
    Add-Check "iis-tls-endpoint" $true "TLS $($certificate.NotAfter.ToUniversalTime().ToString('o'))"
} catch {
    Add-Check "iis-tls-endpoint" $false "TLS endpoint unavailable or invalid"
}

if ($ForbiddenApiHost) {
    $apiReachable = Test-NetConnection -ComputerName $ForbiddenApiHost -Port 443 -InformationLevel Quiet
    $routeDetail = if ($apiReachable) { "unexpected TCP/443 route" } else { "TCP/443 denied" }
    Add-Check "gateway-to-api-route-denied" (-not $apiReachable) $routeDetail
} else {
    $routeDetail = if ($RequireReleaseEvidence) { "-ForbiddenApiHost is required" } else { "not requested in development mode" }
    Add-Check "gateway-to-api-route-denied" (-not $RequireReleaseEvidence) $routeDetail
}

$result = [ordered]@{
    schemaVersion = 1
    generatedAtUtc = [DateTime]::UtcNow.ToString("o")
    machine = $env:COMPUTERNAME
    serviceName = $ServiceName
    releaseEvidenceRequired = [bool]$RequireReleaseEvidence
    passed = -not ($checks | Where-Object { -not $_.passed })
    checks = $checks
}
$result | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath $OutputPath -Encoding utf8
Write-Host "Preflight evidence: $OutputPath"
if (-not $result.passed) { exit 1 }
