package relaypool

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/fiatjaf/go-nostr/event"
	"github.com/fiatjaf/go-nostr/filter"
	nostrutils "github.com/fiatjaf/go-nostr/utils"
	"github.com/gorilla/websocket"
)

type RelayPool struct {
	SecretKey *string

	Relays        map[string]Policy
	websockets    map[string]*websocket.Conn
	subscriptions map[string]*Subscription

	Notices chan *NoticeMessage
}

type Policy struct {
	SimplePolicy
	ReadSpecific map[string]SimplePolicy
}

type SimplePolicy struct {
	Read  bool
	Write bool
}

type NoticeMessage struct {
	Message string
	Relay   string
}

func (nm *NoticeMessage) UnmarshalJSON(b []byte) error {
	var temp []json.RawMessage
	if err := json.Unmarshal(b, &temp); err != nil {
		return err
	}
	if len(temp) < 2 {
		return errors.New("message is not an array of 2 or more")
	}
	var tag string
	if err := json.Unmarshal(temp[0], &tag); err != nil {
		return err
	}
	if tag != "notice" {
		return errors.New("tag is not 'notice'")
	}

	if err := json.Unmarshal(temp[1], &nm.Message); err != nil {
		return err
	}
	return nil
}

// New creates a new RelayPool with no relays in it
func New() *RelayPool {
	return &RelayPool{
		Relays:     make(map[string]Policy),
		websockets: make(map[string]*websocket.Conn),

		Notices: make(chan *NoticeMessage),
	}
}

// Add adds a new relay to the pool, if policy is nil, it will be a simple
// read+write policy.
func (r *RelayPool) Add(url string, policy *Policy) error {
	if policy == nil {
		policy = &Policy{SimplePolicy: SimplePolicy{Read: true, Write: true}}
	}

	nm := nostrutils.NormalizeURL(url)
	if nm == "" {
		return fmt.Errorf("invalid relay URL '%s'", url)
	}

	conn, _, err := websocket.DefaultDialer.Dial(nostrutils.NormalizeURL(url), nil)
	if err != nil {
		return fmt.Errorf("error opening websocket to '%s': %w", nm, err)
	}

	r.Relays[nm] = *policy
	r.websockets[nm] = conn

	for _, sub := range r.subscriptions {
		sub.addRelay(nm, conn)
	}

	go func() {
		for {
			typ, message, err := conn.ReadMessage()
			if err != nil {
				log.Println("read error: ", err)
				return
			}
			if typ == websocket.PingMessage {
				conn.WriteMessage(websocket.PongMessage, nil)
			}

			if typ != websocket.TextMessage || len(message) == 0 || message[0] != '[' {
				continue
			}

			var jsonMessage []json.RawMessage
			err = json.Unmarshal(message, &jsonMessage)
			if err != nil {
				continue
			}

			if len(jsonMessage) < 2 {
				continue
			}

			var label string
			json.Unmarshal(jsonMessage[0], &label)

			switch label {
			case "NOTICE":
				var content string
				json.Unmarshal(jsonMessage[1], &content)
				r.Notices <- &NoticeMessage{
					Relay:   nm,
					Message: content,
				}
			case "EVENT":
				if len(jsonMessage) < 3 {
					continue
				}

				var channel string
				json.Unmarshal(jsonMessage[1], &channel)
				if subscription, ok := r.subscriptions[channel]; ok {
					var event event.Event
					json.Unmarshal(jsonMessage[2], &event)
					ok, _ := event.CheckSignature()
					if !ok {
						continue
					}

					subscription.Events <- EventMessage{
						Relay: nm,
						Event: event,
					}
				}
			}
		}
	}()

	return nil
}

// Remove removes a relay from the pool.
func (r *RelayPool) Remove(url string) {
	nm := nostrutils.NormalizeURL(url)

	for _, sub := range r.subscriptions {
		sub.removeRelay(nm)
	}
	if conn, ok := r.websockets[nm]; ok {
		conn.Close()
	}

	delete(r.Relays, nm)
	delete(r.websockets, nm)
}

func (r *RelayPool) Sub(filter filter.EventFilter) *Subscription {
	random := make([]byte, 7)
	rand.Read(random)

	subscription := Subscription{}
	subscription.channel = hex.EncodeToString(random)
	subscription.relays = make(map[string]*websocket.Conn)
	for relay, policy := range r.Relays {
		if policy.Read {
			ws := r.websockets[relay]
			subscription.relays[relay] = ws
		}
	}
	subscription.Events = make(chan EventMessage)
	r.subscriptions[subscription.channel] = &subscription

	subscription.Sub(&filter)
	return &subscription
}

func (r *RelayPool) PublishEvent(evt *event.Event) (*event.Event, chan PublishStatus, error) {
	status := make(chan PublishStatus)

	if r.SecretKey == nil && evt.Sig == "" {
		return nil, status, errors.New("PublishEvent needs either a signed event to publish or to have been configured with a .SecretKey.")
	}

	if evt.Sig == "" {
		err := evt.Sign(*r.SecretKey)
		if err != nil {
			return nil, status, fmt.Errorf("Error signing event: %w", err)
		}
	}

	jevt, _ := json.Marshal(evt)
	for relay, conn := range r.websockets {
		go func(relay string, conn *websocket.Conn) {
			err := conn.WriteJSON([]interface{}{"EVENT", jevt})
			if err != nil {
				log.Printf("error sending event to '%s': %s", relay, err.Error())
				status <- PublishStatus{relay, PublishStatusFailed}
			}
			status <- PublishStatus{relay, PublishStatusSent}

			subscription := r.Sub(filter.EventFilter{ID: evt.ID})

			time.Sleep(5 * time.Second)
			subscription.Unsub()
		}(relay, conn)
	}

	return evt, status, nil
}
