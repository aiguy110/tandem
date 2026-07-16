package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ScreencastFrame is a browser image and the viewport metadata reported by CDP.
type ScreencastFrame struct {
	DataB64 string             `json:"dataB64"`
	Meta    ScreencastMetadata `json:"meta"`
}

type ScreencastMetadata struct {
	DeviceWidth  float64 `json:"deviceWidth"`
	DeviceHeight float64 `json:"deviceHeight"`
	OffsetTop    float64 `json:"offsetTop"`
	Timestamp    float64 `json:"timestamp"`
}

// BrowserInputEvent is the normalized browser_input.event wire shape.
type BrowserInputEvent struct {
	Kind       string
	X, Y       float64
	Button     string
	Buttons    int
	ClickCount int
	DeltaX     float64
	DeltaY     float64
	Key        string
	Code       string
	KeyCode    int
	Text       string
}

type cdpResponse struct {
	Result json.RawMessage
	Err    error
}

type cdpEnvelope struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// SharedBrowser is the daemon-owned, ungated CDP connection used by the human
// browser pane. It intentionally dials Chrome directly rather than Broker.EndpointFor.
type SharedBrowser struct {
	wsURL string

	mu            sync.Mutex
	conn          *websocket.Conn
	sessionID     string
	pending       map[int64]chan cdpResponse
	nextID        int64
	closed        bool
	desiredCast   bool
	casting       bool
	reconnecting  bool
	frameCallback func(ScreencastFrame)
	writeMu       sync.Mutex
	connectMu     sync.Mutex
	closeOnce     sync.Once
	closedCh      chan struct{}
}

func NewSharedBrowser(browserWSURL string) *SharedBrowser {
	return &SharedBrowser{wsURL: browserWSURL, pending: make(map[int64]chan cdpResponse), closedCh: make(chan struct{})}
}

func (s *SharedBrowser) Connect(ctx context.Context) error {
	s.connectMu.Lock()
	defer s.connectMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("shared browser closed")
	}
	if s.conn != nil && s.sessionID != "" {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, s.wsURL, nil)
	if err != nil {
		return fmt.Errorf("connect direct CDP: %w", err)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = conn.Close()
		return errors.New("shared browser closed")
	}
	s.conn = conn
	s.sessionID = ""
	s.casting = false
	s.mu.Unlock()
	go s.readLoop(conn)

	var targets struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
			URL      string `json:"url"`
		} `json:"targetInfos"`
	}
	if err := s.callOn(ctx, conn, "", "Target.getTargets", map[string]any{}, &targets); err != nil {
		s.disconnect(conn, err)
		return err
	}
	targetID := ""
	for _, target := range targets.TargetInfos {
		if target.Type == "page" {
			targetID = target.TargetID
			break
		}
	}
	if targetID == "" {
		var created struct {
			TargetID string `json:"targetId"`
		}
		if err := s.callOn(ctx, conn, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &created); err != nil {
			s.disconnect(conn, err)
			return err
		}
		targetID = created.TargetID
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := s.callOn(ctx, conn, "", "Target.attachToTarget", map[string]any{"targetId": targetID, "flatten": true}, &attached); err != nil {
		s.disconnect(conn, err)
		return err
	}
	if attached.SessionID == "" {
		s.disconnect(conn, errors.New("CDP attach returned no session"))
		return errors.New("CDP attach returned no session")
	}
	s.mu.Lock()
	s.sessionID = attached.SessionID
	desired := s.desiredCast
	s.mu.Unlock()
	if desired {
		if err := s.startRemoteCast(ctx, conn, attached.SessionID); err != nil {
			s.disconnect(conn, err)
			return err
		}
	}
	return nil
}

func (s *SharedBrowser) IsConnected() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn != nil && s.sessionID != "" && !s.closed
}

func (s *SharedBrowser) StartScreencast(ctx context.Context, callback func(ScreencastFrame)) error {
	s.mu.Lock()
	s.frameCallback = callback
	s.desiredCast = true
	already := s.casting
	s.mu.Unlock()
	if already {
		return nil
	}
	if err := s.Connect(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	conn, session := s.conn, s.sessionID
	already = s.casting
	s.mu.Unlock()
	if already {
		return nil
	}
	return s.startRemoteCast(ctx, conn, session)
}

func (s *SharedBrowser) startRemoteCast(ctx context.Context, conn *websocket.Conn, session string) error {
	err := s.callOn(ctx, conn, session, "Page.startScreencast", map[string]any{
		"format": "jpeg", "quality": 70, "maxWidth": 1280, "maxHeight": 800, "everyNthFrame": 1,
	}, nil)
	if err == nil {
		s.mu.Lock()
		if s.conn == conn && s.sessionID == session {
			s.casting = true
		}
		s.mu.Unlock()
	}
	return err
}

func (s *SharedBrowser) StopScreencast(ctx context.Context) error {
	s.mu.Lock()
	s.frameCallback = nil
	s.desiredCast = false
	if !s.casting || s.conn == nil {
		s.casting = false
		s.mu.Unlock()
		return nil
	}
	conn, session := s.conn, s.sessionID
	s.casting = false
	s.mu.Unlock()
	return s.callOn(ctx, conn, session, "Page.stopScreencast", map[string]any{}, nil)
}

func (s *SharedBrowser) IsCasting() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.casting
}

func (s *SharedBrowser) Dispatch(ctx context.Context, event BrowserInputEvent) error {
	if err := s.Connect(ctx); err != nil {
		return err
	}
	method := "Input.dispatchMouseEvent"
	params := map[string]any{"x": event.X, "y": event.Y}
	switch event.Kind {
	case "mousemove":
		params["type"], params["buttons"] = "mouseMoved", event.Buttons
	case "mousedown":
		params["type"], params["button"], params["buttons"] = "mousePressed", defaultString(event.Button, "left"), defaultInt(event.Buttons, 1)
		params["clickCount"] = defaultInt(event.ClickCount, 1)
	case "mouseup":
		params["type"], params["button"], params["buttons"] = "mouseReleased", defaultString(event.Button, "left"), event.Buttons
		params["clickCount"] = defaultInt(event.ClickCount, 1)
	case "click":
		press := map[string]any{"type": "mousePressed", "x": event.X, "y": event.Y, "button": "left", "buttons": 1, "clickCount": 1}
		if err := s.sessionCall(ctx, method, press, nil); err != nil {
			return err
		}
		params = map[string]any{"type": "mouseReleased", "x": event.X, "y": event.Y, "button": "left", "buttons": 1, "clickCount": 1}
	case "wheel":
		params["type"], params["deltaX"], params["deltaY"] = "mouseWheel", event.DeltaX, event.DeltaY
	case "keydown":
		method = "Input.dispatchKeyEvent"
		params = map[string]any{"type": "keyDown", "key": event.Key, "code": event.Code, "windowsVirtualKeyCode": event.KeyCode}
		if event.Text != "" {
			params["text"] = event.Text
		}
	case "keyup":
		method = "Input.dispatchKeyEvent"
		params = map[string]any{"type": "keyUp", "key": event.Key, "code": event.Code, "windowsVirtualKeyCode": event.KeyCode}
	case "text":
		method, params = "Input.insertText", map[string]any{"text": event.Text}
	default:
		return fmt.Errorf("unknown browser input kind %q", event.Kind)
	}
	return s.sessionCall(ctx, method, params, nil)
}

func (s *SharedBrowser) Navigate(ctx context.Context, targetURL string) error {
	if err := s.Connect(ctx); err != nil {
		return err
	}
	return s.sessionCall(ctx, "Page.navigate", map[string]any{"url": targetURL}, nil)
}

func (s *SharedBrowser) sessionCall(ctx context.Context, method string, params any, out any) error {
	for attempt := 0; attempt < 2; attempt++ {
		if err := s.Connect(ctx); err != nil {
			return err
		}
		s.mu.Lock()
		conn, session := s.conn, s.sessionID
		s.mu.Unlock()
		err := s.callOn(ctx, conn, session, method, params, out)
		if err == nil || attempt == 1 || ctx.Err() != nil {
			return err
		}
		s.disconnect(conn, err)
	}
	return nil
}

func (s *SharedBrowser) callOn(ctx context.Context, conn *websocket.Conn, session, method string, params any, out any) error {
	if conn == nil {
		return errors.New("shared browser not connected")
	}
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	response := make(chan cdpResponse, 1)
	s.pending[id] = response
	s.mu.Unlock()
	message := map[string]any{"id": id, "method": method, "params": params}
	if session != "" {
		message["sessionId"] = session
	}
	payload, err := json.Marshal(message)
	if err == nil {
		s.writeMu.Lock()
		err = conn.WriteMessage(websocket.TextMessage, payload)
		s.writeMu.Unlock()
	}
	if err != nil {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return err
	}
	select {
	case got := <-response:
		if got.Err != nil {
			return got.Err
		}
		if out != nil && len(got.Result) > 0 {
			return json.Unmarshal(got.Result, out)
		}
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return ctx.Err()
	case <-s.closedCh:
		return errors.New("shared browser closed")
	}
}

func (s *SharedBrowser) readLoop(conn *websocket.Conn) {
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			s.disconnect(conn, err)
			return
		}
		var message cdpEnvelope
		if json.Unmarshal(data, &message) != nil {
			continue
		}
		if message.ID != 0 {
			s.mu.Lock()
			ch := s.pending[message.ID]
			delete(s.pending, message.ID)
			s.mu.Unlock()
			if ch != nil {
				if message.Error != nil {
					ch <- cdpResponse{Err: fmt.Errorf("CDP %d: %s", message.Error.Code, message.Error.Message)}
				} else {
					ch <- cdpResponse{Result: message.Result}
				}
			}
			continue
		}
		if message.Method == "Page.screencastFrame" {
			var frame struct {
				Data      string `json:"data"`
				SessionID int    `json:"sessionId"`
				Metadata  struct {
					DeviceWidth, DeviceHeight float64
					OffsetTop                 float64
					Timestamp                 float64
				} `json:"metadata"`
			}
			if json.Unmarshal(message.Params, &frame) != nil {
				continue
			}
			s.mu.Lock()
			callback, session := s.frameCallback, s.sessionID
			s.mu.Unlock()
			if callback != nil {
				callback(ScreencastFrame{DataB64: frame.Data, Meta: ScreencastMetadata{
					DeviceWidth: frame.Metadata.DeviceWidth, DeviceHeight: frame.Metadata.DeviceHeight,
					OffsetTop: frame.Metadata.OffsetTop, Timestamp: frame.Metadata.Timestamp,
				}})
			}
			// Chrome will not send the next frame until this one is acknowledged.
			go func(frameID int, session string) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				_ = s.callOn(ctx, conn, session, "Page.screencastFrameAck", map[string]any{"sessionId": frameID}, nil)
			}(frame.SessionID, session)
		}
	}
}

func (s *SharedBrowser) disconnect(conn *websocket.Conn, cause error) {
	s.mu.Lock()
	if s.conn != conn {
		s.mu.Unlock()
		return
	}
	s.conn, s.sessionID, s.casting = nil, "", false
	pending := s.pending
	s.pending = make(map[int64]chan cdpResponse)
	shouldReconnect := s.desiredCast && !s.closed && !s.reconnecting
	if shouldReconnect {
		s.reconnecting = true
	}
	s.mu.Unlock()
	_ = conn.Close()
	for _, ch := range pending {
		select {
		case ch <- cdpResponse{Err: cause}:
		default:
		}
	}
	if shouldReconnect {
		go s.reconnectCasting()
	}
}

func (s *SharedBrowser) reconnectCasting() {
	for {
		s.mu.Lock()
		if s.closed || !s.desiredCast {
			s.reconnecting = false
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := s.Connect(ctx)
		cancel()
		if err == nil {
			s.mu.Lock()
			s.reconnecting = false
			s.mu.Unlock()
			return
		}
		select {
		case <-s.closedCh:
			s.mu.Lock()
			s.reconnecting = false
			s.mu.Unlock()
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (s *SharedBrowser) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.desiredCast = false
		s.frameCallback = nil
		conn := s.conn
		s.conn, s.sessionID, s.casting = nil, "", false
		pending := s.pending
		s.pending = make(map[int64]chan cdpResponse)
		close(s.closedCh)
		s.mu.Unlock()
		if conn != nil {
			_ = conn.Close()
		}
		for _, ch := range pending {
			select {
			case ch <- cdpResponse{Err: errors.New("shared browser closed")}:
			default:
			}
		}
	})
	return nil
}

func defaultString(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
func defaultInt(v, fallback int) int {
	if v == 0 {
		return fallback
	}
	return v
}
