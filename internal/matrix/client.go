package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

type Room struct {
	ID   string
	Name string
}

type Space struct {
	ID       string
	Name     string
	Children []Room
}

type RoomInfo struct {
	Room
	Spaces []Room
}

type MessagePage struct {
	Messages []Message
	Next     string
}

type Message struct {
	RoomID     string
	Sender     string
	Body       string
	EventID    string
	ThreadRoot string
	Timestamp  time.Time
}

type API interface {
	Rooms(context.Context) ([]Room, error)
	Spaces(context.Context) ([]Space, error)
	RoomInfo(context.Context, string) (RoomInfo, error)
	RecentMessages(context.Context, string, string, int) (MessagePage, error)
	Send(context.Context, string, string) error
	SendThread(context.Context, string, string, string) error
	Subscribe(context.Context, string) (<-chan Message, <-chan error)
	Logout(context.Context) error
}

type cryptoSupport interface {
	Init(context.Context) error
	Decrypt(context.Context, *event.Event) (*event.Event, error)
	Verify(context.Context, io.Reader, io.Writer) error
}

var ErrInvalidSession = errors.New("Matrix session is no longer valid")

type Client struct {
	raw    *mautrix.Client
	crypto cryptoSupport
}

func New(homeserverURL, userID, deviceID, accessToken string) (*Client, error) {
	raw, err := mautrix.NewClient(homeserverURL, id.UserID(userID), accessToken)
	if err != nil {
		return nil, err
	}
	raw.DeviceID = id.DeviceID(deviceID)
	return &Client{raw: raw}, nil
}

// ValidateSession checks authentication and identity before crypto or other
// device-specific state is initialized.
func (c *Client) ValidateSession(ctx context.Context) error {
	resp, err := c.raw.Whoami(ctx)
	if err != nil {
		if errors.Is(err, mautrix.MUnknownToken) || errors.Is(err, mautrix.MMissingToken) {
			return fmt.Errorf("%w: homeserver rejected the access token", ErrInvalidSession)
		}
		return fmt.Errorf("validate Matrix session: %w", err)
	}
	if resp.UserID != c.raw.UserID {
		return fmt.Errorf("%w: token belongs to %s, not %s", ErrInvalidSession, resp.UserID, c.raw.UserID)
	}
	if resp.DeviceID != "" && c.raw.DeviceID != "" && resp.DeviceID != c.raw.DeviceID {
		return fmt.Errorf("%w: token belongs to device %s, not %s", ErrInvalidSession, resp.DeviceID, c.raw.DeviceID)
	}
	return nil
}

func (c *Client) Init(ctx context.Context) error {
	if c.crypto == nil {
		return nil
	}
	return c.crypto.Init(ctx)
}

func (c *Client) Rooms(ctx context.Context) ([]Room, error) {
	resp, err := c.raw.JoinedRooms(ctx)
	if err != nil {
		return nil, err
	}
	rooms := make([]Room, 0, len(resp.JoinedRooms))
	for _, roomID := range resp.JoinedRooms {
		var content event.RoomNameEventContent
		_ = c.raw.StateEvent(ctx, roomID, event.StateRoomName, "", &content)
		rooms = append(rooms, Room{ID: roomID.String(), Name: content.Name})
	}
	return rooms, nil
}

func (c *Client) Spaces(ctx context.Context) ([]Space, error) {
	joined, err := c.raw.JoinedRooms(ctx)
	if err != nil {
		return nil, err
	}
	spaces := make([]Space, 0)
	for _, roomID := range joined.JoinedRooms {
		var create event.CreateEventContent
		if err := c.raw.StateEvent(ctx, roomID, event.StateCreate, "", &create); err != nil || create.Type != event.RoomTypeSpace {
			continue
		}
		var name event.RoomNameEventContent
		_ = c.raw.StateEvent(ctx, roomID, event.StateRoomName, "", &name)
		space := Space{ID: roomID.String(), Name: name.Name}
		from := ""
		directIDs := make([]string, 0)
		roomDetails := make(map[string]Room)
		for {
			resp, err := c.raw.Hierarchy(ctx, roomID, &mautrix.ReqHierarchy{From: from, Limit: 100})
			if err != nil {
				return nil, fmt.Errorf("get hierarchy for %s: %w", roomID, err)
			}
			for _, child := range resp.Rooms {
				roomDetails[child.RoomID.String()] = Room{ID: child.RoomID.String(), Name: child.Name}
				if child.RoomID == roomID {
					for _, relation := range child.ChildrenState {
						if relation.Type == event.StateSpaceChild && relation.GetStateKey() != "" {
							directIDs = append(directIDs, relation.GetStateKey())
						}
					}
				}
			}
			if resp.NextBatch == "" {
				break
			}
			from = resp.NextBatch
		}
		seen := make(map[string]bool)
		for _, childID := range directIDs {
			if seen[childID] {
				continue
			}
			seen[childID] = true
			child := roomDetails[childID]
			if child.ID == "" {
				child.ID = childID
			}
			space.Children = append(space.Children, child)
		}
		spaces = append(spaces, space)
	}
	return spaces, nil
}

func (c *Client) RoomInfo(ctx context.Context, room string) (RoomInfo, error) {
	roomID, err := c.resolveRoom(ctx, room)
	if err != nil {
		return RoomInfo{}, err
	}
	state, err := c.raw.State(ctx, roomID)
	if err != nil {
		return RoomInfo{}, err
	}
	info := RoomInfo{Room: Room{ID: roomID.String()}}
	if evt := state[event.StateRoomName][""]; evt != nil {
		info.Name = evt.Content.AsRoomName().Name
	}
	for parentID, evt := range state[event.StateSpaceParent] {
		if len(evt.Content.AsSpaceParent().Via) == 0 {
			continue
		}
		parent := Room{ID: parentID}
		var name event.RoomNameEventContent
		if err := c.raw.StateEvent(ctx, id.RoomID(parentID), event.StateRoomName, "", &name); err == nil {
			parent.Name = name.Name
		}
		info.Spaces = append(info.Spaces, parent)
	}
	return info, nil
}

func (c *Client) resolveRoom(ctx context.Context, room string) (id.RoomID, error) {
	if strings.HasPrefix(room, "!") {
		return id.RoomID(room), nil
	}
	if strings.HasPrefix(room, "#") {
		resp, err := c.raw.ResolveAlias(ctx, id.RoomAlias(room))
		if err != nil {
			return "", err
		}
		return resp.RoomID, nil
	}
	return "", fmt.Errorf("room must be a room ID (!...) or alias (#...)")
}

func (c *Client) prepareRoomEncryption(ctx context.Context, roomID id.RoomID) error {
	if c.crypto == nil {
		return nil
	}
	state, err := c.raw.State(ctx, roomID)
	if err != nil {
		return fmt.Errorf("load room encryption state: %w", err)
	}
	for _, byStateKey := range state {
		for _, evt := range byStateKey {
			c.raw.StateStoreSyncHandler(ctx, evt)
		}
	}
	return nil
}

func (c *Client) Send(ctx context.Context, room, body string) error {
	roomID, err := c.resolveRoom(ctx, room)
	if err != nil {
		return err
	}
	if err = c.prepareRoomEncryption(ctx, roomID); err != nil {
		return err
	}
	_, err = c.raw.SendText(ctx, roomID, body)
	return err
}

func (c *Client) SendThread(ctx context.Context, room, rootEventID, body string) error {
	roomID, err := c.resolveRoom(ctx, room)
	if err != nil {
		return err
	}
	if err = c.prepareRoomEncryption(ctx, roomID); err != nil {
		return err
	}
	content := &event.MessageEventContent{MsgType: event.MsgText, Body: body, RelatesTo: &event.RelatesTo{}}
	content.RelatesTo.SetThread(id.EventID(rootEventID), id.EventID(rootEventID))
	_, err = c.raw.SendMessageEvent(ctx, roomID, event.EventMessage, content)
	return err
}

func (c *Client) RecentMessages(ctx context.Context, room, from string, limit int) (MessagePage, error) {
	roomID, err := c.resolveRoom(ctx, room)
	if err != nil {
		return MessagePage{}, err
	}
	resp, err := c.raw.Messages(ctx, roomID, from, "", mautrix.DirectionBackward, nil, limit)
	if err != nil {
		return MessagePage{}, err
	}
	page := MessagePage{Messages: make([]Message, 0, len(resp.Chunk)), Next: resp.End}
	for i := len(resp.Chunk) - 1; i >= 0; i-- {
		evt := resp.Chunk[i]
		if evt.Type == event.EventEncrypted && c.crypto != nil {
			decrypted, decryptErr := c.crypto.Decrypt(ctx, evt)
			if decryptErr != nil {
				continue
			}
			evt = decrypted
		}
		if msg, ok := MessageFromEvent(evt); ok {
			page.Messages = append(page.Messages, msg)
		}
	}
	return page, nil
}

// Subscribe emits new plain-text room messages. Events from the initial sync are
// skipped, so callers receive a live stream rather than recent history.
// An empty room subscribes to all joined rooms.
func (c *Client) Subscribe(ctx context.Context, room string) (<-chan Message, <-chan error) {
	messages := make(chan Message, 64)
	errs := make(chan error, 1)

	go func() {
		defer close(messages)
		defer close(errs)

		var wanted id.RoomID
		if room != "" {
			var err error
			wanted, err = c.resolveRoom(ctx, room)
			if err != nil {
				errs <- err
				return
			}
		}

		syncer, ok := c.raw.Syncer.(*mautrix.DefaultSyncer)
		if !ok {
			errs <- errors.New("unsupported Matrix syncer")
			return
		}
		syncer.OnEventType(event.EventMessage, func(eventCtx context.Context, evt *event.Event) {
			// The first sync contains recent timeline events, not just new events.
			since, _ := eventCtx.Value(mautrix.SyncTokenContextKey).(string)
			if since == "" || (wanted != "" && evt.RoomID != wanted) {
				return
			}
			msg, ok := MessageFromEvent(evt)
			if !ok {
				return
			}
			select {
			case messages <- msg:
			case <-ctx.Done():
			}
		})
		if err := c.raw.SyncWithContext(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errs <- err
		}
	}()
	return messages, errs
}

func MessageFromEvent(evt *event.Event) (Message, bool) {
	if evt == nil || evt.Type != event.EventMessage {
		return Message{}, false
	}
	if evt.Content.Parsed == nil {
		if len(evt.Content.VeryRaw) == 0 && evt.Content.Raw != nil {
			evt.Content.VeryRaw, _ = json.Marshal(evt.Content.Raw)
		}
		if err := evt.Content.ParseRaw(evt.Type); err != nil {
			return Message{}, false
		}
	}
	content := evt.Content.AsMessage()
	if content.MsgType != event.MsgText && content.MsgType != event.MsgNotice && content.MsgType != event.MsgEmote {
		return Message{}, false
	}
	if content.Body == "" {
		return Message{}, false
	}
	msg := Message{RoomID: evt.RoomID.String(), Sender: evt.Sender.String(), Body: content.Body, EventID: evt.ID.String()}
	if content.RelatesTo != nil {
		msg.ThreadRoot = content.RelatesTo.GetThreadParent().String()
	}
	if evt.Timestamp > 0 {
		msg.Timestamp = time.UnixMilli(evt.Timestamp)
	}
	return msg, true
}

// Verify starts SAS verification with another device belonging to the logged-in user.
func (c *Client) Verify(ctx context.Context, in io.Reader, out io.Writer) error {
	if c.crypto == nil {
		return errors.New("end-to-end encryption is unavailable; rebuild with `-tags goolm`")
	}
	return c.crypto.Verify(ctx, in, out)
}

func (c *Client) Logout(ctx context.Context) error {
	_, err := c.raw.Logout(ctx)
	return err
}
