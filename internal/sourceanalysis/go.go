// Package sourceanalysis provides functionality for performing source analysis on code.
package sourceanalysis

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/google/osv-scanner/v2/internal/cmdlogger"
	"github.com/google/osv-scanner/v2/internal/sourceanalysis/govulncheck"
	"github.com/google/osv-scanner/v2/internal/url"
	"github.com/google/osv-scanner/v2/pkg/models"
	"github.com/ossf/osv-schema/bindings/go/osvschema"
	"golang.org/x/vuln/scan"
)

func goAnalysis(pkgs []models.PackageVulns, source models.SourceInfo) {
	cmd := exec.Command("go", "version")
	_, err := cmd.Output()
	if err != nil {
		cmdlogger.Infof("Skipping call analysis on Go code since Go is not installed.")
		return
	}

	// Set GOVERSION to the Go version in go.mod.
	var goVersion string
	for _, pkg := range pkgs {
		if pkg.Package.Name == "stdlib" {
			goVersion = pkg.Package.Version
			break
		}
	}

	vulns, vulnsByID := vulnsFromAllPkgs(pkgs)
	// Filter out advisories with no symbol information first
	// This is purely an optimisation step, further filtering is done in matchAnalysisWithPackageVulns function
	filteredVulns := []osvschema.Vulnerability{}
	for _, vuln := range vulns {
		if vulnHasImportsField(vuln, nil) {
			filteredVulns = append(filteredVulns, vuln)
		}
	}

	res, err := runGovulncheck(filepath.Dir(source.Path), filteredVulns, goVersion)
	if err != nil {
		// TODO: Better method to identify the type of error and give advice specific to the error
		cmdlogger.Errorf(
			"Failed to run code analysis (govulncheck) on '%s' because %s\n"+
				"(the Go toolchain is required)", source.Path, err.Error(),
		)

		return
	}
	matchAnalysisWithPackageVulns(pkgs, res, vulnsByID)
}

func matchAnalysisWithPackageVulns(pkgs []models.PackageVulns, idToFindings map[string][]*govulncheck.Finding, vulnsByID map[string]osvschema.Vulnerability) {
	idToModuleToCalled := map[string]map[string]bool{}
	for id, findings := range idToFindings {
		idToModuleToCalled[id] = map[string]bool{}
		for _, f := range findings {
			modulePath := f.Trace[0].Module
			called := f.Trace[0].Function != ""
			idToModuleToCalled[f.OSV][modulePath] = called
		}
	}

	for _, pv := range pkgs {
		// Use index to keep reference to original element in slice
		for groupIdx := range pv.Groups {
			for _, vulnID := range pv.Groups[groupIdx].IDs {
				analysis := &pv.Groups[groupIdx].ExperimentalAnalysis
				if *analysis == nil {
					*analysis = make(map[string]models.AnalysisInfo)
				}

				moduleToCalled, ok := idToModuleToCalled[vulnID]
				if !ok { // If vulnerability not found, check if it contains any source information
					fillNotImportedAnalysisInfo(vulnsByID, vulnID, pv, analysis)
					continue
				}

				pkg := pv.Package
				if !vulnHasImportsField(vulnsByID[vulnID], &pkg) && moduleToCalled[pv.Package.Name] {
					// Vuln entry does not have any symbol information, therefore called being true is not useful
					continue
				}

				(*analysis)[vulnID] = models.AnalysisInfo{
					Called: moduleToCalled[pv.Package.Name],
				}
			}
		}
	}
}

func vulnHasImportsField(vuln osvschema.Vulnerability, pkg *models.PackageInfo) bool {
	for _, affected := range vuln.Affected {
		if pkg != nil {
			// TODO: Compare versions to see if this is the correct affected element
			// ver, err := semantic.Parse(pv.Package.Version, semantic.SemverVersion)
			if affected.Package.Name != pkg.Name {
				continue
			}
		}
		_, hasImportsField := affected.EcosystemSpecific["imports"]
		if hasImportsField {
			return true
		}
	}

	return false
}

// fillNotImportedAnalysisInfo checks for any source information in advisories, and sets called to false
func fillNotImportedAnalysisInfo(vulnsByID map[string]osvschema.Vulnerability, vulnID string, pv models.PackageVulns, analysis *map[string]models.AnalysisInfo) {
	if vulnHasImportsField(vulnsByID[vulnID], &pv.Package) {
		// If there is source information, then analysis has been performed, and
		// code does not import the vulnerable package, so definitely not called
		(*analysis)[vulnID] = models.AnalysisInfo{
			Called: false,
		}
	}
}

func runGovulncheck(moddir string, vulns []osvschema.Vulnerability, goVersion string) (map[string][]*govulncheck.Finding, error) {
	// Create a temporary directory containing all the vulnerabilities that
	// are passed in to check against govulncheck.
	//
	// This enables OSV scanner to supply the OSV vulnerabilities to run
	// against govulncheck and manage the database separately from vuln.go.dev.
	dbdir, err := os.MkdirTemp("", "")
	if err != nil {
		return nil, err
	}
	defer func() {
		rerr := os.RemoveAll(dbdir)
		if err == nil {
			err = rerr
		}
	}()

	for _, vuln := range vulns {
		dat, err := json.Marshal(vuln)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(fmt.Sprintf("%s/%s.json", dbdir, vuln.ID), dat, 0600); err != nil {
			return nil, err
		}
	}

	// this only errors if the file path is not absolute,
	// which paths from os.MkdirTemp should always be
	dbdirURL, _ := url.FromFilePath(dbdir)

	// Run govulncheck on the module at moddir and vulnerability database that
	// was just created.
	cmd := scan.Command(context.Background(), "-db", dbdirURL.String(), "-C", moddir, "-json", "./...")
	var b bytes.Buffer
	cmd.Stdout = &b
	cmd.Env = append(os.Environ(), "GOVERSION=go"+goVersion)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	if err := cmd.Wait(); err != nil {
		return nil, err
	}

	// Group the output of govulncheck based on the OSV ID.
	h := &osvHandler{
		idToFindings: map[string][]*govulncheck.Finding{},
	}
	if err := handleJSON(bytes.NewReader(b.Bytes()), h); err != nil {
		return nil, err
	}

	return h.idToFindings, nil
}

type osvHandler struct {
	idToFindings map[string][]*govulncheck.Finding
}

func (h *osvHandler) Finding(f *govulncheck.Finding) {
	h.idToFindings[f.OSV] = append(h.idToFindings[f.OSV], f)
}

func handleJSON(from io.Reader, to *osvHandler) error {
	dec := json.NewDecoder(from)
	for dec.More() {
		msg := govulncheck.Message{}
		// decode the next message in the stream
		if err := dec.Decode(&msg); err != nil {
			return err
		}
		if msg.Finding != nil {
			to.Finding(msg.Finding)
		}
	}

	return nil
}
