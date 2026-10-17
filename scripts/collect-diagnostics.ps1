[CmdletBinding()]
param(
    [string]$BinaryPath = "$env:ProgramFiles\DebtFlow\SipGateway\sip-gateway.exe",
    [string]$ConfigPath = "$env:ProgramData\DebtFlow\SipGateway\config.yaml",
    [string]$ServiceName = "DebtFlowSipGateway",
    [string]$OutputPath = (Join-Path $PWD "sip-gateway-diagnostics.zip")
)

$ErrorActionPreference = "Stop"
$temporaryDirectory = Join-Path ([IO.Path]::GetTempPath()) ("debt-flow-sip-gateway-" + [guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $temporaryDirectory | Out-Null

try {
    $reportPath = Join-Path $temporaryDirectory "gateway.json"
    & $BinaryPath --config $ConfigPath --write-diagnostics $reportPath
    if ($LASTEXITCODE -ne 0) { throw "gateway diagnostics command failed" }

    $service = Get-Service -Name $ServiceName -ErrorAction SilentlyContinue
    [ordered]@{
        found = $null -ne $service
        name = if ($service) { $service.Name } else { $ServiceName }
        status = if ($service) { $service.Status.ToString() } else { "not_found" }
        startType = if ($service) { $service.StartType.ToString() } else { "unknown" }
    } | ConvertTo-Json | Set-Content -Encoding utf8 (Join-Path $temporaryDirectory "service.json")

    $probes = [ordered]@{}
    foreach ($probe in @("healthz", "readyz")) {
        try {
            $response = Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:8080/$probe" -TimeoutSec 3
            $probes[$probe] = [ordered]@{ reachable = $true; statusCode = [int]$response.StatusCode }
        } catch {
            $statusCode = 0
            if ($_.Exception.Response) { $statusCode = [int]$_.Exception.Response.StatusCode }
            $probes[$probe] = [ordered]@{ reachable = $statusCode -ne 0; statusCode = $statusCode }
        }
    }
    $probes | ConvertTo-Json -Depth 3 | Set-Content -Encoding utf8 (Join-Path $temporaryDirectory "probes.json")

    try {
        $metrics = (Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:9090/metrics" -TimeoutSec 3).Content
        $safeMetrics = $metrics -split "`n" | Where-Object {
            $_ -match '^(# (HELP|TYPE) )?(debt_flow_sip_gateway_|go_|process_)'
        }
        $safeMetrics | Set-Content -Encoding utf8 (Join-Path $temporaryDirectory "metrics.prom")
    } catch {
        "# metrics endpoint unavailable" | Set-Content -Encoding utf8 (Join-Path $temporaryDirectory "metrics.prom")
    }

    $forbidden = '(?i)(password\s*[:=]|authorization\s*[:=]|digest[ _-]*response|-----BEGIN [A-Z ]*PRIVATE KEY-----|sessionCapability|callCapability|plaintextKeys|eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.)'
    foreach ($file in Get-ChildItem -Path $temporaryDirectory -File) {
        if (Select-String -Path $file.FullName -Pattern $forbidden -Quiet) {
            throw "secret scan rejected diagnostics file $($file.Name)"
        }
    }

    if (Test-Path $OutputPath) { throw "diagnostics output already exists: $OutputPath" }
    Compress-Archive -Path (Join-Path $temporaryDirectory "*") -DestinationPath $OutputPath
    Write-Output "Sanitized diagnostics bundle: $OutputPath"
} finally {
    Remove-Item -Path $temporaryDirectory -Recurse -Force -ErrorAction SilentlyContinue
}
