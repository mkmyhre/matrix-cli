package tui

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"matrix-cli/internal/config"
	"matrix-cli/internal/matrix"
)

type incomingMsg struct {
	message matrix.Message
	ok      bool
}
type streamErrMsg struct{ err error }
type sentMsg struct{ err error }
type clearStatusMsg struct{ version int }
type accountNotificationMsg struct {
	notification AccountNotification
	ok           bool
}

// AccountOption describes a locally configured account in the account picker.
type AccountOption struct {
	Name    string
	Color   string
	UserID  string
	Error   string
	Default bool
}

// AccountNotification is a live message received by a background account.
type AccountNotification struct {
	Account  string
	Color    string
	RoomName string
	Message  matrix.Message
	Error    string
}

// AccountDestination requests navigation to an account and optional room.
type AccountDestination struct {
	Account string
	Room    string
}

type RunResult struct {
	Destination   AccountDestination
	Notifications []AccountNotification
	Unread        map[string]int
}

type AccountSwitcher struct {
	Current              string
	Accounts             []AccountOption
	Notifications        <-chan AccountNotification
	InitialNotifications []AccountNotification
	InitialUnread        map[string]int
	SetDefault           func(string) error
}
type roomLoadedMsg struct {
	info matrix.RoomInfo
	page matrix.MessagePage
	err  error
}
type olderLoadedMsg struct {
	room string
	page matrix.MessagePage
	err  error
}
type navigatorLoadedMsg struct {
	rooms  []matrix.Room
	spaces []matrix.Space
	err    error
}

const historyPageSize = 10

type mode int

const (
	normalMode mode = iota
	insertMode
)

type navEntry struct {
	room  matrix.Room
	space matrix.Room
}

type chatModel struct {
	ctx              context.Context
	client           matrix.API
	keys             config.Config
	incoming         <-chan matrix.Message
	errors           <-chan error
	input            textinput.Model
	view             viewport.Model
	threadView       viewport.Model
	navView          viewport.Model
	rooms            []matrix.Room
	spaces           []matrix.Space
	nav              []navEntry
	navIndex         int
	navigator        bool
	navigatorLoading bool
	navigatorError   string

	currentRoom          string
	roomInfo             matrix.RoomInfo
	messages             map[string][]matrix.Message
	nextPage             map[string]string
	loadingOlder         map[string]bool
	unread               map[string]int
	selection            int
	rootSelection        int
	thread               string
	mode                 mode
	showIDs              bool
	showHelp             bool
	status               string
	statusVersion        int
	width                int
	height               int
	startupCmd           tea.Cmd
	accountName          string
	accountColor         string
	accounts             []AccountOption
	accountIndex         int
	accountPicker        bool
	accountError         string
	requested            AccountDestination
	setDefaultAccount    func(string) error
	accountNotifications <-chan AccountNotification
	notifications        []AccountNotification
	notificationIndex    int
	notificationPicker   bool
	accountUnread        map[string]int
	currentUserID        string
}

// RunNavigator opens the room/space browser without selecting a room first.
func RunNavigator(ctx context.Context, client matrix.API, cfg config.Config) error {
	_, err := run(ctx, client, "", cfg, AccountSwitcher{})
	return err
}

// RunNavigatorWithAccounts enables in-TUI switching between local accounts.
func RunNavigatorWithAccounts(ctx context.Context, client matrix.API, cfg config.Config, switcher AccountSwitcher) (RunResult, error) {
	return run(ctx, client, "", cfg, switcher)
}

// RunWithAccounts starts in a room when initialRoom is non-empty and returns a
// requested account/room destination when the user switches context.
func RunWithAccounts(ctx context.Context, client matrix.API, cfg config.Config, initialRoom string, switcher AccountSwitcher) (RunResult, error) {
	return run(ctx, client, initialRoom, cfg, switcher)
}

// RunChat opens a room initially; Esc in normal mode returns to the navigator.
func RunChat(ctx context.Context, client matrix.API, room string, cfg config.Config) error {
	_, err := run(ctx, client, room, cfg, AccountSwitcher{})
	return err
}

func run(ctx context.Context, client matrix.API, initialRoom string, cfg config.Config, switcher AccountSwitcher) (RunResult, error) {
	incoming, errs := client.Subscribe(ctx, "")
	input := textinput.New()
	input.Placeholder = "Message"
	input.CharLimit = 4000
	model := &chatModel{
		ctx: ctx, client: client, keys: cfg, incoming: incoming, errors: errs,
		input: input, view: viewport.New(80, 20), threadView: viewport.New(40, 20), navView: viewport.New(80, 20), navigatorLoading: true,
		messages: make(map[string][]matrix.Message), nextPage: make(map[string]string),
		loadingOlder: make(map[string]bool), unread: make(map[string]int), navigator: initialRoom == "",
		accountName: switcher.Current, accountColor: cfg.Color, accounts: switcher.Accounts, setDefaultAccount: switcher.SetDefault,
		accountNotifications: switcher.Notifications, accountUnread: make(map[string]int),
		notifications: append([]AccountNotification(nil), switcher.InitialNotifications...),
	}
	for account, count := range switcher.InitialUnread {
		model.accountUnread[account] = count
	}
	for i, account := range model.accounts {
		if account.Name == model.accountName {
			model.accountIndex = i
			model.currentUserID = account.UserID
			break
		}
	}
	var initialCmd tea.Cmd
	if initialRoom != "" {
		model.status = "loading room…"
		initialCmd = model.loadRoom(initialRoom)
	}
	if initialCmd != nil {
		model.startupCmd = initialCmd
	}
	program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithContext(ctx))
	final, err := program.Run()
	if err != nil {
		return RunResult{}, err
	}
	if result, ok := final.(*chatModel); ok {
		return RunResult{Destination: result.requested, Notifications: result.notifications, Unread: result.accountUnread}, nil
	}
	return RunResult{}, nil
}

// startupCmd is kept outside Init's fixed stream commands so chat can load an
// initial room while the all-room sync starts.
func (m *chatModel) Init() tea.Cmd {
	cmds := []tea.Cmd{waitIncoming(m.incoming), waitError(m.errors), m.loadNavigator()}
	if m.startupCmd != nil {
		cmds = append(cmds, m.startupCmd)
	}
	if m.accountNotifications != nil {
		cmds = append(cmds, waitAccountNotification(m.accountNotifications))
	}
	return tea.Batch(cmds...)
}

func waitIncoming(ch <-chan matrix.Message) tea.Cmd {
	return func() tea.Msg { msg, ok := <-ch; return incomingMsg{message: msg, ok: ok} }
}

func waitAccountNotification(ch <-chan AccountNotification) tea.Cmd {
	return func() tea.Msg {
		notification, ok := <-ch
		return accountNotificationMsg{notification: notification, ok: ok}
	}
}

func waitError(ch <-chan error) tea.Cmd {
	return func() tea.Msg {
		err, ok := <-ch
		if !ok {
			return streamErrMsg{}
		}
		return streamErrMsg{err: err}
	}
}

func (m *chatModel) loadNavigator() tea.Cmd {
	return func() tea.Msg {
		type roomListResult struct {
			rooms []matrix.Room
			err   error
		}
		roomsCh := make(chan roomListResult, 1)
		go func() {
			rooms, err := m.client.Rooms(m.ctx)
			roomsCh <- roomListResult{rooms: rooms, err: err}
		}()
		spaces, spacesErr := m.client.Spaces(m.ctx)
		roomResult := <-roomsCh
		if roomResult.err != nil {
			return navigatorLoadedMsg{err: fmt.Errorf("load rooms: %w", roomResult.err)}
		}
		if spacesErr != nil {
			return navigatorLoadedMsg{err: fmt.Errorf("load spaces: %w", spacesErr)}
		}
		return navigatorLoadedMsg{rooms: roomResult.rooms, spaces: spaces}
	}
}

func (m *chatModel) loadRoom(room string) tea.Cmd {
	return func() tea.Msg {
		info, err := m.client.RoomInfo(m.ctx, room)
		if err != nil {
			return roomLoadedMsg{err: err}
		}
		page, err := m.fetchHistory(info.ID, "")
		return roomLoadedMsg{info: info, page: page, err: err}
	}
}

// fetchHistory keeps paging past reactions, membership events, and other
// non-message events until it has roughly one useful screen of messages.
func (m *chatModel) fetchHistory(room, from string) (matrix.MessagePage, error) {
	result := matrix.MessagePage{Next: from}
	for attempts := 0; attempts < 10 && len(result.Messages) < historyPageSize; attempts++ {
		previous := result.Next
		page, err := m.client.RecentMessages(m.ctx, room, previous, historyPageSize-len(result.Messages))
		if err != nil {
			return matrix.MessagePage{}, err
		}
		result.Messages = mergeMessages(page.Messages, result.Messages)
		result.Next = page.Next
		if page.Next == "" || page.Next == previous {
			break
		}
	}
	return result, nil
}

func (m *chatModel) loadOlder() tea.Cmd {
	room := m.currentRoom
	from := m.nextPage[room]
	if room == "" || m.loadingOlder[room] {
		return nil
	}
	if from == "" {
		m.status = "start of history"
		return nil
	}
	m.loadingOlder[room] = true
	m.status = fmt.Sprintf("loading %d older messages…", historyPageSize)
	return func() tea.Msg {
		page, err := m.fetchHistory(room, from)
		return olderLoadedMsg{room: room, page: page, err: err}
	}
}

func (m *chatModel) buildNavigator() {
	spaceIDs := make(map[string]bool, len(m.spaces))
	contained := make(map[string]bool)
	for _, space := range m.spaces {
		spaceIDs[space.ID] = true
		for _, room := range space.Children {
			contained[room.ID] = true
		}
	}
	roomByID := make(map[string]matrix.Room, len(m.rooms))
	for _, room := range m.rooms {
		roomByID[room.ID] = room
	}
	m.nav = nil
	for _, room := range m.rooms {
		if !spaceIDs[room.ID] && !contained[room.ID] {
			m.nav = append(m.nav, navEntry{room: room})
		}
	}
	sort.Slice(m.nav, func(i, j int) bool { return roomLabel(m.nav[i].room) < roomLabel(m.nav[j].room) })
	sort.Slice(m.spaces, func(i, j int) bool {
		return roomLabel(matrix.Room{ID: m.spaces[i].ID, Name: m.spaces[i].Name}) < roomLabel(matrix.Room{ID: m.spaces[j].ID, Name: m.spaces[j].Name})
	})
	for _, space := range m.spaces {
		spaceRoom := matrix.Room{ID: space.ID, Name: space.Name}
		children := append([]matrix.Room(nil), space.Children...)
		sort.Slice(children, func(i, j int) bool { return roomLabel(children[i]) < roomLabel(children[j]) })
		seen := make(map[string]bool)
		for _, child := range children {
			if child.ID == "" || spaceIDs[child.ID] || seen[child.ID] {
				continue
			}
			seen[child.ID] = true
			known, joined := roomByID[child.ID]
			if !joined {
				continue
			}
			if known.Name != "" {
				child.Name = known.Name
			}
			m.nav = append(m.nav, navEntry{room: child, space: spaceRoom})
		}
	}
	if len(m.nav) > 0 {
		m.navIndex = min(m.navIndex, len(m.nav)-1)
	}
}

func roomLabel(room matrix.Room) string {
	if room.Name != "" {
		return room.Name
	}
	return room.ID
}

func (m *chatModel) Update(raw tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := raw.(type) {
	case tea.KeyMsg:
		key := strings.ToLower(msg.String())
		if msg.Type == tea.KeyCtrlC {
			return m, tea.Quit
		}
		if m.mode != insertMode && key == m.keys.Key("help") {
			m.showHelp = !m.showHelp
			return m, nil
		}
		if m.showHelp {
			if key == m.keys.Key("close_thread") {
				m.showHelp = false
			}
			return m, nil
		}
		if m.accountPicker {
			return m.updateAccountPicker(key)
		}
		if m.notificationPicker {
			return m.updateNotificationPicker(key)
		}
		if m.mode != insertMode && key == m.keys.Key("notifications") {
			m.notificationPicker = true
			m.notificationIndex = 0
			return m, nil
		}
		if m.mode != insertMode && key == m.keys.Key("switch_account") && len(m.accounts) > 0 {
			m.accountPicker = true
			m.accountError = ""
			return m, nil
		}
		if m.navigator {
			return m.updateNavigator(key)
		}
		if m.mode == insertMode {
			if key == m.keys.Key("normal_mode") {
				m.mode = normalMode
				m.input.Blur()
				m.refreshView()
				return m, nil
			}
			if key == m.keys.Key("send") {
				return m, m.sendCurrent()
			}
			var cmd tea.Cmd
			m.input, cmd = m.input.Update(msg)
			return m, cmd
		}
		switch key {
		case m.keys.Key("quit"):
			return m, tea.Quit
		case m.keys.Key("insert_mode"):
			m.mode = insertMode
			m.refreshView()
			return m, m.input.Focus()
		case m.keys.Key("move_down"):
			m.selection = min(len(m.visibleMessages())-1, m.selection+1)
			m.refreshView()
		case m.keys.Key("move_up"):
			if m.selection == 0 {
				return m, m.loadOlder()
			}
			m.selection = max(0, m.selection-1)
			m.refreshView()
		case m.keys.Key("load_older"):
			return m, m.loadOlder()
		case m.keys.Key("toggle_identifiers"):
			m.showIDs = !m.showIDs
		case m.keys.Key("close_thread"):
			if m.thread != "" {
				m.thread = ""
				m.selection = m.rootSelection
				m.refreshView()
			} else {
				m.navigator = true
				m.mode = normalMode
				m.status = ""
			}
		case m.keys.Key("open_thread"):
			visible := m.visibleMessages()
			if m.thread == "" && m.selection >= 0 && m.selection < len(visible) && visible[m.selection].EventID != "" {
				m.rootSelection = m.selection
				m.thread = visible[m.selection].EventID
				m.selection = max(0, len(m.visibleMessages())-1)
				m.refreshView()
			}
		}
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.view.Height = max(3, msg.Height-7)
		m.threadView.Height = m.view.Height
		m.input.Width = max(10, msg.Width-4)
		m.refreshView()
	case incomingMsg:
		if msg.ok {
			m.messages[msg.message.RoomID] = append(m.messages[msg.message.RoomID], msg.message)
			visibleHere := msg.message.RoomID == m.currentRoom && !m.navigator
			if m.thread == "" && msg.message.ThreadRoot != "" {
				visibleHere = false
			} else if m.thread != "" && msg.message.EventID != m.thread && msg.message.ThreadRoot != m.thread {
				visibleHere = false
			}
			if visibleHere {
				m.selection = max(0, len(m.visibleMessages())-1)
				m.refreshView()
			} else {
				m.unread[msg.message.RoomID]++
				if msg.message.Sender != m.currentUserID {
					m.addNotification(AccountNotification{Account: m.accountName, Color: m.accountColor, RoomName: m.roomName(msg.message.RoomID), Message: msg.message})
				}
			}
			return m, waitIncoming(m.incoming)
		}
	case accountNotificationMsg:
		if msg.ok {
			if msg.notification.Error != "" {
				for i := range m.accounts {
					if m.accounts[i].Name == msg.notification.Account {
						m.accounts[i].Error = msg.notification.Error
						break
					}
				}
				m.status = fmt.Sprintf("%s sync error: %s", msg.notification.Account, msg.notification.Error)
			} else if msg.notification.Message.Sender != m.accountUserID(msg.notification.Account) {
				m.addNotification(msg.notification)
			}
			return m, waitAccountNotification(m.accountNotifications)
		}
	case streamErrMsg:
		if msg.err != nil {
			m.status = "sync error: " + msg.err.Error()
		}
	case sentMsg:
		if msg.err != nil {
			m.status = "send error: " + msg.err.Error()
			return m, nil
		}
		return m, m.setTransientStatus("sent", 2*time.Second)
	case navigatorLoadedMsg:
		m.navigatorLoading = false
		if msg.err != nil {
			m.navigatorError = msg.err.Error()
			if m.navigator {
				m.status = "navigator error: " + msg.err.Error()
			}
			return m, nil
		}
		m.rooms, m.spaces = msg.rooms, msg.spaces
		m.buildNavigator()
		m.navigatorError = ""
		if m.navigator && strings.HasPrefix(m.status, "loading rooms") {
			m.status = ""
		}
	case roomLoadedMsg:
		if msg.err != nil {
			m.status = "open room: " + msg.err.Error()
			m.navigator = true
			return m, nil
		}
		m.roomInfo = msg.info
		m.currentRoom = msg.info.ID
		delete(m.unread, msg.info.ID)
		m.messages[msg.info.ID] = mergeMessages(msg.page.Messages, m.messages[msg.info.ID])
		m.nextPage[msg.info.ID] = msg.page.Next
		m.thread = ""
		m.rootSelection = 0
		m.selection = max(0, len(m.visibleMessages())-1)
		m.navigator = false
		m.mode = normalMode
		if len(msg.page.Messages) == 0 {
			m.status = "no message history available"
		} else {
			m.status = ""
		}
		m.refreshView()
	case olderLoadedMsg:
		m.loadingOlder[msg.room] = false
		if msg.err != nil {
			m.status = "load history: " + msg.err.Error()
			return m, nil
		}
		before := len(m.messages[msg.room])
		m.messages[msg.room] = mergeMessages(msg.page.Messages, m.messages[msg.room])
		m.nextPage[msg.room] = msg.page.Next
		if msg.room == m.currentRoom {
			m.selection += len(m.messages[msg.room]) - before
			loaded := len(m.messages[msg.room]) - before
			text := fmt.Sprintf("loaded %d older messages", loaded)
			if loaded == 0 {
				text = "start of history"
			} else if msg.page.Next == "" {
				text += " · start of history"
			}
			m.refreshView()
			return m, m.setTransientStatus(text, 2*time.Second)
		}
	case clearStatusMsg:
		if msg.version == m.statusVersion {
			m.status = ""
		}
	}
	return m, nil
}

func (m *chatModel) setTransientStatus(text string, duration time.Duration) tea.Cmd {
	m.status = text
	m.statusVersion++
	version := m.statusVersion
	return tea.Tick(duration, func(time.Time) tea.Msg { return clearStatusMsg{version: version} })
}

func (m *chatModel) updateAccountPicker(key string) (tea.Model, tea.Cmd) {
	switch key {
	case m.keys.Key("quit"), m.keys.Key("close_thread"), m.keys.Key("switch_account"):
		m.accountPicker = false
		m.accountError = ""
	case m.keys.Key("move_down"):
		if len(m.accounts) > 0 {
			m.accountIndex = min(len(m.accounts)-1, m.accountIndex+1)
		}
	case m.keys.Key("move_up"):
		m.accountIndex = max(0, m.accountIndex-1)
	case m.keys.Key("open_thread"):
		if m.accountIndex >= 0 && m.accountIndex < len(m.accounts) {
			selected := m.accounts[m.accountIndex].Name
			if selected == m.accountName {
				m.accountPicker = false
				return m, nil
			}
			if m.accounts[m.accountIndex].Error != "" {
				m.accountError = m.accounts[m.accountIndex].Error
				return m, nil
			}
			m.requested = AccountDestination{Account: selected}
			return m, tea.Quit
		}
	case m.keys.Key("set_default_account"):
		if m.accountIndex < 0 || m.accountIndex >= len(m.accounts) || m.setDefaultAccount == nil {
			return m, nil
		}
		selected := m.accounts[m.accountIndex].Name
		if err := m.setDefaultAccount(selected); err != nil {
			m.accountError = err.Error()
			return m, nil
		}
		for i := range m.accounts {
			m.accounts[i].Default = m.accounts[i].Name == selected
		}
		m.accountError = "default account set to " + selected
	}
	return m, nil
}

func (m *chatModel) updateNotificationPicker(key string) (tea.Model, tea.Cmd) {
	switch key {
	case m.keys.Key("quit"), m.keys.Key("close_thread"), m.keys.Key("notifications"):
		m.notificationPicker = false
	case m.keys.Key("move_down"):
		if len(m.notifications) > 0 {
			m.notificationIndex = min(len(m.notifications)-1, m.notificationIndex+1)
		}
	case m.keys.Key("move_up"):
		m.notificationIndex = max(0, m.notificationIndex-1)
	case m.keys.Key("open_thread"):
		if m.notificationIndex < 0 || m.notificationIndex >= len(m.notifications) {
			return m, nil
		}
		notification := m.notifications[m.notificationIndex]
		m.notifications = append(m.notifications[:m.notificationIndex], m.notifications[m.notificationIndex+1:]...)
		m.accountUnread[notification.Account] = max(0, m.accountUnread[notification.Account]-1)
		m.notificationIndex = min(m.notificationIndex, max(0, len(m.notifications)-1))
		if notification.Account == m.accountName {
			m.notificationPicker = false
			m.status = "loading " + notification.Message.RoomID + "…"
			return m, m.loadRoom(notification.Message.RoomID)
		}
		for _, account := range m.accounts {
			if account.Name == notification.Account && account.Error != "" {
				m.status = account.Error
				return m, nil
			}
		}
		m.requested = AccountDestination{Account: notification.Account, Room: notification.Message.RoomID}
		return m, tea.Quit
	}
	return m, nil
}

func (m *chatModel) updateNavigator(key string) (tea.Model, tea.Cmd) {
	switch key {
	case m.keys.Key("quit"):
		return m, tea.Quit
	case m.keys.Key("move_down"):
		if len(m.nav) > 0 {
			m.navIndex = min(len(m.nav)-1, m.navIndex+1)
		}
	case m.keys.Key("move_up"):
		m.navIndex = max(0, m.navIndex-1)
	case m.keys.Key("toggle_identifiers"):
		m.showIDs = !m.showIDs
	case m.keys.Key("open_thread"):
		if m.navIndex >= 0 && m.navIndex < len(m.nav) {
			m.status = "loading " + m.nav[m.navIndex].room.ID + "…"
			return m, m.loadRoom(m.nav[m.navIndex].room.ID)
		}
	case m.keys.Key("close_thread"):
		if m.currentRoom != "" {
			m.navigator = false
			delete(m.unread, m.currentRoom)
			m.status = ""
		}
	}
	return m, nil
}

func (m *chatModel) sendCurrent() tea.Cmd {
	body := strings.TrimSpace(m.input.Value())
	if body == "" {
		return nil
	}
	m.input.SetValue("")
	m.status = "sending…"
	thread, room := m.thread, m.currentRoom
	return func() tea.Msg {
		if thread != "" {
			return sentMsg{err: m.client.SendThread(m.ctx, room, thread, body)}
		}
		return sentMsg{err: m.client.Send(m.ctx, room, body)}
	}
}

func mergeMessages(first, second []matrix.Message) []matrix.Message {
	merged := make([]matrix.Message, 0, len(first)+len(second))
	seen := make(map[string]bool, len(first)+len(second))
	for _, list := range [][]matrix.Message{first, second} {
		for _, msg := range list {
			key := msg.EventID
			if key != "" && seen[key] {
				continue
			}
			if key != "" {
				seen[key] = true
			}
			merged = append(merged, msg)
		}
	}
	return merged
}

func (m *chatModel) mainMessages() []matrix.Message {
	all := m.messages[m.currentRoom]
	visible := make([]matrix.Message, 0, len(all))
	for _, msg := range all {
		if msg.ThreadRoot == "" {
			visible = append(visible, msg)
		}
	}
	return visible
}

func (m *chatModel) threadMessages() []matrix.Message {
	if m.thread == "" {
		return nil
	}
	all := m.messages[m.currentRoom]
	visible := make([]matrix.Message, 0, len(all))
	for _, msg := range all {
		if msg.EventID == m.thread || msg.ThreadRoot == m.thread {
			visible = append(visible, msg)
		}
	}
	return visible
}

func (m *chatModel) visibleMessages() []matrix.Message {
	if m.thread != "" {
		return m.threadMessages()
	}
	return m.mainMessages()
}

func (m *chatModel) accountUserID(accountName string) string {
	for _, account := range m.accounts {
		if account.Name == accountName {
			return account.UserID
		}
	}
	return ""
}

func (m *chatModel) roomName(roomID string) string {
	for _, room := range m.rooms {
		if room.ID == roomID && room.Name != "" {
			return room.Name
		}
	}
	return roomID
}

func (m *chatModel) addNotification(notification AccountNotification) {
	if notification.Account == "" || notification.Message.RoomID == "" {
		return
	}
	m.notifications = append([]AccountNotification{notification}, m.notifications...)
	if len(m.notifications) > 100 {
		m.notifications = m.notifications[:100]
	}
	m.accountUnread[notification.Account]++
}

func shortSender(sender string) string {
	sender = strings.TrimPrefix(sender, "@")
	if before, _, ok := strings.Cut(sender, ":"); ok {
		return before
	}
	return sender
}

func indentBlock(text, prefix string) string {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if i == 0 {
			lines[i] = prefix + l
		} else {
			lines[i] = strings.Repeat(" ", len(prefix)) + l
		}
	}
	return strings.Join(lines, "\n")
}

func wrapMessageBody(body string, width int) []string {
	if width <= 0 {
		return strings.Split(body, "\n")
	}
	// Wordwrap keeps normal prose readable; Wrap also breaks long URLs and
	// unspaced text so the viewport never has to clip horizontally.
	body = ansi.Wordwrap(body, width, "")
	body = ansi.Wrap(body, width, "")
	return strings.Split(body, "\n")
}

func renderMessages(messages []matrix.Message, selected int, active bool, width int) string {
	lines := make([]string, 0, len(messages))
	for i, msg := range messages {
		stamp := "     "
		if !msg.Timestamp.IsZero() {
			stamp = msg.Timestamp.Local().Format("15:04")
		}
		rawHeader := fmt.Sprintf("%s  %-14s ", stamp, shortSender(msg.Sender))
		header := dimStyle.Render(rawHeader)
		headerWidth := lipgloss.Width(rawHeader)
		contentWidth := width - 2 // selection marker or normal outer indent
		msgLines := make([]string, 0)
		if width > 0 && contentWidth-headerWidth < 24 {
			// A thread pane is too narrow for metadata and useful body text on the
			// same line. Put the body below the header and use the pane width.
			msgLines = append(msgLines, dimStyle.Render(strings.TrimRight(rawHeader, " ")))
			// Leave room for the selection bar, outer indent, and indented body.
			bodyWidth := max(1, contentWidth-4)
			for _, bodyLine := range wrapMessageBody(msg.Body, bodyWidth) {
				msgLines = append(msgLines, "  "+bodyLine)
			}
		} else {
			bodyWidth := 0
			if width > 0 {
				bodyWidth = max(1, contentWidth-headerWidth)
			}
			bodyLines := wrapMessageBody(msg.Body, bodyWidth)
			if len(bodyLines) == 0 {
				bodyLines = []string{""}
			}
			for j, bodyLine := range bodyLines {
				if j == 0 {
					msgLines = append(msgLines, header+bodyLine)
				} else {
					msgLines = append(msgLines, strings.Repeat(" ", headerWidth)+bodyLine)
				}
			}
		}
		text := strings.Join(msgLines, "\n")
		line := indentBlock(text, "  ")
		if i == selected {
			line = indentBlock(text, "│ ")
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func (m *chatModel) refreshView() {
	main := m.mainMessages()
	if m.thread == "" {
		if len(main) == 0 {
			m.selection = 0
		} else {
			m.selection = min(max(0, m.selection), len(main)-1)
		}
		m.view.Width = max(20, m.width-6)
		m.view.Height = max(3, m.height-9)
		m.view.SetContent(renderMessages(main, m.selection, m.mode == normalMode, m.view.Width))
		m.view.GotoBottom()
		return
	}
	thread := m.threadMessages()
	if len(thread) == 0 {
		m.selection = 0
	} else {
		m.selection = min(max(0, m.selection), len(thread)-1)
	}
	if m.keys.EffectiveThreadView() == config.ThreadViewFocused {
		m.view.Width = max(20, m.width-6)
		m.view.Height = max(3, m.height-9)
		m.view.SetContent(renderMessages(thread, m.selection, m.mode == normalMode, m.view.Width))
		m.view.GotoBottom()
		return
	}
	if len(main) == 0 {
		m.rootSelection = 0
	} else {
		m.rootSelection = min(max(0, m.rootSelection), len(main)-1)
	}
	columnWidth := max(20, (m.width-5)/2)
	if m.width < 80 {
		columnWidth = max(20, m.width-4)
		paneHeight := max(3, (m.height-13)/2)
		m.view.Height, m.threadView.Height = paneHeight, paneHeight
	} else {
		paneHeight := max(3, m.height-9)
		m.view.Height, m.threadView.Height = paneHeight, paneHeight
	}
	m.view.Width = max(16, columnWidth-4)
	m.threadView.Width = max(16, columnWidth-4)
	m.view.SetContent(renderMessages(main, m.rootSelection, false, m.view.Width))
	m.threadView.SetContent(renderMessages(thread, m.selection, m.mode == normalMode, m.threadView.Width))
	m.view.GotoBottom()
	m.threadView.GotoBottom()
}

var (
	titleStyle    = lipgloss.NewStyle().Bold(true)
	sectionStyle  = lipgloss.NewStyle().Bold(true)
	selectedStyle = lipgloss.NewStyle().Bold(true)
	modeStyle     = lipgloss.NewStyle().Bold(true)
	dimStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	unreadStyle   = lipgloss.NewStyle().Bold(true)
	errorStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	successStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
)

func truncateText(text string, limit int) string {
	if limit < 2 || utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit-1]) + "…"
}

func (m *chatModel) label(room matrix.Room) string {
	if m.showIDs || room.Name == "" {
		return room.ID
	}
	return truncateText(room.Name, max(12, m.width-8))
}

func (m *chatModel) roomNavLabel(room matrix.Room) string {
	label := m.label(room)
	if count := m.unread[room.ID]; count > 0 {
		label += unreadStyle.Render(fmt.Sprintf(" (%d)", count))
	}
	return label
}

func accountStyle(color string) lipgloss.Style {
	style := lipgloss.NewStyle().Bold(true)
	if config.ValidAccountColor(color) && color != "" {
		style = style.Foreground(lipgloss.Color(color))
	}
	return style
}

func (m *chatModel) accountPickerView() string {
	lines := make([]string, 0, len(m.accounts)+2)
	for i, account := range m.accounts {
		marker := "  "
		if i == m.accountIndex {
			marker = "│ "
		}
		label := accountStyle(account.Color).Render(account.Name)
		if account.Name == m.accountName {
			label += "  (current)"
		}
		if account.Default {
			label += "  [default]"
		}
		if count := m.accountUnread[account.Name]; count > 0 {
			label += fmt.Sprintf("  %d", count)
		}
		if account.Error != "" {
			label += "  [sign-in required]"
		}
		lines = append(lines, marker+label)
	}
	if len(lines) == 0 {
		lines = append(lines, "  (no configured accounts)")
	}
	footer := fmt.Sprintf("%s/%s move · %s switch · %s set default · %s close", m.keys.Key("move_down"), m.keys.Key("move_up"), m.keys.Key("open_thread"), m.keys.Key("set_default_account"), m.keys.Key("close_thread"))
	if m.accountError != "" {
		footer = m.accountError
	}
	return m.panel("Accounts", strings.Join(lines, "\n"), max(24, m.width-2), true) + "\n" + dimStyle.Render(footer)
}

func (m *chatModel) notificationPickerView() string {
	lines := make([]string, 0, len(m.notifications)*2)
	for i, notification := range m.notifications {
		marker := "  "
		if i == m.notificationIndex {
			marker = "│ "
		}
		stamp := "     "
		if !notification.Message.Timestamp.IsZero() {
			stamp = notification.Message.Timestamp.Local().Format("15:04")
		}
		room := notification.RoomName
		if room == "" {
			room = notification.Message.RoomID
		}
		account := accountStyle(notification.Color).Render(notification.Account)
		lines = append(lines, fmt.Sprintf("%s%s  %s / %s  %s", marker, stamp, account, truncateText(room, 24), shortSender(notification.Message.Sender)))
		preview := strings.ReplaceAll(notification.Message.Body, "\n", " ")
		lines = append(lines, "    "+truncateText(preview, max(20, m.width-8)), "")
	}
	if len(lines) == 0 {
		lines = append(lines, "  No new notifications while the TUI has been running.")
	}
	footer := fmt.Sprintf("%s/%s move · %s open · %s close", m.keys.Key("move_down"), m.keys.Key("move_up"), m.keys.Key("open_thread"), m.keys.Key("close_thread"))
	return m.panel(fmt.Sprintf("Notifications · %d", len(m.notifications)), strings.Join(lines, "\n"), max(24, m.width-2), true) + "\n" + dimStyle.Render(footer)
}

func (m *chatModel) navigatorView() string {
	var lines []string
	selectedLine := -1
	if m.accountName != "" {
		accountLine := accountStyle(m.accountColor).Render("Account: " + m.accountName)
		if count := len(m.notifications); count > 0 {
			accountLine += fmt.Sprintf("  ·  %d notifications", count)
		}
		lines = append(lines, accountLine, "")
	}
	if m.navigatorLoading {
		lines = append(lines, "Loading rooms and spaces…")
	} else if m.navigatorError != "" {
		lines = append(lines, errorStyle.Render("Unable to load rooms and spaces"))
	}
	lines = append(lines, sectionStyle.Render("Home"))
	homeCount := 0
	for i, entry := range m.nav {
		if entry.space.ID == "" {
			homeCount++
			prefix := "  "
			if i == m.navIndex {
				prefix = selectedStyle.Render("│ ")
				selectedLine = len(lines)
			}
			lines = append(lines, prefix+m.roomNavLabel(entry.room))
		}
	}
	if homeCount == 0 {
		lines = append(lines, "  (no ungrouped rooms)")
	}
	for _, space := range m.spaces {
		spaceRoom := matrix.Room{ID: space.ID, Name: space.Name}
		lines = append(lines, "", sectionStyle.Render(m.label(spaceRoom)))
		count := 0
		for i, entry := range m.nav {
			if entry.space.ID != space.ID {
				continue
			}
			count++
			prefix := "  "
			if i == m.navIndex {
				prefix = selectedStyle.Render("│ ")
				selectedLine = len(lines)
			}
			lines = append(lines, prefix+m.roomNavLabel(entry.room))
		}
		if count == 0 {
			lines = append(lines, "  (no rooms)")
		}
	}
	footer := fmt.Sprintf("%s open · %s accounts · %s notifications · %s help", m.keys.Key("open_thread"), m.keys.Key("switch_account"), m.keys.Key("notifications"), m.keys.Key("help"))
	if m.status != "" {
		footer = renderStatus(m.status)
	}
	body := strings.Join(lines, "\n")
	if m.height > 0 {
		viewportHeight := max(3, m.height-5)
		if m.keys.EffectiveTheme() == config.ThemeBoxed {
			viewportHeight = max(3, m.height-7)
		}
		m.navView.Width = max(20, m.width-4)
		m.navView.Height = viewportHeight
		m.navView.SetContent(body)
		if selectedLine >= 0 {
			if selectedLine < m.navView.YOffset {
				m.navView.SetYOffset(selectedLine)
			} else if selectedLine >= m.navView.YOffset+m.navView.Height {
				m.navView.SetYOffset(selectedLine - m.navView.Height + 1)
			}
		}
		body = m.navView.View()
	}
	return m.panel("Rooms & Spaces", body, max(24, m.width-2), true) + "\n" + dimStyle.Render(footer)
}

func (m *chatModel) panel(title, content string, width int, active bool) string {
	if m.keys.EffectiveTheme() == config.ThemeBoxed {
		color := lipgloss.Color("8")
		if active {
			color = lipgloss.Color("12")
		}
		style := lipgloss.NewStyle().Width(max(16, width-2)).Border(lipgloss.RoundedBorder()).BorderForeground(color)
		return style.Render(sectionStyle.Render(title) + "\n" + content)
	}
	separatorWidth := max(8, width-2)
	return sectionStyle.Render(title) + "\n" + dimStyle.Render(strings.Repeat("─", separatorWidth)) + "\n" + content
}

func renderStatus(status string) string {
	if strings.Contains(status, "error") {
		return errorStyle.Render(status)
	}
	if status == "sent" || strings.HasPrefix(status, "loaded") {
		return successStyle.Render(status)
	}
	return status
}

func (m *chatModel) helpView() string {
	actions := make([]string, 0, len(config.DefaultKeybindings))
	for action := range config.DefaultKeybindings {
		actions = append(actions, action)
	}
	sort.Strings(actions)
	lines := []string{}
	for _, action := range actions {
		name := strings.ReplaceAll(action, "_", " ")
		lines = append(lines, fmt.Sprintf("%-20s %s", name, m.keys.Key(action)))
	}
	lines = append(lines, "", m.keys.Key("help")+" or "+m.keys.Key("close_thread")+" closes help")
	return m.panel("Keybindings", strings.Join(lines, "\n"), max(32, m.width-2), true)
}

func (m *chatModel) View() string {
	if m.showHelp {
		return m.helpView()
	}
	if m.accountPicker {
		return m.accountPickerView()
	}
	if m.notificationPicker {
		return m.notificationPickerView()
	}
	if m.navigator {
		return m.navigatorView()
	}

	location := "Home"
	if len(m.roomInfo.Spaces) > 0 {
		parents := make([]string, len(m.roomInfo.Spaces))
		for i, space := range m.roomInfo.Spaces {
			parents[i] = m.label(space)
		}
		location = strings.Join(parents, ", ")
	}
	header := location + " / " + m.label(m.roomInfo.Room)
	if m.thread != "" {
		threadLabel := "thread"
		if m.showIDs {
			threadLabel += " " + m.thread
		}
		header += " / " + threadLabel
	}
	status := m.status
	if status == "" {
		status = fmt.Sprintf("%s 10 older · %s help · %s accounts · %s notifications", m.keys.Key("load_older"), m.keys.Key("help"), m.keys.Key("switch_account"), m.keys.Key("notifications"))
		if m.thread != "" {
			status = fmt.Sprintf("%s room · %s", m.keys.Key("close_thread"), status)
		}
	} else {
		status = renderStatus(status)
	}
	content := m.panel("Room", m.view.View(), max(24, m.width-2), true)
	if m.thread != "" {
		if m.keys.EffectiveThreadView() == config.ThreadViewSplit {
			paneWidth := max(22, (m.width-3)/2)
			left := m.panel("Room", m.view.View(), paneWidth, false)
			right := m.panel(fmt.Sprintf("Thread · %d replies", max(0, len(m.threadMessages())-1)), m.threadView.View(), paneWidth, true)
			if m.width < 80 {
				content = lipgloss.JoinVertical(lipgloss.Left, left, right)
			} else {
				content = lipgloss.JoinHorizontal(lipgloss.Top, left, right)
			}
		} else {
			content = m.panel(fmt.Sprintf("Thread · %d replies", max(0, len(m.threadMessages())-1)), m.view.View(), max(24, m.width-2), true)
		}
	}
	if m.accountName != "" {
		header = accountStyle(m.accountColor).Render(m.accountName) + " / " + header
	}
	if count := len(m.notifications); count > 0 {
		header += fmt.Sprintf("  ·  %s notifications (%d)", m.keys.Key("notifications"), count)
	}
	footer := dimStyle.Render(status)
	if m.mode == insertMode {
		footer = modeStyle.Render("INSERT") + "  " + footer
	}
	return titleStyle.Render(header) + "\n\n" + content + "\n\n" + m.input.View() + "\n" + footer
}
