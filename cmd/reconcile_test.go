package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/maxghenis/openmessage/internal/db"
	"github.com/maxghenis/openmessage/internal/reconcile"
	"github.com/maxghenis/openmessage/internal/storage/sqlite"
)

func TestReconcileSignalRefusesWhenBackendProbeResponds(t *testing.T) {
	sourceDir := newReconcileSourceFixture(t, false)
	legacyPath := filepath.Join(sourceDir, legacyDatabaseName)
	v2Path := filepath.Join(sourceDir, "v2", v2StoreName)
	legacyBefore, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	v2Before, err := os.ReadFile(v2Path)
	if err != nil {
		t.Fatal(err)
	}

	var probed bool
	deps := reconcileSignalDependencies{
		now:      func() time.Time { return time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC) },
		version:  "test-version",
		commit:   "test-commit",
		probeURL: "http://127.0.0.1:7007/api/status",
		httpClient: &http.Client{Transport: reconcileRoundTripperFunc(func(request *http.Request) (*http.Response, error) {
			probed = true
			if request.Method != http.MethodGet || request.URL.Path != "/api/status" {
				t.Fatalf("probe request = %s %s", request.Method, request.URL)
			}
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("backend owns the endpoint")),
				Request:    request,
			}, nil
		})},
		openLegacy: func(string) (*db.Store, error) {
			t.Fatal("legacy store opened while backend probe reported running")
			return nil, nil
		},
		openV2: func(string) (*sqlite.Store, error) {
			t.Fatal("v2 store opened while backend probe reported running")
			return nil, nil
		},
		openV2ReadOnly: func(string) (*sqlite.Store, error) {
			t.Fatal("read-only v2 store opened while backend probe reported running")
			return nil, nil
		},
		reconcile: func(context.Context, reconcile.Options) (reconcile.Report, error) {
			t.Fatal("reconciler called while backend probe reported running")
			return reconcile.Report{}, nil
		},
	}

	var stdout, stderr bytes.Buffer
	err = runReconcileSignalCommand(
		context.Background(),
		zerolog.Nop(),
		[]string{"--from", sourceDir},
		deps,
		&stdout,
		&stderr,
	)
	if err == nil {
		t.Fatal("runReconcileSignalCommand() succeeded while backend was running")
	}
	if !probed {
		t.Fatal("backend was not probed")
	}
	if got := ExitCode(err); got != reconcileLockExitCode {
		t.Fatalf("ExitCode = %d, want %d (%v)", got, reconcileLockExitCode, err)
	}
	var output reconcileSignalFailureOutput
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode failure output: %v\n%s", err, stdout.String())
	}
	if output.OK || output.ExitCode != reconcileLockExitCode ||
		!strings.Contains(output.Error, "backend is running") {
		t.Fatalf("failure output = %+v", output)
	}
	if !strings.Contains(stderr.String(), "stop the backend and retry") {
		t.Fatalf("stderr is not actionable: %s", stderr.String())
	}
	legacyAfter, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	v2After, err := os.ReadFile(v2Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(legacyBefore, legacyAfter) || !bytes.Equal(v2Before, v2After) {
		t.Fatal("backend refusal changed a message store")
	}
}

func TestReconcileSignalParsesFlagsAndEmitsOneJSONReport(t *testing.T) {
	sourceDir := newReconcileSourceFixture(t, true)
	legacyPath := filepath.Join(sourceDir, legacyDatabaseName)
	v2Path := filepath.Join(sourceDir, "v2", v2StoreName)
	legacyBefore, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	v2Before, err := os.ReadFile(v2Path)
	if err != nil {
		t.Fatal(err)
	}
	deps := reconcileSignalDependencies{
		now:      func() time.Time { return time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC) },
		version:  "test-version",
		commit:   "test-commit",
		probeURL: "http://127.0.0.1:7007/api/status",
		httpClient: &http.Client{Transport: reconcileRoundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, syscall.ECONNREFUSED
		})},
		openLegacy: db.OpenReadOnly,
		openV2: func(string) (*sqlite.Store, error) {
			t.Fatal("writable v2 store opened for a dry run")
			return nil, nil
		},
		openV2ReadOnly: sqlite.OpenReadOnly,
	}
	want := reconcile.Report{
		DryRun:                 true,
		ConversationsScanned:   2,
		ConversationsCreated:   1,
		MessagesScanned:        7,
		MessagesImported:       3,
		MessagesAlreadyPresent: 4,
		MediaDeferred:          1,
		Skipped:                0,
		SkipReasons:            map[string]int{},
	}
	var called bool
	deps.reconcile = func(
		_ context.Context,
		options reconcile.Options,
	) (reconcile.Report, error) {
		called = true
		if options.Legacy == nil || options.V2 == nil {
			t.Fatal("reconcile options omitted a store")
		}
		if !options.DryRun {
			t.Fatal("DryRun = false, want true")
		}
		wantSince, err := parseDayBound("2026-07-15", false)
		if err != nil {
			t.Fatal(err)
		}
		if options.SinceMS != wantSince {
			t.Fatalf("SinceMS = %d, want %d", options.SinceMS, wantSince)
		}
		want.SinceMS = wantSince
		return want, nil
	}

	var stdout, stderr bytes.Buffer
	err = runReconcileSignalCommand(
		context.Background(),
		zerolog.Nop(),
		[]string{
			"--from", sourceDir,
			"--since", "2026-07-15",
			"--dry-run",
			"--json",
		},
		deps,
		&stdout,
		&stderr,
	)
	if err != nil {
		t.Fatalf("runReconcileSignalCommand(): %v\nstderr:\n%s", err, stderr.String())
	}
	if !called {
		t.Fatal("reconciler was not called")
	}
	var got reconcile.Report
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not a reconciliation report: %v\n%s", err, stdout.String())
	}
	if got.MessagesImported != want.MessagesImported ||
		got.MessagesAlreadyPresent != want.MessagesAlreadyPresent ||
		got.SinceMS != want.SinceMS ||
		!got.DryRun {
		t.Fatalf("report = %+v, want %+v", got, want)
	}
	decoder := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	if err := decoder.Decode(&got); err != nil {
		t.Fatalf("decode first stdout value: %v", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("stdout has more than one JSON value: err=%v extra=%v", err, extra)
	}
	if !strings.Contains(stderr.String(), "Signal reconciliation dry run complete") {
		t.Fatalf("human dry-run report missing from stderr: %s", stderr.String())
	}
	legacyAfter, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	v2After, err := os.ReadFile(v2Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(legacyBefore, legacyAfter) || !bytes.Equal(v2Before, v2After) {
		t.Fatal("dry run changed a message store")
	}
}

func TestDefaultReconcileSignalDataDir(t *testing.T) {
	t.Run("macOS app directory", func(t *testing.T) {
		t.Setenv("OPENMESSAGES_DATA_DIR", "")
		home := filepath.Join(t.TempDir(), "home")
		t.Setenv("HOME", home)

		want := filepath.Join(home, "Library", "Application Support", "OpenMessage")
		if got := defaultReconcileSignalDataDir(); got != want {
			t.Fatalf("defaultReconcileSignalDataDir() = %q, want %q", got, want)
		}
	})

	t.Run("environment override", func(t *testing.T) {
		override := filepath.Join(t.TempDir(), "explicit data")
		t.Setenv("OPENMESSAGES_DATA_DIR", override)

		if got := defaultReconcileSignalDataDir(); got != override {
			t.Fatalf("defaultReconcileSignalDataDir() = %q, want %q", got, override)
		}
	})
}

func TestReconcileSignalPartialFailureEmitsReportWithoutCompletion(t *testing.T) {
	sourceDir := newReconcileSourceFixture(t, true)
	sentinel := errors.New("injected reconciliation failure")
	deps := reconcileSignalDependencies{
		now:      func() time.Time { return time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC) },
		version:  "test-version",
		commit:   "test-commit",
		probeURL: "http://127.0.0.1:7007/api/status",
		httpClient: &http.Client{Transport: reconcileRoundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, syscall.ECONNREFUSED
		})},
		openLegacy: db.OpenReadOnly,
		openV2: func(string) (*sqlite.Store, error) {
			t.Fatal("writable v2 store opened for a dry run")
			return nil, nil
		},
		openV2ReadOnly: sqlite.OpenReadOnly,
		reconcile: func(
			context.Context,
			reconcile.Options,
		) (reconcile.Report, error) {
			return reconcile.Report{
				DryRun:           true,
				MessagesScanned:  3,
				MessagesImported: 2,
				SkipReasons:      map[string]int{},
			}, sentinel
		},
	}

	var stdout, stderr bytes.Buffer
	err := runReconcileSignalCommand(
		context.Background(),
		zerolog.Nop(),
		[]string{"--from", sourceDir, "--dry-run"},
		deps,
		&stdout,
		&stderr,
	)
	if !errors.Is(err, sentinel) {
		t.Fatalf("runReconcileSignalCommand() error = %v, want wrapped sentinel", err)
	}
	if got := ExitCode(err); got != reconcileRunExitCode {
		t.Fatalf("ExitCode = %d, want %d", got, reconcileRunExitCode)
	}
	var report reconcile.Report
	if decodeErr := json.Unmarshal(stdout.Bytes(), &report); decodeErr != nil {
		t.Fatalf("decode partial report: %v\n%s", decodeErr, stdout.String())
	}
	if report.MessagesScanned != 3 || report.MessagesImported != 2 {
		t.Fatalf("partial report = %+v", report)
	}
	if !strings.Contains(stderr.String(), "Signal reconciliation failed") {
		t.Fatalf("failure missing from stderr: %s", stderr.String())
	}
	if strings.Contains(stderr.String(), "complete") {
		t.Fatalf("stderr contradicts partial failure: %s", stderr.String())
	}
}

func TestReconcileSignalUsesWritableV2ForRepair(t *testing.T) {
	sourceDir := newReconcileSourceFixture(t, true)
	var writableOpened bool
	deps := reconcileSignalDependencies{
		now:      func() time.Time { return time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC) },
		version:  "test-version",
		commit:   "test-commit",
		probeURL: "http://127.0.0.1:7007/api/status",
		httpClient: &http.Client{Transport: reconcileRoundTripperFunc(func(*http.Request) (*http.Response, error) {
			return nil, syscall.ECONNREFUSED
		})},
		openLegacy: db.OpenReadOnly,
		openV2: func(path string) (*sqlite.Store, error) {
			writableOpened = true
			return sqlite.Open(path)
		},
		openV2ReadOnly: func(string) (*sqlite.Store, error) {
			t.Fatal("read-only v2 store opened for a repair")
			return nil, nil
		},
		reconcile: func(
			context.Context,
			reconcile.Options,
		) (reconcile.Report, error) {
			return reconcile.Report{SkipReasons: map[string]int{}}, nil
		},
	}

	var stdout, stderr bytes.Buffer
	if err := runReconcileSignalCommand(
		context.Background(),
		zerolog.Nop(),
		[]string{"--from", sourceDir},
		deps,
		&stdout,
		&stderr,
	); err != nil {
		t.Fatalf("runReconcileSignalCommand(): %v\nstderr:\n%s", err, stderr.String())
	}
	if !writableOpened {
		t.Fatal("repair did not open the writable v2 store")
	}
}

func newReconcileSourceFixture(t *testing.T, validStores bool) string {
	t.Helper()
	sourceDir := t.TempDir()
	v2Dir := filepath.Join(sourceDir, "v2")
	if err := os.Mkdir(v2Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(sourceDir, legacyDatabaseName)
	v2Path := filepath.Join(v2Dir, v2StoreName)
	if !validStores {
		if err := os.WriteFile(legacyPath, []byte("legacy sentinel"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(v2Path, []byte("v2 sentinel"), 0o600); err != nil {
			t.Fatal(err)
		}
		return sourceDir
	}

	legacy, err := db.New(legacyPath)
	if err != nil {
		t.Fatalf("db.New(): %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("legacy.Close(): %v", err)
	}
	v2, err := sqlite.Open(v2Path)
	if err != nil {
		t.Fatalf("sqlite.Open(): %v", err)
	}
	if err := v2.Close(); err != nil {
		t.Fatalf("v2.Close(): %v", err)
	}
	return sourceDir
}

type reconcileRoundTripperFunc func(*http.Request) (*http.Response, error)

func (function reconcileRoundTripperFunc) RoundTrip(
	request *http.Request,
) (*http.Response, error) {
	return function(request)
}
