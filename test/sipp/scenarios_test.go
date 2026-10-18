package sipp_test

import (
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var requiredScenarios = []string{
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
	"late-200-after-cancel",
}

func TestScenarioFilesAreWellFormedAndRegistered(t *testing.T) {
	runner, err := os.ReadFile(filepath.Join("..", "..", "scripts", "run-sipp-matrix.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob("*.xml")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(requiredScenarios) {
		t.Fatalf("scenario files=%d, want %d", len(files), len(requiredScenarios))
	}
	for _, name := range requiredScenarios {
		t.Run(name, func(t *testing.T) {
			path := name + ".xml"
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var document struct {
				XMLName xml.Name
				Name    string `xml:"name,attr"`
			}
			decoder := xml.NewDecoder(strings.NewReader(string(raw)))
			decoder.CharsetReader = func(label string, input io.Reader) (io.Reader, error) {
				if strings.EqualFold(label, "ISO-8859-1") {
					return input, nil
				}
				return nil, fmt.Errorf("unsupported XML encoding %q", label)
			}
			if err := decoder.Decode(&document); err != nil {
				t.Fatalf("invalid XML: %v", err)
			}
			if document.XMLName.Local != "scenario" || strings.TrimSpace(document.Name) == "" {
				t.Fatalf("invalid SIPp scenario root: %+v", document)
			}
			if !strings.Contains(string(runner), `"`+name+`"`) {
				t.Fatalf("scenario is not registered in run-sipp-matrix.ps1")
			}
		})
	}
}
