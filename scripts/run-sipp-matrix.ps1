[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)]
    [ValidateSet(
        "register-digest",
        "register-auth-failure",
        "unregister",
        "outbound-answer-bye",
        "outbound-answer-bye-pcma",
        "outbound-busy",
        "outbound-no-answer",
        "inbound-answer-bye",
        "inbound-answer-bye-pcma",
        "inbound-answer-bye-mixed-codecs",
        "inbound-cancel",
        "inbound-unsupported-codec",
        "inbound-malformed-sdp",
        "late-200-after-cancel"
    )]
    [string]$Scenario,
    [Parameter(Mandatory = $true)]
    [ValidateSet("udp", "tcp")]
    [string]$Transport,
    [Parameter(Mandatory = $true)]
    [string]$GatewayHost,
    [int]$GatewayPort = 5060,
    [Parameter(Mandatory = $true)]
    [string]$LocalIp,
    [int]$LocalPort = 15060,
    [string]$Username = "1001",
    [string]$EvidenceDirectory = (Join-Path $PWD "sipp-evidence")
)

$ErrorActionPreference = "Stop"
$sipp = Get-Command sipp -ErrorAction Stop
$repository = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$scenarioPath = Join-Path $repository "test\sipp\$Scenario.xml"
if (-not (Test-Path -LiteralPath $scenarioPath)) { throw "SIPp scenario not found: $scenarioPath" }

$timestamp = [DateTime]::UtcNow.ToString("yyyyMMddTHHmmssZ")
$runDirectory = Join-Path $EvidenceDirectory "$timestamp-$Scenario-$Transport"
New-Item -ItemType Directory -Path $runDirectory -Force | Out-Null
$transportArgument = if ($Transport -eq "udp") { "u1" } else { "t1" }
$stats = Join-Path $runDirectory "statistics.csv"
$target = "$GatewayHost`:$GatewayPort"

Push-Location $runDirectory
try {
    & $sipp.Source $target `
        -p $LocalPort -i $LocalIp -r 1 -m 1 -s $Username `
        -sf $scenarioPath -t $transportArgument -timeout_global 240 `
        -trace_msg -trace_err -trace_logs -trace_stat -stf $stats
    $exitCode = $LASTEXITCODE
} finally {
    Pop-Location
}

[ordered]@{
    schemaVersion = 1
    generatedAtUtc = [DateTime]::UtcNow.ToString("o")
    scenario = $Scenario
    transport = $Transport
    gatewayHost = $GatewayHost
    gatewayPort = $GatewayPort
    exitCode = $exitCode
    passed = $exitCode -eq 0
} | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $runDirectory "result.json") -Encoding utf8

Write-Host "SIPp evidence: $runDirectory"
if ($exitCode -ne 0) { exit $exitCode }
