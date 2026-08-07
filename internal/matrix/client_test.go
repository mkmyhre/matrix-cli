package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func TestValidateSessionChecksTokenAndIdentity(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		wantInvalid bool
	}{
		{name: "valid", status: http.StatusOK, body: `{"user_id":"@me:test","device_id":"DEV"}`},
		{name: "unknown token", status: http.StatusUnauthorized, body: `{"errcode":"M_UNKNOWN_TOKEN","error":"unknown"}`, wantInvalid: true},
		{name: "wrong user", status: http.StatusOK, body: `{"user_id":"@other:test","device_id":"DEV"}`, wantInvalid: true},
		{name: "wrong device", status: http.StatusOK, body: `{"user_id":"@me:test","device_id":"OTHER"}`, wantInvalid: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/_matrix/client/v3/account/whoami" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client, err := New(server.URL, "@me:test", "DEV", "token")
			if err != nil {
				t.Fatal(err)
			}
			err = client.ValidateSession(context.Background())
			if errors.Is(err, ErrInvalidSession) != tc.wantInvalid {
				t.Fatalf("error = %v, invalid = %v", err, tc.wantInvalid)
			}
		})
	}
}

func TestRoomsCalculateUnnamedDirectMessageName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/joined_rooms"):
			_, _ = w.Write([]byte(`{"joined_rooms":["!dm:test"]}`))
		case strings.Contains(r.URL.Path, "/state/m.room.name"):
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.Path, "/joined_members"):
			_, _ = w.Write([]byte(`{"joined":{"@me:test":{"display_name":"Me"},"@alice:test":{"display_name":"Alice"}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, "@me:test", "DEV", "token")
	if err != nil {
		t.Fatal(err)
	}
	rooms, err := client.Rooms(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rooms) != 1 || rooms[0].Name != "Alice" {
		t.Fatalf("rooms = %#v", rooms)
	}
}

func TestSpacesExcludeAccessibleButUnjoinedChildren(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/joined_rooms"):
			_, _ = w.Write([]byte(`{"joined_rooms":["!space:test","!joined:test"]}`))
		case strings.Contains(r.URL.Path, "/hierarchy"):
			_, _ = w.Write([]byte(`{"rooms":[
				{"room_id":"!space:test","name":"Team","children_state":[
					{"type":"m.space.child","state_key":"!joined:test","content":{"via":["test"]}},
					{"type":"m.space.child","state_key":"!public:test","content":{"via":["test"]}}
				]},
				{"room_id":"!joined:test","name":"Joined"},
				{"room_id":"!public:test","name":"Public but not joined"}
			]}`))
		case strings.Contains(r.URL.Path, "/state/m.room.create"):
			if strings.Contains(r.URL.Path, "!space:test") {
				_, _ = w.Write([]byte(`{"type":"m.space"}`))
			} else {
				_, _ = w.Write([]byte(`{}`))
			}
		case strings.Contains(r.URL.Path, "/state/m.room.name"):
			_, _ = w.Write([]byte(`{"name":"Team"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := New(server.URL, "@me:test", "DEV", "token")
	if err != nil {
		t.Fatal(err)
	}
	spaces, err := client.Spaces(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(spaces) != 1 || len(spaces[0].Children) != 1 || spaces[0].Children[0].ID != "!joined:test" {
		t.Fatalf("spaces = %#v", spaces)
	}
}

func TestMessageFromEvent(t *testing.T) {
	evt := &event.Event{
		Type: event.EventMessage, RoomID: id.RoomID("!room:test"), Sender: id.UserID("@alice:test"), ID: id.EventID("$event"), Timestamp: 1234,
		Content: event.Content{Parsed: &event.MessageEventContent{MsgType: event.MsgText, Body: "hello"}},
	}
	got, ok := MessageFromEvent(evt)
	if !ok {
		t.Fatal("text message was rejected")
	}
	if got.RoomID != "!room:test" || got.Sender != "@alice:test" || got.Body != "hello" || got.EventID != "$event" {
		t.Fatalf("unexpected message: %#v", got)
	}
	if !got.Timestamp.Equal(time.UnixMilli(1234)) {
		t.Fatalf("timestamp = %s", got.Timestamp)
	}
}

type historyCrypto struct{ sawSession bool }

func (*historyCrypto) Init(context.Context) error { return nil }
func (c *historyCrypto) Decrypt(_ context.Context, evt *event.Event) (*event.Event, error) {
	c.sawSession = evt.Content.AsEncrypted().SessionID == id.SessionID("SESSION")
	return &event.Event{
		Type: event.EventMessage, RoomID: evt.RoomID, Sender: evt.Sender, ID: evt.ID, Timestamp: evt.Timestamp,
		Content: event.Content{Parsed: &event.MessageEventContent{MsgType: event.MsgText, Body: "decrypted"}},
	}, nil
}
func (*historyCrypto) Verify(context.Context, io.Reader, io.Writer) error { return nil }

func TestRecentMessagesParsesEncryptedContentBeforeDecrypting(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"end":"next","chunk":[{
			"type":"m.room.encrypted","room_id":"!room:test","event_id":"$encrypted","sender":"@a:test",
			"content":{"algorithm":"m.megolm.v1.aes-sha2","ciphertext":"abc","device_id":"DEV","sender_key":"key","session_id":"SESSION"}
		}]}`))
	}))
	defer server.Close()
	client, err := New(server.URL, "@me:test", "DEV", "token")
	if err != nil {
		t.Fatal(err)
	}
	crypto := &historyCrypto{}
	client.crypto = crypto
	page, err := client.RecentMessages(context.Background(), "!room:test", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !crypto.sawSession || len(page.Messages) != 1 || page.Messages[0].Body != "decrypted" {
		t.Fatalf("encrypted history was not parsed and decrypted: saw=%v page=%#v", crypto.sawSession, page)
	}
}

func TestRecentMessagesReturnsParsedPageInChronologicalOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("from"); got != "page1" {
			t.Errorf("from = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"start":"page1","end":"page2","chunk":[
			{"type":"m.room.message","event_id":"$new","sender":"@a:test","content":{"msgtype":"m.text","body":"new"}},
			{"type":"m.room.message","event_id":"$old","sender":"@a:test","content":{"msgtype":"m.text","body":"old"}}
		]}`))
	}))
	defer server.Close()
	client, err := New(server.URL, "@me:test", "DEV", "token")
	if err != nil {
		t.Fatal(err)
	}
	page, err := client.RecentMessages(context.Background(), "!room:test", "page1", 30)
	if err != nil {
		t.Fatal(err)
	}
	if page.Next != "page2" || len(page.Messages) != 2 {
		t.Fatalf("unexpected page: %#v", page)
	}
	if page.Messages[0].Body != "old" || page.Messages[1].Body != "new" {
		t.Fatalf("messages not chronological: %#v", page.Messages)
	}
}

func TestSubscribeSkipsInitialTimeline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_ = json.NewEncoder(w).Encode(map[string]any{"filter_id": "filter"})
			return
		}
		body := "old"
		next := "batch1"
		if r.URL.Query().Get("since") == "batch1" {
			body, next = "new", "batch2"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"next_batch": next,
			"rooms": map[string]any{"join": map[string]any{"!room:test": map[string]any{
				"timeline": map[string]any{"events": []any{map[string]any{
					"type": "m.room.message", "event_id": "$" + body, "sender": "@alice:test",
					"origin_server_ts": 1234, "content": map[string]any{"msgtype": "m.text", "body": body},
				}}},
			}}},
		})
	}))
	defer server.Close()

	client, err := New(server.URL, "@me:test", "DEV", "token")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	messages, errs := client.Subscribe(ctx, "!room:test")
	select {
	case msg := <-messages:
		if msg.Body != "new" {
			t.Fatalf("received %q, expected initial event to be skipped", msg.Body)
		}
		cancel()
	case err := <-errs:
		t.Fatalf("sync failed: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for message")
	}
}

func TestMessageFromEventParsesRawHistoryContent(t *testing.T) {
	evt := &event.Event{
		Type: event.EventMessage, RoomID: id.RoomID("!room:test"),
		Content: event.Content{Raw: map[string]any{"msgtype": "m.text", "body": "from history"}},
	}
	got, ok := MessageFromEvent(evt)
	if !ok || got.Body != "from history" {
		t.Fatalf("message = %#v, ok = %v", got, ok)
	}
}

func TestUndecryptableMessagePreservesHistoryMetadata(t *testing.T) {
	evt := &event.Event{
		Type: event.EventEncrypted, RoomID: id.RoomID("!room:test"), Sender: id.UserID("@alice:test"),
		ID: id.EventID("$encrypted"), Timestamp: 1234,
	}
	got := undecryptableMessage(evt)
	if got.RoomID != "!room:test" || got.Sender != "@alice:test" || got.EventID != "$encrypted" {
		t.Fatalf("placeholder metadata = %#v", got)
	}
	if !strings.Contains(got.Body, "Unable to decrypt") || !got.Timestamp.Equal(time.UnixMilli(1234)) {
		t.Fatalf("placeholder content = %#v", got)
	}
}

func TestMessageFromEventIncludesThreadRoot(t *testing.T) {
	relation := &event.RelatesTo{}
	relation.SetThread(id.EventID("$root"), id.EventID("$previous"))
	evt := &event.Event{
		Type: event.EventMessage, RoomID: id.RoomID("!room:test"), ID: id.EventID("$reply"),
		Content: event.Content{Parsed: &event.MessageEventContent{MsgType: event.MsgText, Body: "reply", RelatesTo: relation}},
	}
	got, ok := MessageFromEvent(evt)
	if !ok || got.ThreadRoot != "$root" {
		t.Fatalf("thread root = %q, ok = %v", got.ThreadRoot, ok)
	}
}

func TestMessageFromEventFiltersNonText(t *testing.T) {
	for _, tc := range []struct {
		name string
		evt  *event.Event
	}{
		{"nil", nil},
		{"wrong type", &event.Event{Type: event.EventReaction}},
		{"image", &event.Event{Type: event.EventMessage, Content: event.Content{Parsed: &event.MessageEventContent{MsgType: event.MsgImage, Body: "image"}}}},
		{"empty", &event.Event{Type: event.EventMessage, Content: event.Content{Parsed: &event.MessageEventContent{MsgType: event.MsgText}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := MessageFromEvent(tc.evt); ok {
				t.Fatal("event was not filtered")
			}
		})
	}
}
