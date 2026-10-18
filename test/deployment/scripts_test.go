package deployment

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestInstallerValidatesStagedBinaryThenStopsServiceBeforeReplacingStableBinary(t *testing.T) {
	script := readScript(t, "install-service.ps1")
	validate := strings.Index(script, "& $stagedTarget --config $config --check-config")
	stop := strings.Index(script, "Stop-Service -Name $ServiceName")
	replaceBinary := strings.Index(script, "Move-Item -LiteralPath $stagedTarget -Destination $target -Force")
	if validate < 0 || stop < 0 || replaceBinary < 0 || validate > stop || stop > replaceBinary {
		t.Fatal("installer must validate staging before stopping the service and replace only after it stops")
	}
}

func TestInstallerRollsBackBinaryAndRestartsExistingServiceOnFailure(t *testing.T) {
	script := readScript(t, "install-service.ps1")
	for _, required := range []string{
		`Copy-Item -LiteralPath $target -Destination $rollbackTarget -Force`,
		`Copy-Item -LiteralPath $rollbackTarget -Destination $target -Force`,
		`Start-Service -Name $ServiceName -ErrorAction SilentlyContinue`,
		`Remove-Item -LiteralPath $stagedTarget -Force -ErrorAction SilentlyContinue`,
		`-ExpectedSignerThumbprint is required with -RequireSignature`,
		`source executable must have a valid Authenticode signature from the expected signer`,
		`$actualThumbprint -ne $expectedThumbprint`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("transactional installer is missing %s", required)
		}
	}
}

func TestInstallerCreatesTheServiceBeforeGrantingItsVirtualAccountACL(t *testing.T) {
	script := readScript(t, "install-service.ps1")
	create := strings.Index(script, `"create", $ServiceName`)
	serviceSID := strings.Index(script, `Invoke-ServiceControl -Arguments @("sidtype"`)
	rootACL := strings.Index(script, `Set-DirectoryAcl -Path $DataDirectory`)
	if create < 0 || serviceSID < 0 || rootACL < 0 || create > serviceSID || serviceSID > rootACL {
		t.Fatal("installer must register and enable the service SID before applying virtual-account ACLs")
	}
	if !strings.Contains(script, `Set-DirectoryAcl -Path $dataPath -ServiceRights "M"`) ||
		!strings.Contains(script, `Set-DirectoryAcl -Path $logsPath -ServiceRights "M"`) ||
		!strings.Contains(script, `Set-DirectoryAcl -Path $keysPath -ServiceRights "RX"`) {
		t.Fatal("installer must keep keys read-only and grant modify only to data/log directories")
	}
}

func TestKeyProvisioningSupportsRunningBeforeServiceRegistration(t *testing.T) {
	script := readScript(t, "provision-capability-key.ps1")
	lookup := strings.Index(script, "Get-Service -Name $ServiceName -ErrorAction SilentlyContinue")
	grant := strings.Index(script, `"NT SERVICE\${ServiceName}:(OI)(CI)RX"`)
	write := strings.Index(script, "[System.IO.File]::WriteAllText")
	if lookup < 0 || grant < 0 || write < 0 || lookup > grant || grant > write {
		t.Fatal("key provisioning must conditionally grant the registered service read access before writing the key")
	}
}

func TestReleasePreflightRequiresSignedPackageEvidenceAndProtectedKeys(t *testing.T) {
	script := readScript(t, "test-pilot-preflight.ps1")
	for _, required := range []string{
		`"release-binary-checksum"`,
		`"release-package-checksums"`,
		`"release-cyclonedx-sbom"`,
		`"release-build-info"`,
		`"release-manifest-binary-binding"`,
		`"capability-keys-protected"`,
		`"secure-browser-origin"`,
		`Get-AuthenticodeSignature`,
		`"-ExpectedSignerThumbprint is required"`,
		`$actualThumbprint -eq $expectedThumbprint`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("release preflight is missing %s", required)
		}
	}
	if !strings.Contains(script, `$checksumValid -or -not $RequireReleaseEvidence`) ||
		!strings.Contains(script, `$packageChecksumsValid -or -not $RequireReleaseEvidence`) ||
		!strings.Contains(script, `$sbomValid -or -not $RequireReleaseEvidence`) {
		t.Fatal("development mode may relax package evidence but release mode must not")
	}
	for _, required := range []string{
		`StartsWith($releasePrefix, [StringComparison]::OrdinalIgnoreCase)`,
		`duplicate checksum path`,
		`$actualHash -ne $expectedHash`,
		`$entryCount -gt 0 -and $binaryEntrySeen`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("recursive package checksum verification is missing %s", required)
		}
	}
	if !strings.Contains(script, `"wildcard browser origin enabled"`) ||
		!strings.Contains(script, `Add-Check "secure-browser-origin" (-not $insecureLoopbackOrigin)`) {
		t.Fatal("preflight must disclose but permit the explicit HTTPS wildcard policy")
	}
}

func TestReleasePreflightScansForbiddenAPIClientKeysWithoutRejectingCapabilityIssuer(t *testing.T) {
	script := readScript(t, "test-pilot-preflight.ps1")
	if strings.Contains(script, `-Pattern '(?i)web.?api|api.?url|http.?client|bearer|authorization'`) {
		t.Fatal("preflight must not scan arbitrary values or comments for API-client words")
	}
	const pattern = `(?im)^\s*[A-Za-z0-9_-]*(?:api|http.?client|bearer|authorization)[A-Za-z0-9_-]*\s*:`
	matcher := regexp.MustCompile(pattern)
	config, err := os.ReadFile(filepath.Join(moduleRoot(t), "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if matcher.Match(config) {
		t.Fatal("production configuration identity values must not look like an outbound API client")
	}
	for _, forbidden := range []string{
		"webApiUrl: https://api.example.test",
		"debtFlowApiEndpoint: https://api.example.test",
		"httpClient: enabled",
		"authorization: bearer",
	} {
		if !matcher.MatchString(forbidden) {
			t.Fatalf("forbidden API-client key was not detected: %q", forbidden)
		}
	}
}

func TestReleaseBuilderUsesSupportedGoModuleMetadataTemplate(t *testing.T) {
	script := readScript(t, "build.ps1")
	if strings.Contains(script, `{{json .}}`) {
		t.Fatal("go list templates do not provide a json function")
	}
	for _, required := range []string{
		`go list -deps`,
		`{{with .Module}}{{if not .Main}}{{.Path}}`,
		`{{if .Version}}{{.Version}}{{else}}unknown{{end}}`,
		`if ($LASTEXITCODE -ne 0) { throw "go list failed while generating the SBOM" }`,
		`$moduleRecords | Sort-Object -Unique`,
		`$fields = $_ -split "` + "`t" + `", 2`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("release builder is missing %s", required)
		}
	}
}

func TestSignedReleaseBuilderBindsVersionAndCommitFromACleanTree(t *testing.T) {
	script := readScript(t, "build.ps1")
	for _, required := range []string{
		`[ValidatePattern('^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$')]`,
		`if ($RequireSignature -and (-not $hasGitCommit -or $sourceModified))`,
		`release-manifest.json`,
		`gitCommit = $gitCommit`,
		`targetOs = "windows"`,
		`targetArchitecture = "amd64"`,
		`Get-ChildItem -LiteralPath $OutputDirectory -Force`,
		`signed release builds require a new or empty output directory`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("signed release builder is missing %s", required)
		}
	}
}

func TestOutboxAlertsUseConfiguredCapacityRatio(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(moduleRoot(t), "deploy", "prometheus", "sip-gateway.rules.yml"))
	if err != nil {
		t.Fatal(err)
	}
	rules := string(raw)
	for _, expression := range []string{
		"debt_flow_sip_gateway_outbox_capacity_ratio >= 0.8",
		"debt_flow_sip_gateway_outbox_capacity_ratio >= 0.9",
	} {
		if !strings.Contains(rules, expression) {
			t.Fatalf("Prometheus rules are missing threshold expression %q", expression)
		}
	}
	if strings.Contains(rules, "outbox_pending_events > 80000") ||
		strings.Contains(rules, "outbox_pending_events > 90000") {
		t.Fatal("outbox alerts must follow configured capacity rather than default absolute limits")
	}
}

func TestCapacityEvidenceAndDashboardExposeOutboxPressure(t *testing.T) {
	script := readScript(t, "observe-live-capacity.ps1")
	for _, required := range []string{
		`outboxCapacityRatio = Read-Metric $metrics "debt_flow_sip_gateway_outbox_capacity_ratio"`,
		`maxOutboxCapacityRatio = ($samples.outboxCapacityRatio | Measure-Object -Maximum).Maximum`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("capacity sampler is missing %s", required)
		}
	}
	dashboard, err := os.ReadFile(filepath.Join(moduleRoot(t), "deploy", "grafana", "sip-gateway-dashboard.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dashboard), `"expr": "debt_flow_sip_gateway_outbox_capacity_ratio"`) {
		t.Fatal("Grafana dashboard does not expose configured outbox capacity pressure")
	}
}

func TestSQLiteFailureAlertIsConfigured(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(moduleRoot(t), "deploy", "prometheus", "sip-gateway.rules.yml"))
	if err != nil {
		t.Fatal(err)
	}
	rules := string(raw)
	if !strings.Contains(rules, "increase(debt_flow_sip_gateway_sqlite_failures_total[5m]) > 0") {
		t.Fatal("Prometheus rules do not alert on SQLite operation failures")
	}
}

func TestMediaQualityAlertsAndDashboardAreConfigured(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(moduleRoot(t), "deploy", "prometheus", "sip-gateway.rules.yml"))
	if err != nil {
		t.Fatal(err)
	}
	rules := string(raw)
	var rulesDocument map[string]any
	if err := yaml.Unmarshal(raw, &rulesDocument); err != nil {
		t.Fatalf("Prometheus rules are invalid YAML: %v", err)
	}
	for _, required := range []string{
		"DebtFlowSipGatewayRTPPacketLossHigh",
		"debt_flow_sip_gateway_rtp_reported_loss_total",
		") > 0.02",
		"DebtFlowSipGatewayRTPJitterHigh",
		"debt_flow_sip_gateway_rtp_jitter_seconds_bucket",
		") > 0.05",
	} {
		if !strings.Contains(rules, required) {
			t.Fatalf("Prometheus media rules are missing %q", required)
		}
	}
	dashboard, err := os.ReadFile(filepath.Join(moduleRoot(t), "deploy", "grafana", "sip-gateway-dashboard.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"RTP packet loss ratio", "RTCP jitter p95"} {
		if !strings.Contains(string(dashboard), required) {
			t.Fatalf("Grafana media dashboard is missing %q", required)
		}
	}
}

func readScript(t *testing.T, name string) string {
	t.Helper()
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	path := filepath.Join(filepath.Dir(current), "..", "..", "scripts", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
