package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
