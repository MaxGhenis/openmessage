//go:build !windows

package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/maxghenis/openmessage/internal/client"
	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"go.mau.fi/util/exhttp"
	"google.golang.org/protobuf/proto"
)

type googlePairingRoundTripper func(*http.Request) (*http.Response, error)

func (f googlePairingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestGoogleAccountPairingInterruptBeforeLongPollConnect(t *testing.T) {
	const childPathEnv = "OPENMESSAGE_TEST_PAIR_CANCEL_SESSION"
	if path := os.Getenv(childPathEnv); path != "" {
		settings := exhttp.SensibleClientSettings
		settings.TransportOverride = func(exhttp.ClientSettings) http.RoundTripper {
			return googlePairingRoundTripper(func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/SignInGaia") {
					data, err := proto.Marshal(&gmproto.SignInGaiaResponse{
						TokenData: &gmproto.TokenData{TachyonAuthToken: []byte("synthetic-test-token")},
						DeviceData: &gmproto.SignInGaiaResponse_DeviceData{
							DeviceWrapper: &gmproto.SignInGaiaResponse_DeviceData_DeviceWrapper{Device: &gmproto.Device{SourceID: "test-device"}},
							UnknownItems2: []*gmproto.RPCGaiaData_UnknownContainer_Item2_Item1{{DestOrSourceUUID: "11111111-1111-4111-8111-111111111111", UnknownInt4: 1}},
						},
					})
					if err != nil {
						return nil, err
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {libgm.ContentTypeProtobuf}}, Body: io.NopCloser(bytes.NewReader(data)), Request: req}, nil
				}
				if strings.HasSuffix(req.URL.Path, "/ReceiveMessages") {
					// Fail before libgm signals its initial long-poll WaitGroup.
					// Send a real interrupt in this child process only.
					go func() {
						time.Sleep(20 * time.Millisecond)
						_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
					}()
					return &http.Response{StatusCode: 401, Header: http.Header{"Content-Type": {libgm.ContentTypeProtobuf}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
				}
				return nil, fmt.Errorf("unexpected synthetic request path %s", req.URL.Path)
			})
		}
		gm := libgm.NewClient(libgm.NewAuthData(), nil, zerolog.Nop(), settings)
		defer gm.Disconnect()
		err := runGoogleAccountPairing(&client.Client{GM: gm, Logger: zerolog.Nop()}, path, `{"SID":"synthetic-cookie"}`)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("interrupt should cancel pairing: %v", err)
		}
		return
	}

	path := filepath.Join(t.TempDir(), "session.json")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestGoogleAccountPairingInterruptBeforeLongPollConnect$", "-test.v")
	child.Env = append(os.Environ(), childPathEnv+"="+path)
	output, err := child.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("Google pairing did not exit after interrupt while its first long poll failed: %s", output)
	}
	if err != nil {
		t.Fatalf("pairing child failed: %v\n%s", err, output)
	}
	assertGooglePairingNotSaved(t, path, string(output))
	if strings.Contains(string(output), "EMOJI:") {
		t.Errorf("failed startup displayed an emoji: %s", output)
	}
}
