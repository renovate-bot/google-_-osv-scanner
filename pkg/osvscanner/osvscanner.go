package osvscanner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"deps.dev/util/resolve"
	scalibr "github.com/google/osv-scalibr"
	"github.com/google/osv-scalibr/artifact/image/layerscanning/image"
	"github.com/google/osv-scalibr/clients/datasource"
	"github.com/google/osv-scalibr/clients/resolution"
	"github.com/google/osv-scalibr/extractor"
	"github.com/google/osv-scalibr/plugin"
	"github.com/google/osv-scanner/v2/internal/clients/clientimpl/baseimagematcher"
	"github.com/google/osv-scanner/v2/internal/clients/clientimpl/licensematcher"
	"github.com/google/osv-scanner/v2/internal/clients/clientimpl/localmatcher"
	"github.com/google/osv-scanner/v2/internal/clients/clientimpl/osvmatcher"
	"github.com/google/osv-scanner/v2/internal/clients/clientinterfaces"
	"github.com/google/osv-scanner/v2/internal/cmdlogger"
	"github.com/google/osv-scanner/v2/internal/config"
	"github.com/google/osv-scanner/v2/internal/depsdev"
	"github.com/google/osv-scanner/v2/internal/imodels"
	"github.com/google/osv-scanner/v2/internal/imodels/results"
	"github.com/google/osv-scanner/v2/internal/output"
	"github.com/google/osv-scanner/v2/internal/scalibrextract"
	"github.com/google/osv-scanner/v2/internal/scalibrplugin"
	"github.com/google/osv-scanner/v2/internal/version"
	"github.com/google/osv-scanner/v2/pkg/models"
	"github.com/google/osv-scanner/v2/pkg/osvscanner/internal/imagehelpers"
	"github.com/ossf/osv-schema/bindings/go/osvschema"
	"osv.dev/bindings/go/osvdev"
)

type ScannerActions struct {
	ExperimentalScannerActions

	LockfilePaths      []string
	DirectoryPaths     []string
	GitCommits         []string
	Recursive          bool
	IncludeGitRoot     bool
	NoIgnore           bool
	Image              string
	IsImageArchive     bool
	ConfigOverridePath string
	CallAnalysisStates map[string]bool
	ShowAllPackages    bool
	ShowAllVulns       bool

	// local databases
	CompareOffline    bool
	DownloadDatabases bool
	LocalDBPath       string

	// license scanning
	ScanLicensesSummary   bool
	ScanLicensesAllowlist []string

	// Deprecated: in favor of LockfilePaths
	SBOMPaths []string
}

type ExperimentalScannerActions struct {
	TransitiveScanningActions

	ExtractorsEnabled  []string
	ExtractorsDisabled []string

	DetectorsEnabled  []string
	DetectorsDisabled []string
}

type TransitiveScanningActions struct {
	Disabled         bool
	NativeDataSource bool
	MavenRegistry    string
}

type ExternalAccessors struct {
	// Matchers
	VulnMatcher      clientinterfaces.VulnerabilityMatcher
	LicenseMatcher   clientinterfaces.LicenseMatcher
	BaseImageMatcher clientinterfaces.BaseImageMatcher

	// Required for pomxmlnet Extractor
	MavenRegistryAPIClient *datasource.MavenRegistryAPIClient
	// Required for vendored Extractor
	OSVDevClient *osvdev.OSVClient

	// DependencyClients is a map of implementations of DependencyClient
	// for each ecosystem, the following is currently implemented:
	// - [osvschema.EcosystemMaven] required for pomxmlnet Extractor
	DependencyClients map[osvschema.Ecosystem]resolve.Client
}

// ErrNoPackagesFound for when no packages are found during a scan.
var ErrNoPackagesFound = errors.New("no packages found in scan")

// ErrVulnerabilitiesFound includes both vulnerabilities being found or license violations being found,
// however, will not be raised if only uncalled vulnerabilities are found.
var ErrVulnerabilitiesFound = errors.New("vulnerabilities found")

// ErrAPIFailed describes errors related to querying API endpoints.
// TODO(v2): Actually use this error
var ErrAPIFailed = errors.New("API query failed")

func initializeExternalAccessors(actions ScannerActions) (ExternalAccessors, error) {
	externalAccessors := ExternalAccessors{
		DependencyClients: map[osvschema.Ecosystem]resolve.Client{},
	}
	var err error

	// Offline Mode
	// ------------
	if actions.CompareOffline {
		// --- Vulnerability Matcher ---
		externalAccessors.VulnMatcher, err =
			localmatcher.NewLocalMatcher(actions.LocalDBPath,
				"osv-scanner_scan/"+version.OSVVersion, actions.DownloadDatabases)
		if err != nil {
			return ExternalAccessors{}, err
		}

		return externalAccessors, nil
	}

	// Online Mode
	// -----------
	// --- Vulnerability Matcher ---
	externalAccessors.VulnMatcher = osvmatcher.New(5*time.Minute, "osv-scanner_scan/"+version.OSVVersion)

	// --- License Matcher ---
	if len(actions.ScanLicensesAllowlist) > 0 || actions.ScanLicensesSummary {
		depsDevAPIClient, err := datasource.NewCachedInsightsClient(depsdev.DepsdevAPI, "osv-scanner_scan/"+version.OSVVersion)
		if err != nil {
			return ExternalAccessors{}, err
		}

		externalAccessors.LicenseMatcher = &licensematcher.DepsDevLicenseMatcher{
			Client: depsDevAPIClient,
		}
	}

	// --- Base Image Matcher ---
	if actions.Image != "" {
		externalAccessors.BaseImageMatcher = &baseimagematcher.DepsDevBaseImageMatcher{
			HTTPClient: *http.DefaultClient,
			Config:     baseimagematcher.DefaultConfig(),
		}
	}

	// --- OSV.dev Client ---
	// We create a separate client from VulnMatcher to keep things clean.
	externalAccessors.OSVDevClient = osvdev.DefaultClient()

	// --- No Transitive Scanning ---
	if actions.Disabled {
		return externalAccessors, nil
	}

	// --- Transitive Scanning Clients ---
	externalAccessors.MavenRegistryAPIClient, err = datasource.NewMavenRegistryAPIClient(datasource.MavenRegistry{
		URL:             actions.MavenRegistry,
		ReleasesEnabled: true,
	}, "")

	if err != nil {
		return ExternalAccessors{}, err
	}

	if !actions.NativeDataSource {
		externalAccessors.DependencyClients[osvschema.EcosystemMaven], err = resolution.NewDepsDevClient(depsdev.DepsdevAPI, "osv-scanner_scan/"+version.OSVVersion)
	} else {
		externalAccessors.DependencyClients[osvschema.EcosystemMaven], err = resolution.NewMavenRegistryClient(actions.MavenRegistry, "")
	}

	// We only support native registry client for PyPI.
	externalAccessors.DependencyClients[osvschema.EcosystemPyPI] = resolution.NewPyPIRegistryClient("")

	if err != nil {
		return ExternalAccessors{}, err
	}

	return externalAccessors, nil
}

// DoScan performs the osv scanner action, with optional reporter to output information
func DoScan(actions ScannerActions) (models.VulnerabilityResults, error) {
	// --- Sanity check flags ----
	// TODO(v2): Move the logic of the offline flag changing other flags into here from the main.go/scan.go
	if actions.CompareOffline {
		if actions.ScanLicensesSummary {
			return models.VulnerabilityResults{}, errors.New("cannot retrieve licenses locally")
		}
	}

	if !actions.CompareOffline && actions.DownloadDatabases {
		return models.VulnerabilityResults{}, errors.New("databases can only be downloaded when running in offline mode")
	}

	scanResult := results.ScanResults{
		ConfigManager: config.Manager{
			DefaultConfig: config.Config{},
			ConfigMap:     make(map[string]config.Config),
		},
	}

	// --- Setup Config ---
	if actions.ConfigOverridePath != "" {
		err := scanResult.ConfigManager.UseOverride(actions.ConfigOverridePath)
		if err != nil {
			cmdlogger.Errorf("Failed to read config file: %s", err)
			return models.VulnerabilityResults{}, err
		}
	}

	// --- Setup Accessors/Clients ---
	accessors, err := initializeExternalAccessors(actions)
	if err != nil {
		return models.VulnerabilityResults{}, fmt.Errorf("failed to initialize accessors: %w", err)
	}

	// ----- Perform Scanning -----
	packagesAndFindings, err := scan(accessors, actions)
	if err != nil {
		return models.VulnerabilityResults{}, err
	}

	scanResult.PackageScanResults = packagesAndFindings.PackageResults
	scanResult.GenericFindings = packagesAndFindings.GenericFindings

	// ----- Filtering -----
	filterUnscannablePackages(&scanResult)
	filterIgnoredPackages(&scanResult)

	// ----- Custom Overrides -----
	overrideGoVersion(&scanResult)

	// --- Make Vulnerability Requests ---
	if accessors.VulnMatcher != nil {
		err = makeVulnRequestWithMatcher(scanResult.PackageScanResults, accessors.VulnMatcher)
		if err != nil {
			return models.VulnerabilityResults{}, err
		}
	}

	// --- Make License Requests ---
	if accessors.LicenseMatcher != nil {
		err = accessors.LicenseMatcher.MatchLicenses(context.Background(), scanResult.PackageScanResults)
		if err != nil {
			return models.VulnerabilityResults{}, err
		}
	}

	vulnerabilityResults := buildVulnerabilityResults(actions, &scanResult)

	if actions.ScanLicensesSummary {
		vulnerabilityResults.LicenseSummary = buildLicenseSummary(&scanResult)
	}

	filtered := filterResults(&vulnerabilityResults, &scanResult.ConfigManager, actions.ShowAllPackages)
	if filtered > 0 {
		cmdlogger.Infof(
			"Filtered %d %s from output",
			filtered,
			output.Form(filtered, "vulnerability", "vulnerabilities"),
		)
	}

	return vulnerabilityResults, determineReturnErr(vulnerabilityResults, actions.ShowAllVulns, false)
}

func DoContainerScan(actions ScannerActions) (models.VulnerabilityResults, error) {
	scanResult := results.ScanResults{
		ConfigManager: config.Manager{
			DefaultConfig: config.Config{},
			ConfigMap:     make(map[string]config.Config),
		},
	}

	if actions.ConfigOverridePath != "" {
		err := scanResult.ConfigManager.UseOverride(actions.ConfigOverridePath)
		if err != nil {
			cmdlogger.Errorf("Failed to read config file: %s", err)
			return models.VulnerabilityResults{}, err
		}
	}

	// --- Setup Accessors/Clients ---
	accessors, err := initializeExternalAccessors(actions)
	if err != nil {
		return models.VulnerabilityResults{}, fmt.Errorf("failed to initialize accessors: %w", err)
	}

	filesystemExtractors := getExtractors(
		scalibrextract.ExtractorsArtifacts,
		accessors,
		actions,
	)

	if len(filesystemExtractors) == 0 {
		return models.VulnerabilityResults{}, errors.New("at least one extractor must be enabled")
	}

	// --- Initialize Image To Scan ---'

	var img *image.Image
	if actions.IsImageArchive {
		cmdlogger.Infof("Scanning local image tarball %q", actions.Image)
		img, err = image.FromTarball(actions.Image, image.DefaultConfig())
	} else if actions.Image != "" {
		path, exportErr := imagehelpers.ExportDockerImage(actions.Image)
		if exportErr != nil {
			return models.VulnerabilityResults{}, exportErr
		}
		defer os.Remove(path)

		img, err = image.FromTarball(path, image.DefaultConfig())
		cmdlogger.Infof("Scanning image %q", actions.Image)
	}
	if err != nil {
		return models.VulnerabilityResults{}, err
	}

	defer func() {
		err := img.CleanUp()
		if err != nil {
			cmdlogger.Errorf("Failed to clean up image: %s", err)
		}
	}()

	detectors := scalibrplugin.ResolveEnabledDetectors(actions.DetectorsEnabled, actions.DetectorsDisabled)

	plugins := make([]plugin.Plugin, len(filesystemExtractors)+len(detectors))
	for i, ext := range filesystemExtractors {
		plugins[i] = ext.(plugin.Plugin)
	}

	for i, det := range detectors {
		plugins[i+len(filesystemExtractors)] = det.(plugin.Plugin)
	}

	capabilities := &plugin.Capabilities{
		DirectFS:      true,
		RunningSystem: false,
		OS:            plugin.OSLinux,
	}
	plugins = plugin.FilterByCapabilities(plugins, capabilities)

	// --- Do Scalibr Scan ---
	scanner := scalibr.New()
	scalibrSR, err := scanner.ScanContainer(context.Background(), img, &scalibr.ScanConfig{
		Plugins:      plugins,
		Capabilities: capabilities,
	})
	if err != nil {
		return models.VulnerabilityResults{}, fmt.Errorf("failed to scan container image: %w", err)
	}

	if scalibrSR.Inventory.IsEmpty() {
		return models.VulnerabilityResults{}, ErrNoPackagesFound
	}

	// --- Save Scalibr Scan Results ---
	scanResult.PackageScanResults = make([]imodels.PackageScanResult, len(scalibrSR.Inventory.Packages))
	for i, inv := range scalibrSR.Inventory.Packages {
		scanResult.PackageScanResults[i].PackageInfo = imodels.FromInventory(inv)
		scanResult.PackageScanResults[i].LayerDetails = inv.LayerDetails
	}

	// --- Fill Image Metadata ---
	scanResult.ImageMetadata, err = imagehelpers.BuildImageMetadata(img, accessors.BaseImageMatcher)
	if err != nil { // Not getting image metadata is not fatal
		cmdlogger.Errorf("Failed to fully get image metadata: %v", err)
	}

	// ----- Filtering -----
	filterUnscannablePackages(&scanResult)
	filterIgnoredPackages(&scanResult)

	filterNonContainerRelevantPackages(&scanResult)

	// --- Make Vulnerability Requests ---
	if accessors.VulnMatcher != nil {
		err = makeVulnRequestWithMatcher(scanResult.PackageScanResults, accessors.VulnMatcher)
		if err != nil {
			return models.VulnerabilityResults{}, err
		}
	}

	// --- Make License Requests ---
	if accessors.LicenseMatcher != nil {
		err = accessors.LicenseMatcher.MatchLicenses(context.Background(), scanResult.PackageScanResults)
		if err != nil {
			return models.VulnerabilityResults{}, err
		}
	}

	// TODO: This is a set of heuristics,
	//    - Assume that packages under usr/ might be a OS package depending on ecosystem
	//    - Assume python packages under dist-packages is a OS package
	// Replace this with an actual implementation in OSV-Scalibr (potentially via full filesystem accountability).
	for _, psr := range scanResult.PackageScanResults {
		if (strings.HasPrefix(psr.PackageInfo.Location(), "usr/") && psr.PackageInfo.Ecosystem().Ecosystem == osvschema.EcosystemGo) ||
			strings.Contains(psr.PackageInfo.Location(), "dist-packages/") && psr.PackageInfo.Ecosystem().Ecosystem == osvschema.EcosystemPyPI {
			psr.PackageInfo.AnnotationsDeprecated = append(psr.PackageInfo.AnnotationsDeprecated, extractor.InsideOSPackage)
		}
	}

	scanResult.GenericFindings = scalibrSR.Inventory.GenericFindings

	vulnerabilityResults := buildVulnerabilityResults(actions, &scanResult)

	if actions.ScanLicensesSummary {
		vulnerabilityResults.LicenseSummary = buildLicenseSummary(&scanResult)
	}

	filtered := filterResults(&vulnerabilityResults, &scanResult.ConfigManager, actions.ShowAllPackages)
	if filtered > 0 {
		cmdlogger.Infof(
			"Filtered %d %s from output",
			filtered,
			output.Form(filtered, "vulnerability", "vulnerabilities"),
		)
	}

	return vulnerabilityResults, determineReturnErr(vulnerabilityResults, actions.ShowAllVulns, true)
}

func buildLicenseSummary(scanResult *results.ScanResults) []models.LicenseCount {
	var licenseSummary []models.LicenseCount

	counts := make(map[models.License]int)
	for _, pkg := range scanResult.PackageScanResults {
		for _, l := range pkg.Licenses {
			counts[l] += 1
		}
	}

	if len(counts) == 0 {
		// No packages found.
		return []models.LicenseCount{}
	}

	licenses := slices.AppendSeq(make([]models.License, 0, len(counts)), maps.Keys(counts))

	// Sort the license count in descending count order with the UNKNOWN
	// license last.
	sort.Slice(licenses, func(i, j int) bool {
		if licenses[i] == "UNKNOWN" {
			return false
		}
		if licenses[j] == "UNKNOWN" {
			return true
		}
		if counts[licenses[i]] == counts[licenses[j]] {
			return licenses[i] < licenses[j]
		}

		return counts[licenses[i]] > counts[licenses[j]]
	})

	licenseSummary = make([]models.LicenseCount, len(licenses))
	for i, license := range licenses {
		licenseSummary[i].Name = license
		licenseSummary[i].Count = counts[license]
	}

	return licenseSummary
}

// determineReturnErr determines whether we found a "vulnerability" or not,
// and therefore whether we should return a ErrVulnerabilityFound error.
func determineReturnErr(vulnResults models.VulnerabilityResults, showAllVulns bool, isContainerScanning bool) error {
	if len(vulnResults.Results) > 0 {
		var vuln bool
		onlyUnimportantVuln := true
		var licenseViolation bool
		for _, vf := range vulnResults.Flatten() {
			if vf.Vulnerability.ID != "" {
				vuln = true
				// TODO(gongh): rewrite the logic once we support reachability analysis for container scanning.
				if !isContainerScanning && vf.GroupInfo.IsCalled() {
					onlyUnimportantVuln = false
				} else if isContainerScanning && !vf.GroupInfo.IsGroupUnimportant() {
					onlyUnimportantVuln = false
				}
			}
			if len(vf.LicenseViolations) > 0 {
				licenseViolation = true
			}
		}

		if !vuln && !licenseViolation {
			return nil
		}

		onlyUnimportantVuln = onlyUnimportantVuln && vuln && !licenseViolation

		// If the user didn't enable showing all vulns and we only found unimportant ones,
		// we should return without error.
		if !showAllVulns && onlyUnimportantVuln {
			// There is no error.
			return nil
		}

		return ErrVulnerabilitiesFound
	}

	return nil
}

// TODO(V2): Add context
func makeVulnRequestWithMatcher(
	packages []imodels.PackageScanResult,
	matcher clientinterfaces.VulnerabilityMatcher) error {
	invs := make([]*extractor.Package, 0, len(packages))
	for _, pkgs := range packages {
		invs = append(invs, pkgs.PackageInfo.Package)
	}

	res, err := matcher.MatchVulnerabilities(context.Background(), invs)
	if err != nil {
		cmdlogger.Errorf("error when retrieving vulns: %v", err)
		if res == nil {
			return err
		}
	}

	for i, vulns := range res {
		packages[i].Vulnerabilities = vulns
	}

	return nil
}

// Overrides Go version using osv-scanner.toml
func overrideGoVersion(scanResults *results.ScanResults) {
	for i, psr := range scanResults.PackageScanResults {
		pkg := psr.PackageInfo
		if pkg.Name() == "stdlib" && pkg.Ecosystem().Ecosystem == osvschema.EcosystemGo {
			configToUse := scanResults.ConfigManager.Get(pkg.Location())
			if configToUse.GoVersionOverride != "" {
				scanResults.PackageScanResults[i].PackageInfo.Package.Version = configToUse.GoVersionOverride
			}

			continue
		}
	}
}

// SetLogger sets the global slog handler for the cmdlogger.
func SetLogger(handler slog.Handler) {
	cmdlogger.GlobalHandler = handler
}
