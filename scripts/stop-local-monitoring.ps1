[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [string]$RuntimeDirectory = "$env:ProgramData\DebtFlow\SipGateway\monitoring"
)

$ErrorActionPreference = "Stop"
$processFile = Join-Path $RuntimeDirectory "processes.json"
if (-not (Test-Path -LiteralPath $processFile -PathType Leaf)) {
    throw "monitoring process file not found: $processFile"
}
$allowedNames = @("prometheus", "alertmanager", "grafana-server")
foreach ($entry in @(Get-Content -LiteralPath $processFile -Raw | ConvertFrom-Json)) {
    $process = Get-Process -Id $entry.Id -ErrorAction SilentlyContinue
    if ($null -eq $process -or $allowedNames -notcontains $process.ProcessName) { continue }
    if ($PSCmdlet.ShouldProcess("$($process.ProcessName) ($($process.Id))", "Stop local monitoring process")) {
        Stop-Process -Id $process.Id -ErrorAction Stop
    }
}
Remove-Item -LiteralPath $processFile -Force
