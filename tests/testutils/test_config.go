package testutils

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/datazip-inc/olake/tests/testutils/constants"
)

// ExecuteQueryFn drives the driver's source: the suites call it with an operation name, and the
// driver's own implementation knows what that means for its source.
type ExecuteQueryFn func(ctx context.Context, t *testing.T, cfg *TestConfig, operation string)

// TestConfig holds the configuration for a single test suite run per driver
type TestConfig struct {
	Driver     string
	DataFormat string

	// Suite is the unique identifier for the test suite running.
	// This is used to isolate test data and resources for concurrent test runs.
	Suite string

	// ImagePlatform overrides the platform the driver image runs under, for images that exist
	// only for amd64 and run emulated elsewhere.
	ImagePlatform string

	// DriverImage the test is intedned to run under. Defaults to the building the image of the current codebase
	DriverImage string

	// OlakeRootPath is the repo the tests run from, the directory `make docker.<driver>.build`
	// runs in and the committed fixtures are read from. Resolved by setupWorkingDir.
	OlakeRootPath string

	// TestWorkingDir is this config's private /tmp working dir, the folder where we run olake
	// commands and expect the generated files. Every file the suite reads or writes lives in it and
	// is addressed by name through GetFilePath, so there is no path to keep a field for.
	TestWorkingDir string

	// SourceBaseConfig is the working copy of source.json, parsed: the suite's own credentials,
	// database and prefixes, after applySuite renamed what it isolates. ExecuteQuery connects with
	// it, so the harness and olake always drive the same source.
	SourceBaseConfig SourceConfig `json:"-"`

	// Driver shape: the same for every suite this driver runs, so it is declared once here
	// rather than per test.
	Namespace       string
	ExecuteQuery    ExecuteQueryFn `json:"-"`
	DestinationDB   string
	CursorField     string
	PartitionRegex  string
	FilterConfig    string
	ColumnToExclude string

	// sourceEdit and streamEdit are the driver's own per-suite isolation: what a driver must rename
	// so its concurrent suites do not contend, and whatever that rename implies for the catalog.
	sourceEdit ConfigEditFn
	streamEdit ConfigEditFn
}

type TestConfigOption func(*TestConfig)

// ConfigEditFn edits one of the suite's working-copy JSON files. It receives the config so a
// driver can name what it isolates after the suite.
type ConfigEditFn func(cfg *TestConfig, doc map[string]interface{}) error

// NewTestConfig builds a driver's config from what a suite cannot derive for itself: the source it
// reads, the namespace it drives and the destination olake derives from them. dataFormat names the
// driver's testdata subdirectory, for the drivers that have one.
func NewTestConfig(t *testing.T, driver constants.DriverType, namespace, destinationDB string, executeQuery ExecuteQueryFn, opts ...TestConfigOption) (*TestConfig, error) {
	t.Helper()
	cfg := &TestConfig{
		Driver:        string(driver),
		Namespace:     namespace,
		DestinationDB: destinationDB,
		ExecuteQuery:  executeQuery,
	}

	for _, opt := range opts {
		opt(cfg)
	}

	err := cfg.setup(t)
	if err != nil {
		return nil, err
	}

	return cfg, nil
}

func WithImagePlatform(platform string) TestConfigOption {
	return func(c *TestConfig) {
		c.ImagePlatform = platform
	}
}

func WithDataFormat(dataFormat string) TestConfigOption {
	return func(c *TestConfig) {
		c.DataFormat = dataFormat
	}
}

func WithSourceEdit(edit ConfigEditFn) TestConfigOption {
	return func(c *TestConfig) {
		c.sourceEdit = edit
	}
}

func WithStreamEdit(edit ConfigEditFn) TestConfigOption {
	return func(c *TestConfig) {
		c.streamEdit = edit
	}
}

func (c *TestConfig) generateSuiteName(t *testing.T) {
	t.Helper()
	nonSuiteChars := regexp.MustCompile(`[^a-z0-9]+`)
	suite := strings.ToLower(t.Name())
	suite = strings.TrimPrefix(suite, "test")
	suite = strings.TrimPrefix(suite, strings.ToLower(string(c.Driver)))
	c.Suite = strings.Trim(nonSuiteChars.ReplaceAllString(suite, "_"), "_")
}

// setup initializes the derived fields of the TestConfig and does the steup for configuring the test isolation like isolated configs
func (c *TestConfig) setup(t *testing.T) error {
	t.Helper()

	c.generateSuiteName(t)
	if err := c.setupWorkingDir(t); err != nil {
		return err
	}
	if err := c.getOrBuildDriverImage(); err != nil {
		return err
	}
	c.addTimingLogsMiddleware()

	if err := c.applySuite(); err != nil {
		return err
	}

	sourceConfig, err := ReadSourceConfig(c.GetFilePath("source.json"))
	if err != nil {
		return fmt.Errorf("failed to read the source config of driver %q suite %q: %s", c.Driver, c.Suite, err)
	}
	c.SourceBaseConfig = sourceConfig

	return nil
}

func (c *TestConfig) String() string {
	config, _ := json.MarshalIndent(c, "", "  ")
	return string(config)
}

// getOrBuildDriverImage just sets the driver image in case  builds the driver image against current codebase
func (c *TestConfig) getOrBuildDriverImage() error {
	if c.DriverImage != "" {
		return nil
	}
	if image := os.Getenv(driverImageEnvVar); image != "" {
		c.DriverImage = image
		return nil
	}

	driverVersion := os.Getenv(driverVersionEnvVar)
	if driverVersion == "" {
		if err := buildDriverImage(c); err != nil {
			return fmt.Errorf("failed to build the %s driver image from the current codebase: %s", c.Driver, err)
		}
		driverVersion = currentDriverVersion
	}

	c.DriverImage = getDriverImage(c.Driver, driverVersion)
	return nil
}

func (c *TestConfig) addTimingLogsMiddleware() {
	executeFunc := c.ExecuteQuery
	c.ExecuteQuery = func(ctx context.Context, t *testing.T, cfg *TestConfig, operation string) {
		defer TrackPhaseTiming(t, c.Driver, fmt.Sprintf("query %q", operation))()
		executeFunc(ctx, t, cfg, operation)
	}
}

// UniqueID identifies this run among every suite that can be running beside it: the driver and the
// suite itself. Everything a suite must not share is named after it.
func (c *TestConfig) UniqueID() string {
	return Combine(c.withSuite(c.Driver))
}

func (c *TestConfig) withSuite(base string) string {
	return Combine(base, c.Suite)
}

// TestTableName is the source table a suite drives. The suite suffix is what keeps concurrent
// suites off each other's table -- without it they race the same DROP/CREATE.
func (c *TestConfig) GetTableName() string {
	return Combine("test_table_olake", c.Suite)
}

// GetFilePath addresses a file in the suite's working directory by name -- the configs, the
// catalog, state and stats all live there, and the container reads them under the same names.
func (c *TestConfig) GetFilePath(fileName string) string {
	return filepath.Join(c.TestWorkingDir, fileName)
}

// GetFixturePath addresses a committed fixture in the driver's testdata directory, for the one
// thing a run must outlive its working directory: the benchmark history the perf suite appends to.
// Everything else a suite reads is the working copy setupWorkingDir made, via GetFilePath.
func (c *TestConfig) GetFixturePath(fileName string, dataFormat ...string) string {
	return filepath.Join(c.OlakeRootPath, "tests", c.Driver, "testdata", filepath.Join(dataFormat...), fileName)
}

// setupWorkingDir gives the suite a private working directory holding its own copy of every config
// the driver container reads, so the repo fixtures stay read-only and concurrent suites never share
// a writable file. The shared fixtures land first and the driver's own overwrite them by name, so a
// driver overrides a common config just by committing a file of the same name.
func (c *TestConfig) setupWorkingDir(t *testing.T) (err error) {
	c.TestWorkingDir = t.TempDir()

	c.OlakeRootPath, err = repoRoot()
	if err != nil {
		return fmt.Errorf("failed to determine the repo root; the tests run from a git checkout: %s", err)
	}

	commonFixturesDir := filepath.Join(c.OlakeRootPath, "tests/testdata")
	driverFixuresDir := filepath.Join(c.OlakeRootPath, "tests", c.Driver, "testdata", c.DataFormat)
	for _, fixtures := range []string{commonFixturesDir, driverFixuresDir} {
		if err := copyDirFiles(fixtures, c.TestWorkingDir); err != nil {
			return fmt.Errorf("failed to copy the fixtures of %s into %s: %s", fixtures, c.TestWorkingDir, err)
		}
	}
	return nil
}

// applySuite derives every config the driver container reads from its committed base, so the base
// files stay untouched, and retargets the copies at the names this suite owns.
func (c *TestConfig) applySuite() error {
	c.DestinationDB = c.withSuite(c.DestinationDB)

	enableArrowWrites := func(destinationConf map[string]interface{}) error {
		writer, ok := destinationConf["writer"].(map[string]interface{})
		if !ok {
			return fmt.Errorf("no writer object in iceberg_destination.json")
		}
		writer["arrow_writes"] = true

		return nil
	}

	err := CopyJSONWithEdit(c.GetFilePath("iceberg_destination.json"), c.GetFilePath("iceberg_destination_arrow.json"), enableArrowWrites)
	if err != nil {
		return fmt.Errorf("failed to derive the arrow destination config of driver %q suite %q: %s", c.Driver, c.Suite, err)
	}

	isolateSource := func(source map[string]interface{}) error {
		if c.sourceEdit == nil {
			return nil
		}
		return c.sourceEdit(c, source)
	}
	if err := c.getOrRenderConfig("source.template.json", "source.json", isolateSource); err != nil {
		return fmt.Errorf("failed to isolate the source config of driver %q for suite %q: %s", c.Driver, c.Suite, err)
	}

	isolateCatalog := func(catalog map[string]interface{}) error {
		if c.streamEdit == nil {
			return nil
		}
		return c.streamEdit(c, catalog)
	}
	if err := c.getOrRenderConfig("streams.template.json", "streams.json", isolateCatalog); err != nil {
		return fmt.Errorf("failed to retarget the catalog of driver %q at suite %q table %s: %s", c.Driver, c.Suite, c.GetTableName(), err)
	}
	return nil
}

func (c *TestConfig) getOrRenderConfig(template, configPath string, edit editFunc) error {
	_, err := os.Stat(c.GetFilePath(configPath))
	if errors.Is(err, os.ErrNotExist) {
		return c.renderConfig(template, configPath, edit)
	} else if err != nil {
		return err
	}

	return nil
}

// renderConfig expands the placeholders of the committed template in base into the working copy the
// container reads at out, and applies edit to the result.
func (c *TestConfig) renderConfig(base, out string, edit editFunc) error {
	raw, err := os.ReadFile(c.GetFilePath(base))
	if err != nil {
		return fmt.Errorf("failed to read %s: %s", base, err)
	}
	expanded, err := c.expandPlaceholders(raw)
	if err != nil {
		return fmt.Errorf("failed to expand %s: %s", base, err)
	}
	doc, err := ParseJSONDoc(expanded)
	if err != nil {
		return fmt.Errorf("failed to parse %s: %s", base, err)
	}
	if err := edit(doc); err != nil {
		return err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal %s: %s", out, err)
	}
	return WriteHostFile(c.GetFilePath(out), data)
}

// placeholder matches the ${name} form alone: the source configs carry credentials, and a secret
// holding a bare $ has to survive rendering untouched.
var placeholder = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandPlaceholders substitutes the ${suite} a committed config spells its per-suite names with --
// ${SUITE} for the drivers whose identifiers are uppercase -- so the file reads as what it renders.
func (c *TestConfig) expandPlaceholders(raw []byte) ([]byte, error) {
	var unknown []string
	expanded := placeholder.ReplaceAllFunc(raw, func(match []byte) []byte {
		switch name := string(placeholder.FindSubmatch(match)[1]); name {
		case "suite":
			return []byte(c.Suite)
		case "SUITE":
			return []byte(strings.ToUpper(c.Suite))
		default:
			unknown = append(unknown, name)
			return match
		}
	})
	if len(unknown) > 0 {
		return nil, fmt.Errorf("undefined placeholder(s) %s: a config can only carry ${suite} and ${SUITE}", strings.Join(unknown, ", "))
	}
	return expanded, nil
}
