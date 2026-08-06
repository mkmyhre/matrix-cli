package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
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
		spaces: []matrix.Space{{ID: "!space:test", Name: "Team", Children: []matrix.Room{{ID: "!child:test", Name: "General"}}}},
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
}

func TestThreadUsesSplitViewAndArrowBreadcrumb(t *testing.T) {
	model := &chatModel{
		keys: config.Config{}, width: 100, mode: normalMode,
		view: viewport.New(40, 10), threadView: viewport.New(40, 10),
		currentRoom: "!room:test",
		roomInfo:    matrix.RoomInfo{Room: matrix.Room{ID: "!room:test", Name: "General"}, Spaces: []matrix.Room{{ID: "!space:test", Name: "Team"}}},
		messages: map[string][]matrix.Message{"!room:test": {
			{EventID: "$root", Sender: "@a:test", Body: "root"},
			{EventID: "$reply", ThreadRoot: "$root", Sender: "@b:test", Body: "reply"},
		}},
		thread: "$root", rootSelection: 0, selection: 1,
	}
	model.refreshView()
	view := model.View()
	if !strings.Contains(view, "Team -> General -> Thread") {
		t.Fatalf("missing arrow breadcrumb: %s", view)
	}
	for _, text := range []string{"Room", "Thread", "root", "reply", "│"} {
		if !strings.Contains(view, text) {
			t.Errorf("split view missing %q: %s", text, view)
		}
	}
}

func TestMessageRenderingShortensSenderAndShowsTime(t *testing.T) {
	output := renderMessages([]matrix.Message{{Sender: "@alice:example.com", Body: "hello", Timestamp: time.Date(2025, 1, 1, 12, 34, 0, 0, time.Local)}}, 0, true, 80)
	if !strings.Contains(output, "12:34") || !strings.Contains(output, "alice") || strings.Contains(output, "example.com") {
		t.Fatalf("unexpected rendered message: %s", output)
	}
}

func TestMessageRenderingWrapsInsteadOfClippingInThreadPane(t *testing.T) {
	const width = 32
	body := "A long thread reply with an unbroken-value-abcdefghijklmnopqrstuvwxyz0123456789 that must remain readable"
	output := renderMessages([]matrix.Message{{Sender: "@alice:example.com", Body: body}}, 0, true, width)
	for i, line := range strings.Split(output, "\n") {
		if got := ansi.StringWidth(line); got > width {
			t.Errorf("line %d width = %d, want <= %d: %q", i, got, width, ansi.Strip(line))
		}
	}
	if !strings.Contains(ansi.Strip(output), "must remain readable") {
		t.Fatalf("end of wrapped message is missing: %s", output)
	}
}

func TestNavigatorShowsUnreadCount(t *testing.T) {
	model := &chatModel{keys: config.Config{}, navigator: true, width: 80, unread: map[string]int{"!room:test": 3}, rooms: []matrix.Room{{ID: "!room:test", Name: "Room"}}}
	model.buildNavigator()
	if view := model.navigatorView(); !strings.Contains(view, "(3)") {
		t.Fatalf("missing unread count: %s", view)
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
