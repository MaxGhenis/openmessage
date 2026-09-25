package web

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/maxghenis/openmessage/internal/contactsync"
	"github.com/maxghenis/openmessage/internal/db"
)

func TestContactDirectoryRefreshReachesAutocompleteAndExistingThreads(t *testing.T) {
	var ts *testServer
	state := map[string]any{"complete": false}
	ts = newTestServerWithOptions(t, APIOptions{
		ContactSyncStatus: func() any { return state },
		SyncGoogleContacts: func() (int, error) {
			n, _, err := ts.store.ReplaceContactDirectory([]contactsync.Person{
				{ResourceName: "people/alice", Names: []contactsync.Name{{DisplayName: "Alice Directory"}}, PhoneNumbers: []contactsync.Value{{Value: "+12025550101"}}},
				{ResourceName: "people/new", Names: []contactsync.Name{{DisplayName: "New Person"}}, PhoneNumbers: []contactsync.Value{{Value: "+12025550102"}, {Value: "+12025550103"}}},
			})
			state = map[string]any{"complete": err == nil, "people": 2, "phone_entries": n}
			return n, err
		},
	})
	if err := ts.store.UpsertConversation(&db.Conversation{ConversationID: "existing", Name: "+12025550101", Participants: `[{"number":"+12025550101","name":"+12025550101"}]`, LastMessageTS: 1000}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(ts.server.URL+"/api/contacts/sync", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	for query, want := range map[string]int{"Alice": 1, "New": 2} {
		resp, err = http.Get(ts.server.URL + "/api/contacts?q=" + query)
		if err != nil {
			t.Fatal(err)
		}
		var cs []db.Contact
		json.NewDecoder(resp.Body).Decode(&cs)
		resp.Body.Close()
		if len(cs) != want {
			t.Fatalf("%s: %+v", query, cs)
		}
	}
	cs, err := ts.store.ListConversations(10)
	if err != nil || len(cs) != 1 || cs[0].Name != "Alice Directory" {
		t.Fatalf("%+v %v", cs, err)
	}
	resp, err = http.Get(ts.server.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var status map[string]json.RawMessage
	json.NewDecoder(resp.Body).Decode(&status)
	var sync map[string]any
	if json.Unmarshal(status["contact_sync"], &sync) != nil || sync["complete"] != true {
		t.Fatalf("%s", status["contact_sync"])
	}
}
