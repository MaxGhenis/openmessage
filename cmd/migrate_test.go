package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/migration"
)

func TestMigrateCheckEmitsSplitReportAndCleansStaging(t *testing.T) {
	sourceDir := newMigrateSourceFixture(t)
	targetDir := filepath.Join(sourceDir, "v2")
	if err := os.Mkdir(targetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, v2TempStoreName), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	staleSource := filepath.Join(targetDir, v2TempStoreName+v2TempSourceSuffix)
	if err := os.Mkdir(staleSource, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staleSource, "messages.db"), []byte("stale private snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(targetDir, v2TempBlobName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, v2TempBlobName, "stale"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	legacyBefore, err := os.ReadFile(filepath.Join(sourceDir, legacyDatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	deps := testMigrateDependencies(t)
	deps.transform = successfulTestTransform(t, func(options migration.Options) {
		if !options.Check {
			t.Fatal("transform Check = false, want true")
		}
		for _, path := range []string{
			options.TempStorePath,
			options.TempStorePath + v2TempSourceSuffix,
			options.TempBlobPath,
		} {
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("stale staging was not removed before Transform: %s (%v)", path, err)
			}
		}
	})

	var stdout, stderr bytes.Buffer
	err = runMigrateCommand(
		context.Background(),
		[]string{"--check", "--from", sourceDir, "--json"},
		deps,
		&stdout,
		&stderr,
	)
	if err != nil {
		t.Fatalf("runMigrateCommand: %v\nstderr:\n%s", err, stderr.String())
	}

	var report migration.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("stdout is not an integrity report: %v\n%s", err, stdout.String())
	}
	if !report.OK || !report.Check || !report.Validation.Passed || report.Target.Published {
		t.Fatalf("check report = %+v", report)
	}
	for _, path := range []string{
		filepath.Join(targetDir, v2StoreName),
		filepath.Join(targetDir, v2BlobName),
		filepath.Join(targetDir, v2TempStoreName),
		filepath.Join(targetDir, v2TempStoreName+v2TempSourceSuffix),
		filepath.Join(targetDir, v2TempBlobName),
	} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("check left migration output %s: %v", path, err)
		}
	}
	legacyAfter, err := os.ReadFile(filepath.Join(sourceDir, legacyDatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(legacyBefore, legacyAfter) {
		t.Fatal("legacy database bytes changed during --check")
	}
	if got := stderr.String(); !strings.Contains(got, "OpenMessage migration check: PASSED") ||
		!strings.Contains(got, "contact_meta(CRM)") ||
		!strings.Contains(got, "no v2 store was published") {
		t.Fatalf("human report missing required evidence:\n%s", got)
	}
}

func TestMigratePublishesBlobsThenStoreAndMarksReport(t *testing.T) {
	sourceDir := newMigrateSourceFixture(t)
	targetDir := filepath.Join(sourceDir, "published-v2")
	deps := testMigrateDependencies(t)
	deps.transform = successfulTestTransform(t, func(options migration.Options) {
		if options.Check {
			t.Fatal("transform Check = true, want false")
		}
	})

	var stdout, stderr bytes.Buffer
	err := runMigrateCommand(
		context.Background(),
		[]string{"--from", sourceDir, "--to", targetDir},
		deps,
		&stdout,
		&stderr,
	)
	if err != nil {
		t.Fatalf("runMigrateCommand: %v\nstderr:\n%s", err, stderr.String())
	}

	var report migration.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("stdout is not an integrity report: %v\n%s", err, stdout.String())
	}
	if !report.OK || report.Check || !report.Validation.Passed || !report.Target.Published {
		t.Fatalf("publish report = %+v", report)
	}
	storeBytes, err := os.ReadFile(filepath.Join(targetDir, v2StoreName))
	if err != nil {
		t.Fatalf("read published store: %v", err)
	}
	if string(storeBytes) != "validated-v2-store" {
		t.Fatalf("published store = %q", storeBytes)
	}
	blobBytes, err := os.ReadFile(filepath.Join(targetDir, v2BlobName, "sha256", "fixture"))
	if err != nil {
		t.Fatalf("read published blob: %v", err)
	}
	if string(blobBytes) != "scheduled-media" {
		t.Fatalf("published blob = %q", blobBytes)
	}
	for _, path := range []string{
		filepath.Join(targetDir, v2TempStoreName),
		filepath.Join(targetDir, v2TempStoreName+v2TempSourceSuffix),
		filepath.Join(targetDir, v2TempBlobName),
	} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("published migration left staging %s: %v", path, err)
		}
	}
	if !strings.Contains(stderr.String(), "Migration published") {
		t.Fatalf("human report does not describe publication:\n%s", stderr.String())
	}
}

func TestMigrateHeldInstanceLockUsesExitCodeFour(t *testing.T) {
	sourceDir := newMigrateSourceFixture(t)
	lock, err := acquireInstanceLock(filepath.Join(sourceDir, instanceLockName), instanceLockRecord{
		PID: 4242, Process: "test backend",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	deps := testMigrateDependencies(t)
	deps.transform = func(context.Context, migration.Options) (migration.Report, error) {
		t.Fatal("Transform called while source lock was held")
		return migration.Report{}, nil
	}
	var stdout, stderr bytes.Buffer
	err = runMigrateCommand(
		context.Background(), []string{"--from", sourceDir}, deps, &stdout, &stderr,
	)
	if err == nil {
		t.Fatal("runMigrateCommand succeeded while instance lock was held")
	}
	if got := ExitCode(err); got != migrateLockExitCode {
		t.Fatalf("ExitCode = %d, want %d (%v)", got, migrateLockExitCode, err)
	}
	var output migrateFailureOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode failure output: %v\n%s", err, stdout.String())
	}
	if output.OK || output.ExitCode != migrateLockExitCode || !strings.Contains(output.Error, "locked") {
		t.Fatalf("failure output = %+v", output)
	}
	if !strings.Contains(stderr.String(), "stop the backend") {
		t.Fatalf("stderr is not actionable: %s", stderr.String())
	}
}

func TestMigrateRefusesCanonicalTargetState(t *testing.T) {
	sourceDir := newMigrateSourceFixture(t)
	targetDir := filepath.Join(sourceDir, "v2")
	if err := os.Mkdir(targetDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, v2StoreName), []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}

	deps := testMigrateDependencies(t)
	deps.transform = func(context.Context, migration.Options) (migration.Report, error) {
		t.Fatal("Transform called for an occupied target")
		return migration.Report{}, nil
	}
	result, err := executeMigrate(context.Background(), migrateOptions{sourceDir: sourceDir}, deps)
	if err == nil {
		t.Fatal("executeMigrate succeeded for occupied target")
	}
	if result.hasReport {
		t.Fatalf("occupied target unexpectedly produced transform report: %+v", result.report)
	}
	if got := ExitCode(err); got != migrateSourceExitCode {
		t.Fatalf("ExitCode = %d, want %d (%v)", got, migrateSourceExitCode, err)
	}
	if !strings.Contains(err.Error(), "never overwritten") {
		t.Fatalf("target refusal is not actionable: %v", err)
	}
	bytes, readErr := os.ReadFile(filepath.Join(targetDir, v2StoreName))
	if readErr != nil || string(bytes) != "existing" {
		t.Fatalf("canonical store changed: %q, %v", bytes, readErr)
	}
}

func TestMigrateValidationFailureUsesExitCodeFiveAndCleansStaging(t *testing.T) {
	sourceDir := newMigrateSourceFixture(t)
	targetDir := filepath.Join(sourceDir, "v2")
	deps := testMigrateDependencies(t)
	deps.transform = func(_ context.Context, options migration.Options) (migration.Report, error) {
		if err := os.WriteFile(options.TempStorePath, []byte("invalid"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(options.TempBlobPath, 0o700); err != nil {
			t.Fatal(err)
		}
		return migration.Report{
			Format: migration.ReportFormat,
			Check:  options.Check,
			Source: migration.SourceReport{DatabasePath: options.SourcePath, Unchanged: true},
			Target: migration.TargetReport{DatabasePath: options.TargetStorePath},
			Validation: migration.ValidationReport{
				QuickCheck: "row mismatch", Passed: false,
			},
		}, migration.ErrValidation
	}

	var stdout, stderr bytes.Buffer
	err := runMigrateCommand(
		context.Background(), []string{"--check", "--from", sourceDir}, deps, &stdout, &stderr,
	)
	if err == nil {
		t.Fatal("runMigrateCommand succeeded after validation failure")
	}
	if got := ExitCode(err); got != migrateTransformExitCode {
		t.Fatalf("ExitCode = %d, want %d (%v)", got, migrateTransformExitCode, err)
	}
	var report migration.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("stdout is not the failed integrity report: %v\n%s", err, stdout.String())
	}
	if report.OK || report.Validation.Passed {
		t.Fatalf("failed report = %+v", report)
	}
	for _, path := range []string{
		filepath.Join(targetDir, v2TempStoreName),
		filepath.Join(targetDir, v2TempStoreName+v2TempSourceSuffix),
		filepath.Join(targetDir, v2TempBlobName),
		filepath.Join(targetDir, v2StoreName), filepath.Join(targetDir, v2BlobName),
	} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("validation failure left output %s: %v", path, err)
		}
	}
	if !strings.Contains(stderr.String(), "Migration failed") {
		t.Fatalf("human failure missing from stderr: %s", stderr.String())
	}
}

func TestMigrationSourceFootprintIncludesHotWAL(t *testing.T) {
	directory := t.TempDir()
	databasePath := filepath.Join(directory, legacyDatabaseName)
	if err := os.WriteFile(databasePath, []byte("main"), 0o600); err != nil {
		t.Fatal(err)
	}
	databaseInfo, err := os.Stat(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := migrationSourceFootprint(databasePath, databaseInfo); err != nil || got != 4 {
		t.Fatalf("footprint without WAL = %d, %v; want 4, nil", got, err)
	}
	if err := os.WriteFile(databasePath+"-wal", []byte("hot-wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := migrationSourceFootprint(databasePath, databaseInfo); err != nil || got != 11 {
		t.Fatalf("footprint with WAL = %d, %v; want 11, nil", got, err)
	}
}

func newMigrateSourceFixture(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(directory, legacyDatabaseName), []byte("immutable-legacy-fixture"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	return directory
}

func testMigrateDependencies(t *testing.T) migrateDependencies {
	t.Helper()
	return migrateDependencies{
		now:      func() time.Time { return time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC) },
		version:  "test-version",
		commit:   "test-commit",
		probeURL: "http://127.0.0.1:7007/api/status",
		httpClient: &http.Client{Transport: migrateRoundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, syscall.ECONNREFUSED
		})},
		availableBytes: func(string) (uint64, error) { return math.MaxUint64, nil },
		transform: func(context.Context, migration.Options) (migration.Report, error) {
			t.Fatal("test did not install a Transform fake")
			return migration.Report{}, nil
		},
	}
}

func successfulTestTransform(
	t *testing.T,
	inspect func(migration.Options),
) migrateTransform {
	t.Helper()
	return func(_ context.Context, options migration.Options) (migration.Report, error) {
		if inspect != nil {
			inspect(options)
		}
		if err := os.WriteFile(options.TempStorePath, []byte("validated-v2-store"), 0o600); err != nil {
			t.Fatalf("write staged store: %v", err)
		}
		blobDir := filepath.Join(options.TempBlobPath, "sha256")
		if err := os.MkdirAll(blobDir, 0o700); err != nil {
			t.Fatalf("create staged blob directory: %v", err)
		}
		if err := os.WriteFile(filepath.Join(blobDir, "fixture"), []byte("scheduled-media"), 0o600); err != nil {
			t.Fatalf("write staged blob: %v", err)
		}
		return migration.Report{
			Format: migration.ReportFormat,
			OK:     true,
			Check:  options.Check,
			Source: migration.SourceReport{
				Path: filepath.Dir(options.SourcePath), DatabasePath: options.SourcePath,
				QuickCheck: "ok", Unchanged: true,
			},
			Target: migration.TargetReport{
				Path: options.TargetPath, DatabasePath: options.TargetStorePath,
				SchemaVersion: 10,
			},
			TableCounts: map[string]migration.TableReconciliation{
				"messages": {
					Legacy: 2, V2: 2, Reconciled: 2, ReconciliationRatio: 1,
				},
			},
			PlatformCounts: map[string]migration.PlatformReconciliation{
				"signal": {
					AccountID: "signal-primary", Legacy: 1, V2: 1, ReconciliationRatio: 1,
				},
			},
			ReadState:     migration.ReadStateReport{LossyWarning: "legacy read state was lossy"},
			SampledHashes: []migration.SampledHash{{Matched: true}},
			Validation: migration.ValidationReport{
				QuickCheck: "ok", CountsMatched: true, SampledHashesMatched: true,
				BlobReferencesValid: true, SourceUnchanged: true, Passed: true,
			},
		}, nil
	}
}

type migrateRoundTripperFunc func(*http.Request) (*http.Response, error)

func (function migrateRoundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
