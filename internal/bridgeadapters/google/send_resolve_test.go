package google

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"testing/quick"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"

	"github.com/maxghenis/openmessage/internal/app"
	"github.com/maxghenis/openmessage/internal/bridge"
	"github.com/maxghenis/openmessage/internal/client"
)

const (
	testSwitchAccount = "owner@example.com"
	testPeerNumber    = "+15551234567"
	testSelfNumber    = "+15550000000"
	testRemoteID      = "2957"
)

// The exhaustive SendMessageResponse table (design A3/A4, invariants I2 and
// I3): every status, with and without an account switch, for text and media.
func TestSendMessageResponseClassificationIsTotal(t *testing.T) {
	statuses := []gmproto.SendMessageResponse_Status{0, 1, 2, 3, 4, 99}
	switches := []*gmproto.AccountChangeOrSomethingEvent{
		nil,
		{Account: "no-at"},
		{Account: "x@gmail.com"},
	}
	for _, kind := range []string{"text", "media"} {
		for _, status := range statuses {
			for _, accountSwitch := range switches {
				name := fmt.Sprintf("%s/%s/switch=%q", kind, status, accountSwitch.GetAccount())
				t.Run(name, func(t *testing.T) {
					h := startSendRun(t, newLegacyClient)
					response := &gmproto.SendMessageResponse{
						Status:              status,
						GoogleAccountSwitch: accountSwitch,
					}
					err, sends := h.sendWithResponse(t, kind, response)
					if sends != 1 {
						t.Fatalf("SendMessage calls = %d, want 1", sends)
					}
					want := expectedResponseClassification(kind, status, accountSwitch.GetAccount())
					if want.ok {
						if err != nil {
							t.Fatalf("send error = %v, want success", err)
						}
					} else {
						failure, ok := asOpError(err)
						if !ok {
							t.Fatalf("send error = %T %v, want bridge.OpError", err, err)
						}
						if failure.Class != want.class || failure.Fingerprint != want.fingerprint ||
							failure.Dispatch != want.dispatch || failure.Operation != "send_"+kind {
							t.Fatalf("failure = %+v, want class %q fingerprint %q dispatch %q",
								failure, want.class, want.fingerprint, want.dispatch)
						}
						// I2: an UNKNOWN status is never retried as not dispatched.
						if status == gmproto.SendMessageResponse_UNKNOWN &&
							retryableByDispatcher(failure.Class) && failure.Dispatch == bridge.DispatchNotCalled {
							t.Fatalf("UNKNOWN status classified retryable + not dispatched: %+v", failure)
						}
						// error_detail keeps the status and any switch account.
						if !strings.Contains(err.Error(), status.String()) {
							t.Fatalf("error %q does not name status %s", err.Error(), status)
						}
						if accountSwitch != nil && !strings.Contains(err.Error(), accountSwitch.GetAccount()) {
							t.Fatalf("error %q does not name switch account %q", err.Error(), accountSwitch.GetAccount())
						}
						if want.flag && !strings.Contains(err.Error(), "["+fingerprintAccountPairingSwitched+"]") {
							t.Fatalf("refusal %q does not carry its fingerprint in the text", err.Error())
						}
					}
					snapshot := h.host.GoogleStatus()
					if snapshot.AccountSwitched != want.flag {
						t.Fatalf("AccountSwitched = %v, want %v", snapshot.AccountSwitched, want.flag)
					}
					if want.flag && snapshot.SwitchedAccount != accountSwitch.GetAccount() {
						t.Fatalf("SwitchedAccount = %q, want %q", snapshot.SwitchedAccount, accountSwitch.GetAccount())
					}
					// I3: no response classification touches the receive lifecycle.
					h.assertGenerationIntact(t, "send response "+name)
				})
			}
		}
	}
}

type responseExpectation struct {
	ok          bool
	class       bridge.FailureClass
	fingerprint string
	dispatch    bridge.DispatchCertainty
	flag        bool
}

// expectedResponseClassification restates design A3 independently of the
// implementation.
func expectedResponseClassification(
	kind string,
	status gmproto.SendMessageResponse_Status,
	account string,
) responseExpectation {
	switch {
	case status == gmproto.SendMessageResponse_SUCCESS:
		return responseExpectation{ok: true}
	case strings.Contains(account, "@"):
		return responseExpectation{
			class:       bridge.FailureReauthRequired,
			fingerprint: "google_account_pairing_switched",
			dispatch:    bridge.DispatchNotCalled,
			flag:        true,
		}
	case status == gmproto.SendMessageResponse_UNKNOWN:
		return responseExpectation{
			class:       bridge.FailureTransient,
			fingerprint: "google_" + kind + "_send_unknown_status",
			dispatch:    bridge.DispatchUncertain,
		}
	case status == gmproto.SendMessageResponse_FAILURE_2 || status == gmproto.SendMessageResponse_FAILURE_3:
		return responseExpectation{
			class:       bridge.FailureTransient,
			fingerprint: "google_" + kind + "_send_rejected",
			dispatch:    bridge.DispatchNotCalled,
		}
	default:
		return responseExpectation{
			class:       bridge.FailureMisconfigured,
			fingerprint: "google_" + kind + "_send_refused",
			dispatch:    bridge.DispatchNotCalled,
		}
	}
}

// retryableByDispatcher mirrors messaging.retryableFailure: the classes the
// dispatcher retries when the adapter reports DispatchNotCalled.
func retryableByDispatcher(class bridge.FailureClass) bool {
	switch class {
	case bridge.FailureTransient, bridge.FailureRateLimited, bridge.FailureCredentialsExpired:
		return true
	default:
		return false
	}
}

// The caption goes out after the media part, so whatever the phone says
// about it the overall outcome stays ambiguous; an account switch on it is
// still recorded for status.
func TestSendMediaCaptionClassificationStaysAmbiguous(t *testing.T) {
	statuses := []gmproto.SendMessageResponse_Status{0, 2, 3, 4, 99}
	switches := []*gmproto.AccountChangeOrSomethingEvent{nil, {Account: "no-at"}, {Account: "x@gmail.com"}}
	for _, status := range statuses {
		for _, accountSwitch := range switches {
			t.Run(fmt.Sprintf("%s/switch=%q", status, accountSwitch.GetAccount()), func(t *testing.T) {
				fake := &fakeMediaSendClient{
					uploadResult:       &gmproto.MediaContent{MediaID: "media-id"},
					conversationResult: &gmproto.Conversation{ConversationID: testRemoteID},
					sendResults: []*gmproto.SendMessageResponse{
						{Status: gmproto.SendMessageResponse_SUCCESS},
						{Status: status, GoogleAccountSwitch: accountSwitch},
					},
				}
				a := newMediaSendTestAdapter(t, fake)
				_, err := a.SendMedia(context.Background(), bridge.MediaRequest{
					Conversation: bridge.ConversationRef{RemoteID: testRemoteID},
					Reader:       strings.NewReader("x"),
					Size:         1,
					Caption:      "caption",
					RequestID:    "request-id",
				})
				failure := requireMediaOpError(t, err)
				if failure.Class != bridge.FailureTransient || failure.Dispatch != "" ||
					failure.Fingerprint != "google_caption_send_rejected" {
					t.Fatalf("failure = %+v, want ambiguous transient google_caption_send_rejected", failure)
				}
				if !strings.Contains(err.Error(), status.String()) {
					t.Fatalf("error %q does not name caption status %s", err.Error(), status)
				}
				if len(fake.sent) != 2 {
					t.Fatalf("SendMessage calls = %d, want media then caption", len(fake.sent))
				}
				wantFlag := strings.Contains(accountSwitch.GetAccount(), "@")
				if switched, _ := a.host.GoogleAccountSwitch(); switched != wantFlag {
					t.Fatalf("account switch flag = %v, want %v", switched, wantFlag)
				}
			})
		}
	}
}

// Design A1/A4 and invariant I3: the phone's account container raises the
// flag and the status fields without retiring the receive generation, and a
// send is then refused terminally before any further RPC.
func TestAccountChangeFlagsSessionWithoutRetiringGeneration(t *testing.T) {
	host := newTestApp(t)
	transport := &fakeTransport{}
	a := newTestAdapter(t, host, transport)
	sink := &recordingSink{}
	run, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "google-primary",
		Generation: 1,
	}, sink)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { stopRun(t, run) })

	transport.emit(accountChangeEvent(testSwitchAccount))
	// AccountChange is neither readiness, liveness nor ingress.
	assertNotClosed(t, run.Ready(), "AccountChange")
	if sink.beatCount() != 0 || len(sink.ingressRecords()) != 0 {
		t.Fatalf("AccountChange produced beats %d and ingress %d, want none",
			sink.beatCount(), len(sink.ingressRecords()))
	}
	status := host.GoogleStatus()
	if !status.AccountSwitched || status.SwitchedAccount != testSwitchAccount || status.AccountSwitchedAtMS <= 0 {
		t.Fatalf("status = %+v, want account switch for %s with a timestamp", status, testSwitchAccount)
	}
	if status.NeedsRepair {
		t.Fatal("AccountChange set needs_repair")
	}

	transport.emit(&gmproto.Conversation{ConversationID: "ready"})
	<-run.Ready()
	if !host.GoogleStatus().AccountSwitched {
		t.Fatal("generation readiness cleared the account switch; only proof the phone serves the session may")
	}
	h := &sendRunHarness{host: host, transport: transport, adapter: a, run: run}
	h.assertGenerationIntact(t, "AccountChange")

	fake := &fakeTextSendClient{}
	installTextSendClient(t, host.GetClient(), fake)
	_, err = a.SendText(context.Background(), bridge.TextRequest{
		Conversation: bridge.ConversationRef{
			RemoteID:         testRemoteID,
			Kind:             "direct",
			DirectPeerNumber: testPeerNumber,
		},
		Body:      "hello",
		RequestID: "request-id",
	})
	failure := requireTextOpError(t, err)
	requireAccountSwitchRefusal(t, failure, "send_text")
	if fake.resolve.calls != 0 || fake.sendCalls != 0 {
		t.Fatalf("refused send made GetOrCreate %d and SendMessage %d calls, want 0 and 0",
			fake.resolve.calls, fake.sendCalls)
	}
	h.assertGenerationIntact(t, "account-switch refusal")
}

// Design A1: Google-account sessions never raise the flag (whether they ever
// receive the container is unverified).
func TestGoogleAccountSessionNeverRaisesAccountSwitch(t *testing.T) {
	h := startSendRun(t, newGoogleAccountLegacyClient)
	h.transport.emit(accountChangeEvent(testSwitchAccount))
	if h.host.GoogleStatus().AccountSwitched {
		t.Fatal("AccountChange raised the account switch for a Google-account session")
	}

	empty := &fakeTextSendClient{}
	installTextSendClient(t, h.host.GetClient(), empty)
	_, err := h.adapter.SendText(context.Background(), bridge.TextRequest{
		Conversation: bridge.ConversationRef{RemoteID: testRemoteID},
		Body:         "hello",
		RequestID:    "request-id",
	})
	failure := requireTextOpError(t, err)
	if failure.Fingerprint != fingerprintConversationNotFound || failure.Class != bridge.FailureTransient {
		t.Fatalf("empty lookup on a Google-account session = %+v, want transient not-found", failure)
	}

	// A send response naming an account is still refused (upstream treats it
	// as a certain failure), but it never raises this session's flag.
	refused := &fakeTextSendClient{
		conversationResult: &gmproto.Conversation{ConversationID: testRemoteID},
		sendResult: &gmproto.SendMessageResponse{
			GoogleAccountSwitch: &gmproto.AccountChangeOrSomethingEvent{Account: testSwitchAccount},
		},
	}
	installTextSendClient(t, h.host.GetClient(), refused)
	_, err = h.adapter.SendText(context.Background(), bridge.TextRequest{
		Conversation: bridge.ConversationRef{RemoteID: testRemoteID},
		Body:         "hello",
		RequestID:    "request-id",
	})
	requireAccountSwitchRefusal(t, requireTextOpError(t, err), "send_text")
	if h.host.GoogleStatus().AccountSwitched {
		t.Fatal("a send response raised the account switch for a Google-account session")
	}
	h.assertGenerationIntact(t, "Google-account session")
}

// Design A1: proof that the phone serves this session clears the flag.
func TestProofThePhoneServesSessionClearsAccountSwitch(t *testing.T) {
	t.Run("conversation lookup returns data", func(t *testing.T) {
		h := startSendRun(t, newLegacyClient)
		h.transport.emit(accountChangeEvent(testSwitchAccount))
		fake := &fakeTextSendClient{
			conversationResult: &gmproto.Conversation{ConversationID: testRemoteID},
			sendResult:         &gmproto.SendMessageResponse{Status: gmproto.SendMessageResponse_FAILURE_2},
		}
		installTextSendClient(t, h.host.GetClient(), fake)
		_, _ = h.adapter.SendText(context.Background(), bridge.TextRequest{
			Conversation: bridge.ConversationRef{RemoteID: testRemoteID},
			Body:         "hello",
			RequestID:    "request-id",
		})
		if h.host.GoogleStatus().AccountSwitched {
			t.Fatal("a real conversation did not clear the account switch")
		}
	})

	t.Run("by-number lookup returns data", func(t *testing.T) {
		h := startSendRun(t, newLegacyClient)
		fake := &fakeTextSendClient{
			resolve: fakeConversationResolve{
				result: &gmproto.GetOrCreateConversationResponse{
					Conversation: directConversation(testRemoteID, testPeerNumber),
				},
				// The switch is raised while the lookup is in flight, then the
				// phone answers with data anyway.
				hook: func() { h.transport.emit(accountChangeEvent(testSwitchAccount)) },
			},
			sendResult: &gmproto.SendMessageResponse{Status: gmproto.SendMessageResponse_SUCCESS},
		}
		installTextSendClient(t, h.host.GetClient(), fake)
		if _, err := h.adapter.SendText(context.Background(), directTextRequest()); err != nil {
			t.Fatalf("SendText() error = %v", err)
		}
		if h.host.GoogleStatus().AccountSwitched {
			t.Fatal("a resolved conversation did not clear the account switch")
		}
	})

	t.Run("send succeeds", func(t *testing.T) {
		h := startSendRun(t, newLegacyClient)
		h.transport.emit(accountChangeEvent(testSwitchAccount))
		failure, failed := h.adapter.classifySendMessageResponse(
			h.host.GetClient(),
			"send_text",
			"text",
			&gmproto.SendMessageResponse{Status: gmproto.SendMessageResponse_SUCCESS},
		)
		if failed {
			t.Fatalf("SUCCESS classified as failure %+v", failure)
		}
		if h.host.GoogleStatus().AccountSwitched {
			t.Fatal("a successful send did not clear the account switch")
		}
	})
}

// libgm decrypts a frame, and so fires the fake AccountChange, before it hands
// the (empty) response to the waiting request. An empty lookup whose call
// raised the switch must therefore be refused, not retried (design A2 step 3).
func TestAccountSwitchRaisedDuringEmptyLookupIsTerminal(t *testing.T) {
	ref := bridge.ConversationRef{
		RemoteID:         testRemoteID,
		Kind:             "direct",
		DirectPeerNumber: testPeerNumber,
	}
	t.Run("text", func(t *testing.T) {
		h := startSendRun(t, newLegacyClient)
		fake := &fakeTextSendClient{conversationHook: h.emitAccountChange}
		installTextSendClient(t, h.host.GetClient(), fake)
		_, err := h.adapter.SendText(context.Background(), bridge.TextRequest{
			Conversation: ref, Body: "hello", RequestID: "request-id",
		})
		requireAccountSwitchRefusal(t, requireTextOpError(t, err), "send_text")
		if fake.resolve.calls != 0 || fake.sendCalls != 0 {
			t.Fatalf("calls = (resolve %d, send %d), want none", fake.resolve.calls, fake.sendCalls)
		}
		h.assertGenerationIntact(t, "text refusal")
	})
	t.Run("media", func(t *testing.T) {
		h := startSendRun(t, newLegacyClient)
		fake := &fakeMediaSendClient{
			conversationHook: h.emitAccountChange,
			uploadResult:     &gmproto.MediaContent{MediaID: "media-id"},
		}
		installMediaSendClient(t, h.host.GetClient(), fake)
		_, err := h.adapter.SendMedia(context.Background(), bridge.MediaRequest{
			Conversation: ref, Reader: strings.NewReader("x"), Size: 1, RequestID: "request-id",
		})
		requireAccountSwitchRefusal(t, requireMediaOpError(t, err), "send_media")
		if fake.resolve.calls != 0 || fake.uploadCalls != 0 || len(fake.sent) != 0 {
			t.Fatalf("calls = (resolve %d, upload %d, send %d), want none",
				fake.resolve.calls, fake.uploadCalls, len(fake.sent))
		}
		h.assertGenerationIntact(t, "media refusal")
	})
	t.Run("reaction", func(t *testing.T) {
		h := startSendRun(t, newLegacyClient)
		fake := &fakeReactionSendClient{conversationHook: h.emitAccountChange}
		installReactionSendClient(t, h.host.GetClient(), fake)
		_, err := h.adapter.SendReaction(context.Background(), bridge.ReactionRequest{
			Conversation: ref,
			Target:       bridge.MessageRef{RemoteID: "target-id"},
			Emoji:        "👍",
			Action:       bridge.ReactionAdd,
		})
		requireAccountSwitchRefusal(t, requireReactionOpError(t, err), "send_reaction")
		if fake.resolve.calls != 0 || fake.sendCalls != 0 {
			t.Fatalf("calls = (resolve %d, send %d), want none", fake.resolve.calls, fake.sendCalls)
		}
		h.assertGenerationIntact(t, "reaction refusal")
	})
}

// Design A2 step 1: a lookup error while the phone reports the switch is the
// refusal, unless the error itself indicts the session's credentials.
func TestLookupErrorWhileSwitchedIsRefusedUnlessAuthIndicting(t *testing.T) {
	t.Run("non-auth error while switched", func(t *testing.T) {
		h := startSendRun(t, newLegacyClient)
		fake := &fakeTextSendClient{
			conversationHook: h.emitAccountChange,
			conversationErr:  errors.New("response had no payload"),
		}
		installTextSendClient(t, h.host.GetClient(), fake)
		_, err := h.adapter.SendText(context.Background(), directTextRequest())
		failure := requireTextOpError(t, err)
		requireAccountSwitchRefusal(t, failure, "send_text")
		if !strings.Contains(err.Error(), "response had no payload") {
			t.Fatalf("refusal %q dropped the lookup error", err.Error())
		}
		h.assertGenerationIntact(t, "lookup error refusal")
	})

	t.Run("non-auth error without switch", func(t *testing.T) {
		h := startSendRun(t, newLegacyClient)
		fake := &fakeTextSendClient{conversationErr: errors.New("response had no payload")}
		installTextSendClient(t, h.host.GetClient(), fake)
		_, err := h.adapter.SendText(context.Background(), directTextRequest())
		failure := requireTextOpError(t, err)
		if failure.Class != bridge.FailureTransient || failure.Dispatch != bridge.DispatchNotCalled ||
			failure.Fingerprint != "google_conversation_get_failed" {
			t.Fatalf("failure = %+v, want transient not-dispatched google_conversation_get_failed", failure)
		}
		if fake.resolve.calls != 0 {
			t.Fatalf("a lookup error fell back to GetOrCreate %d times, want 0", fake.resolve.calls)
		}
		h.assertGenerationIntact(t, "lookup error")
	})

	t.Run("auth error while switched keeps auth classification", func(t *testing.T) {
		h := startSendRun(t, newLegacyClient)
		fake := &fakeTextSendClient{
			conversationHook: h.emitAccountChange,
			conversationErr:  errors.New("HTTP 401: invalid authentication credentials"),
		}
		installTextSendClient(t, h.host.GetClient(), fake)
		_, err := h.adapter.SendText(context.Background(), directTextRequest())
		failure := requireTextOpError(t, err)
		if failure.Class != bridge.FailureCredentialsExpired || failure.Fingerprint != "google_auth_expired" {
			t.Fatalf("failure = %+v, want credentials_expired google_auth_expired", failure)
		}
		terminalError(t, h.run.Done(), bridge.FailureCredentialsExpired)
	})
}

// Design A2 step 4: the by-number fallback for direct threads.
func TestSendFallbackResolvesDirectThreadByNumber(t *testing.T) {
	sim := &gmproto.SIMPayload{Two: 7, SIMNumber: 2}
	sameThread := directConversation(testRemoteID, "+1 (555) 123-4567")
	sameThread.Participants[0].SimPayload = sim

	tests := []struct {
		name        string
		resolve     fakeConversationResolve
		wantSend    bool
		class       bridge.FailureClass
		fingerprint string
		movedTo     string
	}{
		{
			name: "same thread sends with the resolved participant and SIM",
			resolve: fakeConversationResolve{result: &gmproto.GetOrCreateConversationResponse{
				Conversation: sameThread,
				Status:       gmproto.GetOrCreateConversationResponse_SUCCESS,
			}},
			wantSend: true,
		},
		{
			name: "moved thread asks for a rebind and sends nothing",
			resolve: fakeConversationResolve{result: &gmproto.GetOrCreateConversationResponse{
				Conversation: directConversation("3001", testPeerNumber),
			}},
			class:       bridge.FailureTransient,
			fingerprint: bridge.FingerprintConversationMoved,
			movedTo:     "3001",
		},
		{
			name: "group thread",
			resolve: fakeConversationResolve{result: &gmproto.GetOrCreateConversationResponse{
				Conversation: func() *gmproto.Conversation {
					conversation := directConversation(testRemoteID, testPeerNumber)
					conversation.IsGroupChat = true
					return conversation
				}(),
			}},
			class:       bridge.FailureMisconfigured,
			fingerprint: fingerprintConversationResolveMismatch,
		},
		{
			name: "two other participants",
			resolve: fakeConversationResolve{result: &gmproto.GetOrCreateConversationResponse{
				Conversation: func() *gmproto.Conversation {
					conversation := directConversation(testRemoteID, testPeerNumber)
					conversation.Participants = append(conversation.Participants, &gmproto.Participant{
						ID: &gmproto.SmallInfo{Number: "+15559876543"},
					})
					return conversation
				}(),
			}},
			class:       bridge.FailureMisconfigured,
			fingerprint: fingerprintConversationResolveMismatch,
		},
		{
			name: "no other participant",
			resolve: fakeConversationResolve{result: &gmproto.GetOrCreateConversationResponse{
				Conversation: &gmproto.Conversation{
					ConversationID: testRemoteID,
					Participants: []*gmproto.Participant{{
						ID:   &gmproto.SmallInfo{Number: testSelfNumber},
						IsMe: true,
					}},
				},
			}},
			class:       bridge.FailureMisconfigured,
			fingerprint: fingerprintConversationResolveMismatch,
		},
		{
			name: "different peer",
			resolve: fakeConversationResolve{result: &gmproto.GetOrCreateConversationResponse{
				Conversation: directConversation(testRemoteID, "+15559876543"),
			}},
			class:       bridge.FailureMisconfigured,
			fingerprint: fingerprintConversationResolveMismatch,
		},
		{
			name: "national-format peer is not the stored E.164 peer",
			resolve: fakeConversationResolve{result: &gmproto.GetOrCreateConversationResponse{
				Conversation: directConversation(testRemoteID, "(555) 123-4567"),
			}},
			class:       bridge.FailureMisconfigured,
			fingerprint: fingerprintConversationResolveMismatch,
		},
		{
			name: "conversation without an ID",
			resolve: fakeConversationResolve{result: &gmproto.GetOrCreateConversationResponse{
				Conversation: directConversation("", testPeerNumber),
			}},
			class:       bridge.FailureMisconfigured,
			fingerprint: fingerprintConversationResolveMismatch,
		},
		{
			name: "no conversation",
			resolve: fakeConversationResolve{result: &gmproto.GetOrCreateConversationResponse{
				Status: gmproto.GetOrCreateConversationResponse_UNKNOWN,
			}},
			class:       bridge.FailureTransient,
			fingerprint: fingerprintConversationNotFound,
		},
		{
			name:        "nil response",
			resolve:     fakeConversationResolve{},
			class:       bridge.FailureTransient,
			fingerprint: fingerprintConversationNotFound,
		},
		{
			name:        "transport error",
			resolve:     fakeConversationResolve{err: errors.New("lookup unavailable")},
			class:       bridge.FailureTransient,
			fingerprint: "google_conversation_get_failed",
		},
		{
			name:        "auth error",
			resolve:     fakeConversationResolve{err: errors.New("HTTP 401: invalid authentication credentials")},
			class:       bridge.FailureCredentialsExpired,
			fingerprint: "google_auth_expired",
		},
	}

	for _, kind := range []string{"text", "media"} {
		for _, test := range tests {
			t.Run(kind+"/"+test.name, func(t *testing.T) {
				var (
					err       error
					sent      []*gmproto.SendMessageRequest
					resolve   *fakeConversationResolve
					getCalls  int
					uploads   int
					operation = "send_" + kind
				)
				switch kind {
				case "text":
					fake := &fakeTextSendClient{
						resolve:    test.resolve,
						sendResult: &gmproto.SendMessageResponse{Status: gmproto.SendMessageResponse_SUCCESS},
					}
					a := newTextSendTestAdapter(t, fake)
					_, err = a.SendText(context.Background(), directTextRequest())
					if fake.sent != nil {
						sent = append(sent, fake.sent)
					}
					resolve, getCalls = &fake.resolve, fake.conversationCalls
				case "media":
					fake := &fakeMediaSendClient{
						resolve:      test.resolve,
						uploadResult: &gmproto.MediaContent{MediaID: "media-id"},
						sendResults:  []*gmproto.SendMessageResponse{{Status: gmproto.SendMessageResponse_SUCCESS}},
					}
					a := newMediaSendTestAdapter(t, fake)
					_, err = a.SendMedia(context.Background(), bridge.MediaRequest{
						Conversation: directTextRequest().Conversation,
						Reader:       strings.NewReader("x"),
						Size:         1,
						RequestID:    "request-id",
					})
					sent, uploads = fake.sent, fake.uploadCalls
					resolve, getCalls = &fake.resolve, fake.conversationCalls
				}

				if getCalls != 1 || resolve.calls != 1 {
					t.Fatalf("lookups = (GetConversation %d, GetOrCreate %d), want (1, 1)", getCalls, resolve.calls)
				}
				numbers := resolve.request.GetNumbers()
				if len(numbers) != 1 || numbers[0].GetNumber() != testPeerNumber ||
					numbers[0].GetNumber2() != testPeerNumber ||
					numbers[0].GetMysteriousInt() != app.ContactNumberMysteriousInt {
					t.Fatalf("GetOrCreate numbers = %+v, want the one stored peer via app.NewContactNumbers", numbers)
				}
				if test.wantSend {
					if err != nil {
						t.Fatalf("send error = %v, want success", err)
					}
					if len(sent) != 1 {
						t.Fatalf("SendMessage calls = %d, want 1", len(sent))
					}
					payload := sent[0]
					if payload.GetConversationID() != testRemoteID ||
						payload.GetMessagePayload().GetParticipantID() != testSelfNumber ||
						payload.GetSIMPayload() != sim {
						t.Fatalf("payload routing = (%q, %q, %p), want (%s, %s, %p)",
							payload.GetConversationID(), payload.GetMessagePayload().GetParticipantID(),
							payload.GetSIMPayload(), testRemoteID, testSelfNumber, sim)
					}
					return
				}
				failure, ok := asOpError(err)
				if !ok {
					t.Fatalf("send error = %T %v, want bridge.OpError", err, err)
				}
				if failure.Class != test.class || failure.Fingerprint != test.fingerprint ||
					failure.Dispatch != bridge.DispatchNotCalled || failure.Operation != operation {
					t.Fatalf("failure = %+v, want class %q fingerprint %q not dispatched",
						failure, test.class, test.fingerprint)
				}
				if len(sent) != 0 || uploads != 0 {
					t.Fatalf("calls = (send %d, upload %d), want none before a usable conversation",
						len(sent), uploads)
				}
				if test.movedTo != "" {
					var moved *bridge.ConversationMovedError
					if !errors.As(failure.Cause, &moved) ||
						moved.FromRemoteID != testRemoteID || moved.ToRemoteID != test.movedTo {
						t.Fatalf("cause = %#v, want ConversationMovedError{%s -> %s}",
							failure.Cause, testRemoteID, test.movedTo)
					}
				}
			})
		}
	}
}

// Design A2 step 4 preconditions: the fallback runs only for text and media
// sends to a direct thread with a stored peer number, after an empty lookup,
// and never while the phone reports the account switch.
func TestSendFallbackIsNotAttemptedOutsideItsPreconditions(t *testing.T) {
	tests := []struct {
		name        string
		ref         bridge.ConversationRef
		setFlag     bool
		cancelCtx   bool
		fingerprint string
	}{
		{
			name:        "group conversation",
			ref:         bridge.ConversationRef{RemoteID: testRemoteID, Kind: "group", DirectPeerNumber: testPeerNumber},
			fingerprint: fingerprintConversationNotFound,
		},
		{
			name:        "unknown kind",
			ref:         bridge.ConversationRef{RemoteID: testRemoteID, DirectPeerNumber: testPeerNumber},
			fingerprint: fingerprintConversationNotFound,
		},
		{
			name:        "broadcast conversation",
			ref:         bridge.ConversationRef{RemoteID: testRemoteID, Kind: "broadcast", DirectPeerNumber: testPeerNumber},
			fingerprint: fingerprintConversationNotFound,
		},
		{
			name:        "direct without a peer number",
			ref:         bridge.ConversationRef{RemoteID: testRemoteID, Kind: "direct"},
			fingerprint: fingerprintConversationNotFound,
		},
		{
			name:        "direct with a peer that has no digits",
			ref:         bridge.ConversationRef{RemoteID: testRemoteID, Kind: "direct", DirectPeerNumber: "+ ()"},
			fingerprint: fingerprintConversationNotFound,
		},
		{
			name:        "account switch already reported",
			ref:         directTextRequest().Conversation,
			setFlag:     true,
			fingerprint: fingerprintAccountPairingSwitched,
		},
		{
			name:        "context ended during the lookup",
			ref:         directTextRequest().Conversation,
			cancelCtx:   true,
			fingerprint: "google_text_context_done",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fake := &fakeTextSendClient{}
			a := newTextSendTestAdapter(t, fake)
			if test.setFlag {
				a.host.NoteGoogleAccountSwitch(a.host.GetClient(), testSwitchAccount)
			}
			if test.cancelCtx {
				fake.conversationHook = cancel
			}
			_, err := a.SendText(ctx, bridge.TextRequest{Conversation: test.ref, Body: "hello", RequestID: "request-id"})
			failure := requireTextOpError(t, err)
			if failure.Fingerprint != test.fingerprint || failure.Dispatch != bridge.DispatchNotCalled {
				t.Fatalf("failure = %+v, want not-dispatched %s", failure, test.fingerprint)
			}
			if fake.resolve.calls != 0 || fake.sendCalls != 0 {
				t.Fatalf("calls = (GetOrCreate %d, send %d), want none", fake.resolve.calls, fake.sendCalls)
			}
		})
	}

	t.Run("reactions never fall back", func(t *testing.T) {
		fake := &fakeReactionSendClient{
			resolve: fakeConversationResolve{result: &gmproto.GetOrCreateConversationResponse{
				Conversation: directConversation(testRemoteID, testPeerNumber),
			}},
		}
		a := newReactionSendTestAdapter(t, fake)
		_, err := a.SendReaction(context.Background(), bridge.ReactionRequest{
			Conversation: directTextRequest().Conversation,
			Target:       bridge.MessageRef{RemoteID: "target-id"},
			Emoji:        "👍",
			Action:       bridge.ReactionAdd,
		})
		failure := requireReactionOpError(t, err)
		if failure.Fingerprint != fingerprintConversationNotFound {
			t.Fatalf("failure = %+v, want %s", failure, fingerprintConversationNotFound)
		}
		if fake.resolve.calls != 0 || fake.sendCalls != 0 {
			t.Fatalf("calls = (GetOrCreate %d, send %d), want none", fake.resolve.calls, fake.sendCalls)
		}
	})
}

// Invariant I1, checked exhaustively over every lookup outcome, account-switch
// timing, conversation kind, peer, fallback outcome and operation: a send
// reaches the transport only after a real conversation for the stored remote
// ID or a validated by-number resolution of that same ID, and the fallback is
// asked only when its preconditions hold.
func TestSendNeverFollowsEmptyLookupWithoutValidatedSameThread(t *testing.T) {
	lookups := []string{"real", "empty", "error"}
	switchDuringLookup := []bool{false, true}
	kinds := []string{"direct", "group", ""}
	peers := []string{"", testPeerNumber}
	fallbacks := []string{"same", "moved", "group", "other_peer", "empty", "nil_response", "error"}

	textFake := &fakeTextSendClient{}
	textAdapter := newTextSendTestAdapter(t, textFake)
	mediaFake := &fakeMediaSendClient{}
	mediaAdapter := newMediaSendTestAdapter(t, mediaFake)
	reactionFake := &fakeReactionSendClient{}
	reactionAdapter := newReactionSendTestAdapter(t, reactionFake)

	cases := 0
	for _, op := range []string{"text", "media", "reaction"} {
		for _, lookup := range lookups {
			for _, raise := range switchDuringLookup {
				for _, kind := range kinds {
					for _, peer := range peers {
						for _, fallback := range fallbacks {
							cases++
							name := fmt.Sprintf("op=%s lookup=%s switch=%v kind=%q peer=%q fallback=%s",
								op, lookup, raise, kind, peer, fallback)
							ref := bridge.ConversationRef{RemoteID: testRemoteID, Kind: kind, DirectPeerNumber: peer}
							var conversation *gmproto.Conversation
							var conversationErr error
							switch lookup {
							case "real":
								conversation = directConversation(testRemoteID, testPeerNumber)
							case "error":
								conversationErr = errors.New("lookup unavailable")
							}
							resolve := scriptedFallback(fallback)

							var (
								host     *app.App
								err      error
								sends    int
								resolves int
								uploads  = -1
							)
							switch op {
							case "text":
								host = textAdapter.host
								host.ClearGoogleAccountSwitch()
								*textFake = fakeTextSendClient{
									conversationResult: conversation,
									conversationErr:    conversationErr,
									resolve:            resolve,
									sendResult:         &gmproto.SendMessageResponse{Status: gmproto.SendMessageResponse_SUCCESS},
								}
								if raise {
									textFake.conversationHook = func() { host.NoteGoogleAccountSwitch(host.GetClient(), testSwitchAccount) }
								}
								_, err = textAdapter.SendText(context.Background(), bridge.TextRequest{
									Conversation: ref, Body: "hello", RequestID: "request-id",
								})
								sends, resolves = textFake.sendCalls, textFake.resolve.calls
							case "media":
								host = mediaAdapter.host
								host.ClearGoogleAccountSwitch()
								*mediaFake = fakeMediaSendClient{
									conversationResult: conversation,
									conversationErr:    conversationErr,
									resolve:            resolve,
									uploadResult:       &gmproto.MediaContent{MediaID: "media-id"},
									sendResults:        []*gmproto.SendMessageResponse{{Status: gmproto.SendMessageResponse_SUCCESS}},
								}
								if raise {
									mediaFake.conversationHook = func() { host.NoteGoogleAccountSwitch(host.GetClient(), testSwitchAccount) }
								}
								_, err = mediaAdapter.SendMedia(context.Background(), bridge.MediaRequest{
									Conversation: ref, Reader: strings.NewReader("x"), Size: 1, RequestID: "request-id",
								})
								sends, resolves, uploads = len(mediaFake.sent), mediaFake.resolve.calls, mediaFake.uploadCalls
							case "reaction":
								host = reactionAdapter.host
								host.ClearGoogleAccountSwitch()
								*reactionFake = fakeReactionSendClient{
									conversationResult: conversation,
									conversationErr:    conversationErr,
									resolve:            resolve,
									sendResult:         &gmproto.SendReactionResponse{Success: true},
								}
								if raise {
									reactionFake.conversationHook = func() { host.NoteGoogleAccountSwitch(host.GetClient(), testSwitchAccount) }
								}
								_, err = reactionAdapter.SendReaction(context.Background(), bridge.ReactionRequest{
									Conversation: ref,
									Target:       bridge.MessageRef{RemoteID: "target-id"},
									Emoji:        "👍",
									Action:       bridge.ReactionAdd,
								})
								sends, resolves = reactionFake.sendCalls, reactionFake.resolve.calls
							}

							fallbackEligible := lookup == "empty" && !raise && kind == "direct" &&
								peer != "" && op != "reaction"
							wantSend := lookup == "real" || (fallbackEligible && fallback == "same")
							if (resolves > 0) != fallbackEligible {
								t.Fatalf("%s: GetOrCreate calls = %d, want eligible=%v", name, resolves, fallbackEligible)
							}
							if (sends > 0) != wantSend {
								t.Fatalf("%s: send calls = %d, want send=%v (err %v)", name, sends, wantSend, err)
							}
							if uploads >= 0 && uploads != sends {
								t.Fatalf("%s: uploads = %d, sends = %d; media must upload only for a usable conversation",
									name, uploads, sends)
							}
							if wantSend {
								if err != nil {
									t.Fatalf("%s: error = %v, want success", name, err)
								}
								continue
							}
							failure, ok := asOpError(err)
							if !ok || failure.Dispatch != bridge.DispatchNotCalled {
								t.Fatalf("%s: error = %v, want a not-dispatched OpError", name, err)
							}
							if raise && lookup != "real" && failure.Fingerprint != fingerprintAccountPairingSwitched {
								t.Fatalf("%s: fingerprint = %q, want the account-switch refusal", name, failure.Fingerprint)
							}
						}
					}
				}
			}
		}
	}
	if want := 3 * 3 * 2 * 3 * 2 * 7; cases != want {
		t.Fatalf("enumerated %d cases, want %d", cases, want)
	}
}

func scriptedFallback(outcome string) fakeConversationResolve {
	switch outcome {
	case "same":
		return fakeConversationResolve{result: &gmproto.GetOrCreateConversationResponse{
			Conversation: directConversation(testRemoteID, testPeerNumber),
		}}
	case "moved":
		return fakeConversationResolve{result: &gmproto.GetOrCreateConversationResponse{
			Conversation: directConversation("3001", testPeerNumber),
		}}
	case "group":
		conversation := directConversation(testRemoteID, testPeerNumber)
		conversation.IsGroupChat = true
		return fakeConversationResolve{result: &gmproto.GetOrCreateConversationResponse{Conversation: conversation}}
	case "other_peer":
		return fakeConversationResolve{result: &gmproto.GetOrCreateConversationResponse{
			Conversation: directConversation(testRemoteID, "+15559876543"),
		}}
	case "empty":
		return fakeConversationResolve{result: &gmproto.GetOrCreateConversationResponse{}}
	case "nil_response":
		return fakeConversationResolve{}
	case "error":
		return fakeConversationResolve{err: errors.New("lookup unavailable")}
	default:
		panic("unknown fallback outcome " + outcome)
	}
}

// Design A2: SendMedia resolves the conversation before UploadMedia so a
// lookup failure never re-uploads the file.
func TestSendMediaResolvesConversationBeforeUpload(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*fakeMediaSendClient)
		wantCalls []string
	}{
		{
			name: "stored conversation",
			configure: func(fake *fakeMediaSendClient) {
				fake.conversationResult = directConversation(testRemoteID, testPeerNumber)
			},
			wantCalls: []string{"get_conversation", "upload", "send"},
		},
		{
			name: "same thread by number",
			configure: func(fake *fakeMediaSendClient) {
				fake.resolve.result = &gmproto.GetOrCreateConversationResponse{
					Conversation: directConversation(testRemoteID, testPeerNumber),
				}
			},
			wantCalls: []string{"get_conversation", "get_or_create_conversation", "upload", "send"},
		},
		{
			name: "moved thread",
			configure: func(fake *fakeMediaSendClient) {
				fake.resolve.result = &gmproto.GetOrCreateConversationResponse{
					Conversation: directConversation("3001", testPeerNumber),
				}
			},
			wantCalls: []string{"get_conversation", "get_or_create_conversation"},
		},
		{
			name:      "no conversation",
			configure: func(*fakeMediaSendClient) {},
			wantCalls: []string{"get_conversation", "get_or_create_conversation"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := &fakeMediaSendClient{
				uploadResult: &gmproto.MediaContent{MediaID: "media-id"},
				sendResults:  []*gmproto.SendMessageResponse{{Status: gmproto.SendMessageResponse_SUCCESS}},
			}
			test.configure(fake)
			a := newMediaSendTestAdapter(t, fake)
			_, _ = a.SendMedia(context.Background(), bridge.MediaRequest{
				Conversation: directTextRequest().Conversation,
				Reader:       strings.NewReader("x"),
				Size:         1,
				RequestID:    "request-id",
			})
			if !reflect.DeepEqual(fake.calls, test.wantCalls) {
				t.Fatalf("transport calls = %v, want %v", fake.calls, test.wantCalls)
			}
		})
	}
}

// canonicalPhoneNumber properties over random input (testing/quick):
// idempotent, output is "" or an optional leading '+' followed by digits, and
// formatting characters never change the result.
func TestCanonicalPhoneNumberProperties(t *testing.T) {
	config := &quick.Config{MaxCount: 2000, Rand: rand.New(rand.NewSource(7))}

	shape := func(input string) bool {
		canonical := canonicalPhoneNumber(input)
		if canonicalPhoneNumber(canonical) != canonical {
			return false
		}
		if canonical == "" {
			return true
		}
		digits := strings.TrimPrefix(canonical, "+")
		if digits == "" {
			return false
		}
		for _, r := range digits {
			if r < '0' || r > '9' {
				return false
			}
		}
		return true
	}
	if err := quick.Check(shape, config); err != nil {
		t.Fatalf("canonical shape property failed: %v", err)
	}

	formatting := func(raw []uint8, seps []uint8, plus bool) bool {
		if len(raw) == 0 {
			return true
		}
		separators := []string{"", " ", "-", "(", ")", ".", " "}
		var digits, formatted strings.Builder
		if plus {
			formatted.WriteString("+")
		}
		for i, value := range raw {
			digit := byte('0' + value%10)
			digits.WriteByte(digit)
			formatted.WriteByte(digit)
			if i < len(seps) {
				formatted.WriteString(separators[int(seps[i])%len(separators)])
			}
		}
		want := digits.String()
		if plus {
			want = "+" + want
		}
		return canonicalPhoneNumber(" "+formatted.String()+" ") == want
	}
	if err := quick.Check(formatting, config); err != nil {
		t.Fatalf("formatting-insensitivity property failed: %v", err)
	}

	if got := canonicalPhoneNumber("+1 (555) 123-4567"); got != testPeerNumber {
		t.Fatalf("canonicalPhoneNumber(formatted) = %q, want %s", got, testPeerNumber)
	}
	if got := canonicalPhoneNumber("15551234567"); got == testPeerNumber {
		t.Fatal("a number without '+' must not canonicalise to the E.164 form")
	}
}

type sendRunHarness struct {
	host      *app.App
	transport *fakeTransport
	adapter   *Adapter
	run       bridge.Run
}

// startSendRun starts a real adapter generation over a fake transport with
// the legacy client newLegacy builds, and waits until it is ready.
func startSendRun(t *testing.T, newLegacy func(*testing.T) *client.Client) *sendRunHarness {
	t.Helper()
	host := newTestApp(t)
	transport := &fakeTransport{}
	a := New("google-primary", host, func() bool { return true })
	a.newClient = func() (*client.Client, transportClient, error) {
		return newLegacy(t), transport, nil
	}
	run, err := a.Start(context.Background(), bridge.StartRequest{
		AccountID:  "google-primary",
		Generation: 1,
	}, nil)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { stopRun(t, run) })
	transport.emit(&gmproto.Conversation{ConversationID: "ready"})
	<-run.Ready()
	return &sendRunHarness{host: host, transport: transport, adapter: a, run: run}
}

// emitAccountChange delivers the phone's account container through the live
// generation's event handler, as libgm does synchronously while decrypting
// the frame that answers a request.
func (h *sendRunHarness) emitAccountChange() {
	h.transport.emit(accountChangeEvent(testSwitchAccount))
}

func (h *sendRunHarness) sendWithResponse(
	t *testing.T,
	kind string,
	response *gmproto.SendMessageResponse,
) (error, int) {
	t.Helper()
	conversation := directConversation(testRemoteID, testPeerNumber)
	switch kind {
	case "text":
		fake := &fakeTextSendClient{conversationResult: conversation, sendResult: response}
		installTextSendClient(t, h.host.GetClient(), fake)
		_, err := h.adapter.SendText(context.Background(), bridge.TextRequest{
			Conversation: bridge.ConversationRef{RemoteID: testRemoteID},
			Body:         "hello",
			RequestID:    "request-id",
		})
		return err, fake.sendCalls
	case "media":
		fake := &fakeMediaSendClient{
			uploadResult:       &gmproto.MediaContent{MediaID: "media-id"},
			conversationResult: conversation,
			sendResults:        []*gmproto.SendMessageResponse{response},
		}
		installMediaSendClient(t, h.host.GetClient(), fake)
		_, err := h.adapter.SendMedia(context.Background(), bridge.MediaRequest{
			Conversation: bridge.ConversationRef{RemoteID: testRemoteID},
			Reader:       strings.NewReader("x"),
			Size:         1,
			RequestID:    "request-id",
		})
		return err, len(fake.sent)
	default:
		t.Fatalf("unknown send kind %q", kind)
		return nil, 0
	}
}

// assertGenerationIntact checks invariant I3: the receive generation is still
// running, Connected is unchanged and needs_repair is not set.
func (h *sendRunHarness) assertGenerationIntact(t *testing.T, context string) {
	t.Helper()
	if !h.host.Connected.Load() {
		t.Fatalf("%s marked the connected receive generation lost", context)
	}
	select {
	case terminal := <-h.run.Done():
		t.Fatalf("%s retired the receive generation with %v", context, terminal)
	default:
	}
	if h.host.GoogleStatus().NeedsRepair {
		t.Fatalf("%s set needs_repair", context)
	}
}

func requireAccountSwitchRefusal(t *testing.T, failure bridge.OpError, operation string) {
	t.Helper()
	if failure.Class != bridge.FailureReauthRequired ||
		failure.Fingerprint != fingerprintAccountPairingSwitched ||
		failure.Dispatch != bridge.DispatchNotCalled ||
		failure.Operation != operation {
		t.Fatalf("failure = %+v, want reauth_required %s not dispatched for %s",
			failure, fingerprintAccountPairingSwitched, operation)
	}
	var refusal *accountPairingSwitchedError
	if !errors.As(failure.Cause, &refusal) {
		t.Fatalf("cause = %T, want *accountPairingSwitchedError", failure.Cause)
	}
	text := failure.Error()
	for _, want := range []string{
		"[" + fingerprintAccountPairingSwitched + "]",
		testSwitchAccount,
		"Google-account pairing",
		"pair again by QR",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("refusal text %q does not contain %q", text, want)
		}
	}
}

func accountChangeEvent(account string) *events.AccountChange {
	return &events.AccountChange{
		AccountChangeOrSomethingEvent: &gmproto.AccountChangeOrSomethingEvent{Account: account},
		IsFake:                        true,
	}
}

func directTextRequest() bridge.TextRequest {
	return bridge.TextRequest{
		Conversation: bridge.ConversationRef{
			RemoteID:         testRemoteID,
			Kind:             "direct",
			DirectPeerNumber: testPeerNumber,
		},
		Body:      "hello",
		RequestID: "request-id",
	}
}

// directConversation is a 1:1 thread: self first, then the one peer.
func directConversation(id, peer string) *gmproto.Conversation {
	return &gmproto.Conversation{
		ConversationID: id,
		Participants: []*gmproto.Participant{
			{ID: &gmproto.SmallInfo{Number: testSelfNumber}, IsMe: true},
			{ID: &gmproto.SmallInfo{Number: peer}},
		},
	}
}

func newGoogleAccountLegacyClient(t *testing.T) *client.Client {
	t.Helper()
	legacy, err := client.NewFromSession(
		&client.SessionData{AuthDataJSON: []byte(`{"dest_reg_id":"6f1c2c1e-6b0a-4d4a-9a55-0d1b5b2f9c11"}`)},
		zerolog.Nop(),
	)
	if err != nil {
		t.Fatalf("NewFromSession() error = %v", err)
	}
	if !legacy.GM.AuthData.IsGoogleAccount() {
		t.Fatal("fixture is not a Google-account session")
	}
	return legacy
}
