[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$PrometheusDirectory,
    [Parameter(Mandatory = $true)][string]$AlertmanagerDirectory,
    [Parameter(Mandatory = $true)][string]$GrafanaDirectory,
    [string]$RuntimeDirectory = "$env:ProgramData\DebtFlow\SipGateway\monitoring"
)

$ErrorActionPreference = "Stop"
$repository = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$prometheusExe = Join-Path $PrometheusDirectory "prometheus.exe"
$alertmanagerExe = Join-Path $AlertmanagerDirectory "alertmanager.exe"
$grafanaExe = Join-Path $GrafanaDirectory "bin\grafana-server.exe"
$grafanaConfig = Join-Path $GrafanaDirectory "conf\defaults.ini"
foreach ($executable in @($prometheusExe, $alertmanagerExe, $grafanaExe)) {
    if (-not (Test-Path -LiteralPath $executable -PathType Leaf)) {
        throw "required executable not found: $executable"
    }
}

$prometheusData = Join-Path $RuntimeDirectory "prometheus-data"
$alertmanagerData = Join-Path $RuntimeDirectory "alertmanager-data"
$grafanaData = Join-Path $RuntimeDirectory "grafana-data"
$grafanaProvisioning = Join-Path $RuntimeDirectory "grafana-provisioning"
$grafanaDashboards = Join-Path $RuntimeDirectory "grafana-dashboards"
New-Item -ItemType Directory -Force -Path $RuntimeDirectory, $prometheusData, $alertmanagerData, $grafanaData, $grafanaProvisioning, $grafanaDashboards | Out-Null

$rulesSource = Join-Path $repository "deploy\prometheus\sip-gateway.rules.yml"
$rulesTarget = Join-Path $RuntimeDirectory "sip-gateway.rules.yml"
Copy-Item -LiteralPath $rulesSource -Destination $rulesTarget -Force
$prometheusTemplate = Get-Content -LiteralPath (Join-Path $repository "deploy\monitoring\prometheus.windows.yml.template") -Raw
$escapedRulesPath = $rulesTarget.Replace("\", "/")
$prometheusConfig = $prometheusTemplate.Replace("__RULES_PATH__", $escapedRulesPath)
$prometheusConfigPath = Join-Path $RuntimeDirectory "prometheus.yml"
[IO.File]::WriteAllText($prometheusConfigPath, $prometheusConfig, [Text.UTF8Encoding]::new($false))

$alertmanagerConfig = Join-Path $repository "deploy\monitoring\alertmanager.yml"
Copy-Item -Path (Join-Path $repository "deploy\monitoring\grafana\provisioning\*") -Destination $grafanaProvisioning -Recurse -Force
Copy-Item -LiteralPath (Join-Path $repository "deploy\grafana\sip-gateway-dashboard.json") -Destination $grafanaDashboards -Force
$dataSourcePath = Join-Path $grafanaProvisioning "datasources\prometheus.yml"
$dataSource = (Get-Content -LiteralPath $dataSourcePath -Raw).Replace("http://prometheus:9090", "http://127.0.0.1:9091")
[IO.File]::WriteAllText($dataSourcePath, $dataSource, [Text.UTF8Encoding]::new($false))
$dashboardProviderPath = Join-Path $grafanaProvisioning "dashboards\sip-gateway.yml"
$dashboardPath = $grafanaDashboards.Replace("\", "/")
$dashboardProvider = (Get-Content -LiteralPath $dashboardProviderPath -Raw).Replace("/var/lib/grafana/dashboards", $dashboardPath)
[IO.File]::WriteAllText($dashboardProviderPath, $dashboardProvider, [Text.UTF8Encoding]::new($false))

$env:GF_PATHS_DATA = $grafanaData
$env:GF_PATHS_PROVISIONING = $grafanaProvisioning
$env:GF_SECURITY_ADMIN_USER = "admin"
$env:GF_AUTH_ANONYMOUS_ENABLED = "false"
$env:GF_USERS_ALLOW_SIGN_UP = "false"
if ([string]::IsNullOrWhiteSpace($env:GF_SECURITY_ADMIN_PASSWORD)) {
    throw "set GF_SECURITY_ADMIN_PASSWORD to a strong password before starting local monitoring"
}

$processes = @()
$processes += Start-Process -FilePath $alertmanagerExe -ArgumentList @(
    "--config.file=`"$alertmanagerConfig`"", "--storage.path=`"$alertmanagerData`"", "--web.listen-address=127.0.0.1:9093"
) -PassThru
$processes += Start-Process -FilePath $prometheusExe -ArgumentList @(
    "--config.file=`"$prometheusConfigPath`"", "--storage.tsdb.path=`"$prometheusData`"", "--storage.tsdb.retention.time=15d",
    "--storage.tsdb.retention.size=2GB", "--web.listen-address=127.0.0.1:9091"
) -PassThru
$processes += Start-Process -FilePath $grafanaExe -WorkingDirectory $GrafanaDirectory -ArgumentList @(
    "--homepath=`"$GrafanaDirectory`"", "--config=`"$grafanaConfig`"",
    "cfg:server.http_addr=127.0.0.1", "cfg:server.http_port=3000"
) -PassThru

$processes | Select-Object Id, ProcessName, StartTime |
    ConvertTo-Json | Set-Content -LiteralPath (Join-Path $RuntimeDirectory "processes.json") -Encoding utf8
Write-Host "Grafana:      http://127.0.0.1:3000"
Write-Host "Prometheus:   http://127.0.0.1:9091"
Write-Host "Alertmanager: http://127.0.0.1:9093"
Write-Host "Diagnostics:  http://127.0.0.1:9090/diagnostics/"
