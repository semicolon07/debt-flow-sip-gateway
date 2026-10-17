[CmdletBinding(SupportsShouldProcess = $true)]
param([string]$ServiceName = "DebtFlowSipGateway")

$ErrorActionPreference = "Stop"
if (-not $PSCmdlet.ShouldProcess($ServiceName, "Stop and remove Windows Service (program data is retained)")) { return }
& sc.exe stop $ServiceName 2>$null | Out-Null
& sc.exe delete $ServiceName | Out-Null
if ($LASTEXITCODE -ne 0) { throw "failed to remove service $ServiceName" }
Write-Host "Service removed. Program files and ProgramData were retained for recovery."

