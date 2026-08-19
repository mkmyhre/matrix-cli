package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"matrix-cli/internal/config"
	"matrix-cli/internal/matrix"
)

func TestNavigatorGroupsHomeAndSpaces(t *testing.T) {
	model := &chatModel{
		keys: config.Config{}, showIDs: false, navigator: true,
		rooms: []matrix.Room{
			{ID: "!home:test", Name: "Loose room"},
			{ID: "!space:test", Name: "Team"},
			{ID: "!child:test", Name: "General"},
		},
		spaces: []matrix.Space{{ID: "!space:test", Name: "Team", Children: []matrix.Room{{ID: "!child:test", Name: "General"}, {ID: "!not-joined:test", Name: "Public but not joined"}}}},
	}
	model.buildNavigator()
	if len(model.nav) != 2 {
		t.Fatalf("navigator entries = %d, want 2", len(model.nav))
	}
	if model.nav[0].room.ID != "!home:test" || model.nav[0].space.ID != "" {
		t.Fatalf("unexpected Home entry: %#v", model.nav[0])
	}
	if model.nav[1].room.ID != "!child:test" || model.nav[1].space.ID != "!space:test" {
		t.Fatalf("unexpected space entry: %#v", model.nav[1])
	}
	view := model.navigatorView()
	for _, text := range []string{"Home", "Loose room", "Team", "General"} {
		if !strings.Contains(view, text) {
			t.Errorf("navigator does not contain %q: %s", text, view)
		}
	}
	if strings.Contains(view, "!space:test") {
		t.Errorf("navigator showed ID despite having a name: %s", view)
	}
	if strings.Contains(view, "Public but not joined") {
		t.Errorf("navigator showed an unjoined space child: %s", view)
	}
}

func TestLoadingStatusUsesAnimatedSpinner(t *testing.T) {
	model := &chatModel{
		status:       "loading 10 older messages…",
		currentRoom:  "!room:test",
		loadingOlder: map[string]bool{"!room:test": true},
	}
	first := ansi.Strip(model.displayStatus())
	_, cmd := model.Update(loadingTickMsg{})
	second := ansi.Strip(model.displayStatus())
	if cmd == nil || first == second || !strings.Contains(second, "loading 10 older messages") {
		t.Fatalf("spinner did not advance: first=%q second=%q cmd=%v", first, second, cmd)
	}
}

func TestNavigatorScrollsToKeepSelectionVisible(t *testing.T) {
	rooms := make([]matrix.Room, 30)
	for i := range rooms {
		rooms[i] = matrix.Room{ID: fmt.Sprintf("!room%d:test", i), Name: fmt.Sprintf("Room %02d", i)}
	}
	model := &chatModel{keys: config.Config{}, navigator: true, width: 60, height: 12, rooms: rooms, navView: viewport.New(60, 7)}
	model.buildNavigator()
	model.navIndex = 0
	first := ansi.Strip(model.navigatorView())
	if !strings.Contains(first, "│ Room 00") || strings.Contains(first, "Room 29") {
		t.Fatalf("navigator did not show first selection: %s", first)
	}
	model.navIndex = len(model.nav) - 1
	last := ansi.Strip(model.navigatorView())
	if !strings.Contains(last, "│ Room 29") || strings.Contains(last, "Room 00") {
		t.Fatalf("navigator did not scroll to last selection: %s", last)
	}
}

func threadTestModel(cfg config.Config) *chatModel {
	return &chatModel{
		keys: cfg, width: 100, height: 30, mode: normalMode,
		view: viewport.New(40, 10), threadView: viewport.New(40, 10),
		currentRoom: "!room:test",
		roomInfo:    matrix.RoomInfo{Room: matrix.Room{ID: "!room:test", Name: "General"}, Spaces: []matrix.Room{{ID: "!space:test", Name: "Team"}}},
		messages: map[string][]matrix.Message{"!room:test": {
			{EventID: "$root", Sender: "@a:test", Body: "root"},
			{EventID: "$other", Sender: "@c:test", Body: "not in thread"},
			{EventID: "$reply", ThreadRoot: "$root", Sender: "@b:test", Body: "reply"},
		}},
		thread: "$root", rootSelection: 0, selection: 1,
	}
}

func TestThreadUsesFocusedViewByDefault(t *testing.T) {
	model := threadTestModel(config.Config{})
	model.refreshView()
	view := ansi.Strip(model.View())
	if !strings.Contains(view, "Team / General / thread") || !strings.Contains(view, "Thread") {
		t.Fatalf("thread context is not obvious: %s", view)
	}
	for _, text := range []string{"root", "reply"} {
		if !strings.Contains(view, text) {
			t.Errorf("focused thread missing %q: %s", text, view)
		}
	}
	if strings.Contains(view, "not in thread") {
		t.Fatalf("focused thread still shows the room timeline: %s", view)
	}
}

func TestMinimalThemeIsDefaultAndBoxedCanBeEnabled(t *testing.T) {
	minimal := threadTestModel(config.Config{})
	minimal.refreshView()
	if view := ansi.Strip(minimal.View()); strings.Contains(view, "╭") || !strings.Contains(view, "──") {
		t.Fatalf("unexpected minimal theme: %s", view)
	}
	boxed := threadTestModel(config.Config{Theme: config.ThemeBoxed})
	boxed.refreshView()
	if view := ansi.Strip(boxed.View()); !strings.Contains(view, "╭") {
		t.Fatalf("boxed theme has no border: %s", view)
	}
}

func TestThreadSplitViewCanBeEnabled(t *testing.T) {
	model := threadTestModel(config.Config{ThreadView: config.ThreadViewSplit})
	model.refreshView()
	view := ansi.Strip(model.View())
	for _, text := range []string{"Room", "Thread", "root", "reply", "not in thread"} {
		if !strings.Contains(view, text) {
			t.Errorf("split view missing %q: %s", text, view)
		}
	}
}

func TestMessageRenderingShortensSenderAndShowsTime(t *testing.T) {
	output := renderMessages([]matrix.Message{{Sender: "@alice:example.com", Body: "hello", Timestamp: time.Date(2025, 1, 1, 12, 34, 0, 0, time.Local)}}, nil, config.ReactionsOff, nil, 0, true, 80)
	if !strings.Contains(output, "12:34") || !strings.Contains(output, "alice") || strings.Contains(output, "example.com") {
		t.Fatalf("unexpected rendered message: %s", output)
	}
}

func TestMessageRenderingWrapsInsteadOfClippingInThreadPane(t *testing.T) {
	const width = 32
	body := "A long thread reply with an unbroken-value-abcdefghijklmnopqrstuvwxyz0123456789 that must remain readable"
	output := renderMessages([]matrix.Message{{Sender: "@alice:example.com", Body: body}}, nil, config.ReactionsOff, nil, 0, true, width)
	for i, line := range strings.Split(output, "\n") {
		if got := ansi.StringWidth(line); got > width {
			t.Errorf("line %d width = %d, want <= %d: %q", i, got, width, ansi.Strip(line))
		}
	}
	if !strings.Contains(ansi.Strip(output), "must remain readable") {
		t.Fatalf("end of wrapped message is missing: %s", output)
	}
}

func TestReactionRenderingCanBeLimitedOrDisabled(t *testing.T) {
	reactions := map[string][]matrix.Reaction{"$message": {
		{EventID: "$1", Key: "👍"}, {EventID: "$2", Key: "👍"}, {EventID: "$3", Key: "❤️"}, {EventID: "$4", Key: "🎉"}, {EventID: "$5", Key: "👀"},
	}}
	messages := []matrix.Message{{EventID: "$message", Body: "hello"}}
	limited := ansi.Strip(renderMessages(messages, reactions, config.ReactionsLimited, nil, 0, true, 80))
	if !strings.Contains(limited, "👍 2") || !strings.Contains(limited, "❤️ 1") || !strings.Contains(limited, "🎉 1") || strings.Contains(limited, "👀 1") {
		t.Fatalf("limited reactions = %q", limited)
	}
	full := ansi.Strip(renderMessages(messages, reactions, config.ReactionsFull, nil, 0, true, 80))
	if !strings.Contains(full, "👀 1") {
		t.Fatalf("full reactions = %q", full)
	}
	off := ansi.Strip(renderMessages(messages, reactions, config.ReactionsOff, nil, 0, true, 80))
	if strings.Contains(off, "👍 2") {
		t.Fatalf("disabled reactions = %q", off)
	}
}

func TestFetchHistoryRetainsReactions(t *testing.T) {
	client := &sendTestClient{page: matrix.MessagePage{
		Messages:  []matrix.Message{{EventID: "$message", Body: "hello"}},
		Reactions: []matrix.Reaction{{EventID: "$reaction", TargetEventID: "$message", Key: "👍"}},
	}}
	model := &chatModel{ctx: context.Background(), client: client}
	page, err := model.fetchHistory("!room:test", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || len(page.Reactions) != 1 || page.Reactions[0].Key != "👍" {
		t.Fatalf("page = %#v", page)
	}
}

type sendTestClient struct {
	err   error
	calls int
	page  matrix.MessagePage
}

func (*sendTestClient) Rooms(context.Context) ([]matrix.Room, error)   { return nil, nil }
func (*sendTestClient) Spaces(context.Context) ([]matrix.Space, error) { return nil, nil }
func (*sendTestClient) RoomInfo(context.Context, string) (matrix.RoomInfo, error) {
	return matrix.RoomInfo{}, nil
}
func (c *sendTestClient) RecentMessages(context.Context, string, string, int) (matrix.MessagePage, error) {
	return c.page, nil
}
func (c *sendTestClient) Send(context.Context, string, string) (string, error) {
	c.calls++
	if c.err != nil {
		return "", c.err
	}
	return "$sent", nil
}
func (c *sendTestClient) SendThread(context.Context, string, string, string) (string, error) {
	return c.Send(context.Background(), "", "")
}
func (*sendTestClient) SendReaction(context.Context, string, string, string) (string, error) {
	return "", nil
}
func (*sendTestClient) Subscribe(context.Context, string) (<-chan matrix.TimelineEvent, <-chan error) {
	return nil, nil
}
func (*sendTestClient) Logout(context.Context) error { return nil }

func runSentCommand(t *testing.T, cmd tea.Cmd) sentMsg {
	t.Helper()
	message := cmd()
	if sent, ok := message.(sentMsg); ok {
		return sent
	}
	if batch, ok := message.(tea.BatchMsg); ok {
		for _, batched := range batch {
			if result := batched(); result != nil {
				if sent, ok := result.(sentMsg); ok {
					return sent
				}
			}
		}
	}
	t.Fatalf("command returned no sentMsg: %T", message)
	return sentMsg{}
}

func TestSendIsOptimisticAndFailedMessageCanBeRetried(t *testing.T) {
	client := &sendTestClient{err: errors.New("offline")}
	model := threadTestModel(config.Config{})
	model.client = client
	model.ctx = context.Background()
	model.thread = ""
	model.messages[model.currentRoom] = nil
	model.delivery = make(map[string]deliveryState)
	model.input.SetValue("hello")

	cmd := model.sendCurrent()
	visible := model.visibleMessages()
	if cmd == nil || len(visible) != 1 || visible[0].Body != "hello" || model.delivery[visible[0].EventID] != deliverySending {
		t.Fatalf("message was not shown optimistically: messages=%#v delivery=%#v", visible, model.delivery)
	}
	localID := visible[0].EventID
	_, _ = model.Update(runSentCommand(t, cmd))
	if model.delivery[localID] != deliveryFailed || !strings.Contains(model.status, "retry") {
		t.Fatalf("failed send state=%v status=%q", model.delivery[localID], model.status)
	}
	if model.input.Value() != "" || model.messages[model.currentRoom][0].Body != "hello" {
		t.Fatal("failed message text was not retained in the timeline")
	}

	client.err = nil
	retry := model.retrySelected()
	if retry == nil || model.delivery[localID] != deliverySending {
		t.Fatal("failed message could not be retried")
	}
	_, _ = model.Update(runSentCommand(t, retry))
	message := model.messages[model.currentRoom][0]
	if message.EventID != "$sent" || model.delivery[message.EventID] != deliverySent || client.calls != 2 {
		t.Fatalf("retry result: message=%#v delivery=%#v calls=%d", message, model.delivery, client.calls)
	}
}

func TestIncomingEchoConfirmsOptimisticMessageWithoutDuplicate(t *testing.T) {
	model := threadTestModel(config.Config{})
	model.messages[model.currentRoom] = []matrix.Message{{RoomID: model.currentRoom, EventID: "$sent", Body: "hello"}}
	model.delivery = map[string]deliveryState{"$sent": deliverySent}
	model.storeIncoming(matrix.Message{RoomID: model.currentRoom, EventID: "$sent", Sender: "@me:test", Body: "hello"})
	if len(model.messages[model.currentRoom]) != 1 || model.delivery["$sent"] != 0 {
		t.Fatalf("sync echo was duplicated: messages=%#v delivery=%#v", model.messages[model.currentRoom], model.delivery)
	}
}

func TestAccountPickerSwitchesAndSetsDefault(t *testing.T) {
	var defaultAccount string
	model := &chatModel{
		keys: config.Config{}, accountName: "dev", accountPicker: true, accountIndex: 1,
		accounts:          []AccountOption{{Name: "dev", Color: "#ff0000", Default: true}, {Name: "prod", Color: "#00ff00"}},
		setDefaultAccount: func(name string) error { defaultAccount = name; return nil },
	}
	view := ansi.Strip(model.accountPickerView())
	for _, expected := range []string{"Accounts", "dev", "prod", "current", "default"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("account picker missing %q: %s", expected, view)
		}
	}
	_, _ = model.updateAccountPicker(model.keys.Key("set_default_account"))
	if defaultAccount != "prod" || !model.accounts[1].Default || model.accounts[0].Default {
		t.Fatalf("default was not updated: %q, %#v", defaultAccount, model.accounts)
	}
	_, cmd := model.updateAccountPicker(model.keys.Key("open_thread"))
	if model.requested.Account != "prod" || cmd == nil {
		t.Fatalf("account switch = %q, cmd=%v", model.requested.Account, cmd)
	}
}

func TestBackgroundAccountErrorIsVisible(t *testing.T) {
	notifications := make(chan AccountNotification)
	model := &chatModel{
		accounts:             []AccountOption{{Name: "prod"}},
		accountNotifications: notifications,
	}
	_, cmd := model.Update(accountNotificationMsg{ok: true, notification: AccountNotification{Account: "prod", Error: "session expired"}})
	if model.accounts[0].Error != "session expired" || !strings.Contains(model.status, "prod sync error: session expired") {
		t.Fatalf("account=%#v status=%q", model.accounts[0], model.status)
	}
	if cmd == nil {
		t.Fatal("notification listener was not continued")
	}
}

func TestNotificationInboxNavigatesAcrossAccounts(t *testing.T) {
	model := &chatModel{
		keys: config.Config{}, accountName: "dev", notificationPicker: true,
		accounts:      []AccountOption{{Name: "dev", UserID: "@me:dev"}, {Name: "prod", Color: "#ff0000", UserID: "@me:prod"}},
		accountUnread: map[string]int{"prod": 1},
		notifications: []AccountNotification{{
			Account: "prod", Color: "#ff0000", RoomName: "Incidents",
			Message: matrix.Message{RoomID: "!incident:prod", Sender: "@alice:prod", Body: "deployment failed", Timestamp: time.Date(2026, 1, 1, 12, 41, 0, 0, time.Local)},
		}},
	}
	view := ansi.Strip(model.notificationPickerView())
	for _, expected := range []string{"Notifications · 1", "prod / Incidents", "alice", "deployment failed"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("notification inbox missing %q: %s", expected, view)
		}
	}
	_, cmd := model.updateNotificationPicker(model.keys.Key("open_thread"))
	if cmd == nil || model.requested.Account != "prod" || model.requested.Room != "!incident:prod" {
		t.Fatalf("destination = %#v, cmd=%v", model.requested, cmd)
	}
	if len(model.notifications) != 0 || model.accountUnread["prod"] != 0 {
		t.Fatalf("notification was not cleared: %#v, %#v", model.notifications, model.accountUnread)
	}
}

func TestNotificationInboxDiscardsSelectedNotification(t *testing.T) {
	model := &chatModel{
		keys: config.Config{}, notificationPicker: true, notificationIndex: 1,
		accountUnread: map[string]int{"dev": 1, "prod": 2},
		notifications: []AccountNotification{
			{Account: "dev", Message: matrix.Message{RoomID: "!dev:test", Body: "keep"}},
			{Account: "prod", Message: matrix.Message{RoomID: "!first:prod", Body: "discard"}},
			{Account: "prod", Message: matrix.Message{RoomID: "!second:prod", Body: "keep"}},
		},
	}
	_, cmd := model.updateNotificationPicker(model.keys.Key("discard_notification"))
	if cmd != nil || len(model.notifications) != 2 || model.notifications[1].Message.Body != "keep" {
		t.Fatalf("notifications = %#v, cmd=%v", model.notifications, cmd)
	}
	if model.accountUnread["prod"] != 1 || model.accountUnread["dev"] != 1 || model.notificationIndex != 1 {
		t.Fatalf("unread=%#v index=%d", model.accountUnread, model.notificationIndex)
	}
	if !strings.Contains(ansi.Strip(model.notificationPickerView()), "d discard") {
		t.Fatal("notification footer does not show discard binding")
	}
}

func TestBackgroundOwnMessageIsNotANotification(t *testing.T) {
	channel := make(chan AccountNotification)
	model := &chatModel{
		keys: config.Config{}, accountNotifications: channel, accountUnread: make(map[string]int),
		accounts: []AccountOption{{Name: "prod", UserID: "@me:prod"}},
	}
	_, _ = model.Update(accountNotificationMsg{ok: true, notification: AccountNotification{
		Account: "prod", Message: matrix.Message{RoomID: "!room:prod", Sender: "@me:prod", Body: "mine"},
	}})
	if len(model.notifications) != 0 || model.accountUnread["prod"] != 0 {
		t.Fatalf("own message created a notification: %#v", model.notifications)
	}
}

func TestNavigatorShowsUnreadCount(t *testing.T) {
	model := &chatModel{keys: config.Config{}, navigator: true, width: 80, unread: map[string]int{"!room:test": 3}, rooms: []matrix.Room{{ID: "!room:test", Name: "Room"}}}
	model.buildNavigator()
	if view := model.navigatorView(); !strings.Contains(view, "(3)") {
		t.Fatalf("missing unread count: %s", view)
	}
}

func TestOpeningRoomReplacesStaleHistoryWithLatestPage(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	model := &chatModel{
		messages: map[string][]matrix.Message{"!room:test": {
			{EventID: "$old-1", Body: "old 1", Timestamp: base},
			{EventID: "$old-2", Body: "old 2", Timestamp: base.Add(time.Minute)},
		}},
		nextPage: make(map[string]string), unread: make(map[string]int),
		view: viewport.New(80, 20), threadView: viewport.New(40, 20),
	}
	latest := []matrix.Message{
		{EventID: "$new-1", Body: "new 1", Timestamp: base.Add(2 * time.Minute)},
		{EventID: "$new-2", Body: "new 2", Timestamp: base.Add(3 * time.Minute)},
	}
	_, _ = model.Update(roomLoadedMsg{
		info:           matrix.RoomInfo{Room: matrix.Room{ID: "!room:test"}},
		page:           matrix.MessagePage{Messages: latest, Next: "next"},
		previousEvents: map[string]bool{"$old-1": true, "$old-2": true},
	})

	got := model.messages["!room:test"]
	if len(got) != 2 || got[0].EventID != "$new-1" || got[1].EventID != "$new-2" {
		t.Fatalf("room history = %#v, want only latest page", got)
	}
	if model.selection != 1 {
		t.Fatalf("selection = %d, want newest message", model.selection)
	}
}

func TestOpeningRoomPreservesLiveMessageReceivedDuringHistoryFetch(t *testing.T) {
	recent := []matrix.Message{{EventID: "$recent"}}
	cached := []matrix.Message{
		{EventID: "$stale"},
		{EventID: "$recent"},
		// A live event can have a missing or skewed timestamp, so request-time
		// event IDs rather than timestamps determine whether it is retained.
		{EventID: "$live"},
	}
	got := replaceWithRecentMessages(recent, cached, map[string]bool{"$stale": true, "$recent": true})
	if len(got) != 2 || got[0].EventID != "$recent" || got[1].EventID != "$live" {
		t.Fatalf("replaced history = %#v", got)
	}
}

func TestOpeningEmptyRoomHistoryStillPreservesLiveMessage(t *testing.T) {
	got := replaceWithRecentMessages(nil, []matrix.Message{{EventID: "$old"}, {EventID: "$live"}}, map[string]bool{"$old": true})
	if len(got) != 1 || got[0].EventID != "$live" {
		t.Fatalf("replaced history = %#v, want live event", got)
	}
}

func TestStaleRoomLoadCannotReplaceNewerNavigation(t *testing.T) {
	model := &chatModel{
		roomRequestVersion: 2,
		currentRoom:        "!new:test",
		messages:           map[string][]matrix.Message{"!new:test": {{EventID: "$new"}}},
	}
	_, _ = model.Update(roomLoadedMsg{
		requestVersion: 1,
		info:           matrix.RoomInfo{Room: matrix.Room{ID: "!old:test"}},
		page:           matrix.MessagePage{Messages: []matrix.Message{{EventID: "$old"}}},
	})
	if model.currentRoom != "!new:test" || len(model.messages["!old:test"]) != 0 {
		t.Fatalf("stale load changed room state: current=%q messages=%#v", model.currentRoom, model.messages)
	}
}

func TestHelpShowsEffectiveBindings(t *testing.T) {
	model := &chatModel{keys: config.Config{Keybindings: map[string]string{"move_down": "down"}}}
	view := model.helpView()
	if !strings.Contains(view, "move down") || !strings.Contains(view, "down") {
		t.Fatalf("unexpected help: %s", view)
	}
}

func TestNavigatorCanToggleIDs(t *testing.T) {
	model := &chatModel{keys: config.Config{}, navigator: true, rooms: []matrix.Room{{ID: "!room:test", Name: "Room"}}}
	model.buildNavigator()
	_, _ = model.updateNavigator(model.keys.Key("toggle_identifiers"))
	if !strings.Contains(model.navigatorView(), "!room:test") {
		t.Fatal("ID toggle did not show room ID")
	}
}
