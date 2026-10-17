package deployment

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstallerStopsAnExistingServiceBeforeReplacingTheBinary(t *testing.T) {
	script := readScript(t, "install-service.ps1")
	stop := strings.Index(script, "Stop-Service -Name $ServiceName")
	copyBinary := strings.Index(script, "Copy-Item $source $target -Force")
	if stop < 0 || copyBinary < 0 || stop > copyBinary {
		t.Fatal("installer must stop an existing service before replacing its executable")
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
		`"release-cyclonedx-sbom"`,
		`"release-build-info"`,
		`"release-manifest-binary-binding"`,
		`"capability-keys-protected"`,
		`"secure-browser-origin"`,
		`Get-AuthenticodeSignature`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("release preflight is missing %s", required)
		}
	}
	if !strings.Contains(script, `$checksumValid -or -not $RequireReleaseEvidence`) ||
		!strings.Contains(script, `$sbomValid -or -not $RequireReleaseEvidence`) {
		t.Fatal("development mode may relax package evidence but release mode must not")
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
