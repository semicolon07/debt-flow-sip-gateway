[CmdletBinding()]
param(
    [string]$ProcessName = "sip-gateway",
    [string]$MetricsUri = "http://127.0.0.1:9090/metrics",
    [int]$DurationMinutes = 120,
    [int]$SampleSeconds = 5,
    [string]$OutputDirectory = (Join-Path $PWD "capacity-evidence")
)

$ErrorActionPreference = "Stop"
if ($DurationMinutes -lt 1 -or $SampleSeconds -lt 1) { throw "duration and sample interval must be positive" }
$OutputDirectory = [IO.Path]::GetFullPath($OutputDirectory)
New-Item -ItemType Directory -Path $OutputDirectory -Force | Out-Null
$samples = [System.Collections.Generic.List[object]]::new()
$logicalProcessors = [Environment]::ProcessorCount
$deadline = [DateTime]::UtcNow.AddMinutes($DurationMinutes)
$previousCpu = $null
$previousAt = $null

function Read-Metric {
    param([string]$Text, [string]$Name)
    $match = [regex]::Match($Text, "(?m)^$([regex]::Escape($Name))(?:\{[^}]*\})?\s+([-+0-9.eE]+)$")
    if (-not $match.Success) { return $null }
    return [double]::Parse($match.Groups[1].Value, [Globalization.CultureInfo]::InvariantCulture)
}

function Read-MetricSum {
    param([string]$Text, [string]$Name)
    $matches = [regex]::Matches($Text, "(?m)^$([regex]::Escape($Name))(?:\{[^}]*\})?\s+([-+0-9.eE]+)$")
    if ($matches.Count -eq 0) { return $null }
    $sum = 0.0
    foreach ($match in $matches) {
        $sum += [double]::Parse($match.Groups[1].Value, [Globalization.CultureInfo]::InvariantCulture)
    }
    return $sum
}

while ([DateTime]::UtcNow -lt $deadline) {
    $sampledAt = [DateTime]::UtcNow
    $process = Get-Process -Name $ProcessName -ErrorAction Stop | Select-Object -First 1
    $cpuPercent = $null
    if ($null -ne $previousCpu) {
        $elapsed = ($sampledAt - $previousAt).TotalSeconds
        if ($elapsed -gt 0) {
            $cpuPercent = [math]::Round((($process.CPU - $previousCpu) / $elapsed / $logicalProcessors) * 100, 3)
        }
    }
    $previousCpu = $process.CPU
    $previousAt = $sampledAt
    try {
        $metrics = (Invoke-WebRequest -Uri $MetricsUri -UseBasicParsing -TimeoutSec 5).Content
    } catch {
        $metrics = ""
    }
    $tcpCount = @(Get-NetTCPConnection -OwningProcess $process.Id -ErrorAction SilentlyContinue).Count
    $udpCount = @(Get-NetUDPEndpoint -OwningProcess $process.Id -ErrorAction SilentlyContinue).Count
    $samples.Add([pscustomobject]@{
        sampledAtUtc = $sampledAt.ToString("o")
        cpuPercent = $cpuPercent
        workingSetBytes = $process.WorkingSet64
        privateMemoryBytes = $process.PrivateMemorySize64
        handleCount = $process.HandleCount
        threadCount = $process.Threads.Count
        tcpConnections = $tcpCount
        udpEndpoints = $udpCount
        goGoroutines = Read-Metric $metrics "go_goroutines"
        goHeapBytes = Read-Metric $metrics "go_memstats_heap_alloc_bytes"
        sessions = Read-Metric $metrics "debt_flow_sip_gateway_sessions"
        connectedSessions = Read-Metric $metrics "debt_flow_sip_gateway_connected_sessions"
        registrations = Read-MetricSum $metrics "debt_flow_sip_gateway_registrations"
        calls = Read-MetricSum $metrics "debt_flow_sip_gateway_calls"
        peerConnections = Read-Metric $metrics "debt_flow_sip_gateway_peer_connections"
        turnAllocations = Read-Metric $metrics "debt_flow_sip_gateway_turn_allocations"
        outboxEvents = Read-Metric $metrics "debt_flow_sip_gateway_outbox_pending_events"
        outboxCapacityRatio = Read-Metric $metrics "debt_flow_sip_gateway_outbox_capacity_ratio"
    })
    Start-Sleep -Seconds $SampleSeconds
}

$csv = Join-Path $OutputDirectory "capacity-samples.csv"
$samples | Export-Csv -LiteralPath $csv -NoTypeInformation -Encoding utf8
$summary = [ordered]@{
    schemaVersion = 1
    generatedAtUtc = [DateTime]::UtcNow.ToString("o")
    durationMinutes = $DurationMinutes
    sampleSeconds = $SampleSeconds
    sampleCount = $samples.Count
    maxCpuPercent = ($samples.cpuPercent | Measure-Object -Maximum).Maximum
    maxWorkingSetBytes = ($samples.workingSetBytes | Measure-Object -Maximum).Maximum
    maxPrivateMemoryBytes = ($samples.privateMemoryBytes | Measure-Object -Maximum).Maximum
    maxGoroutines = ($samples.goGoroutines | Measure-Object -Maximum).Maximum
    maxGoHeapBytes = ($samples.goHeapBytes | Measure-Object -Maximum).Maximum
    maxTcpConnections = ($samples.tcpConnections | Measure-Object -Maximum).Maximum
    maxUdpEndpoints = ($samples.udpEndpoints | Measure-Object -Maximum).Maximum
    maxSessions = ($samples.sessions | Measure-Object -Maximum).Maximum
    maxConnectedSessions = ($samples.connectedSessions | Measure-Object -Maximum).Maximum
    maxRegistrations = ($samples.registrations | Measure-Object -Maximum).Maximum
    maxCalls = ($samples.calls | Measure-Object -Maximum).Maximum
    maxPeerConnections = ($samples.peerConnections | Measure-Object -Maximum).Maximum
    maxTurnAllocations = ($samples.turnAllocations | Measure-Object -Maximum).Maximum
    maxOutboxEvents = ($samples.outboxEvents | Measure-Object -Maximum).Maximum
    maxOutboxCapacityRatio = ($samples.outboxCapacityRatio | Measure-Object -Maximum).Maximum
    finalWorkingSetBytes = $samples[-1].workingSetBytes
    finalPrivateMemoryBytes = $samples[-1].privateMemoryBytes
    finalGoHeapBytes = $samples[-1].goHeapBytes
    finalGoroutines = $samples[-1].goGoroutines
    finalTcpConnections = $samples[-1].tcpConnections
    finalUdpEndpoints = $samples[-1].udpEndpoints
}
$summary | ConvertTo-Json -Depth 4 | Set-Content -LiteralPath (Join-Path $OutputDirectory "capacity-summary.json") -Encoding utf8
Write-Host "Capacity evidence: $OutputDirectory"
