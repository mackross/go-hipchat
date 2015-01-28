package hipchat

import (
	"errors"
	"github.com/mackross/hipchat/xmpp"
	"strings"
	"time"
)

var (
	host = "chat.hipchat.com"
	conf = "conf.hipchat.com"
)

// A Client represents the connection between the application to the HipChat
// service.
type Client struct {
	Username string
	Password string
	Resource string
	Id       string

	// private
	mentionNames    map[string]string
	connection      *xmpp.Conn
	receivedUsers   chan []*User
	receivedRooms   chan []*Room
	receivedMessage chan *Message
	onConnect       chan bool
}

// A Message represents a message received from HipChat.
type Message struct {
	ID   string
	From string
	To   string
	Body string
	Type string
}

// A User represents a member of the HipChat service.
type User struct {
	Id          string
	Name        string
	MentionName string
}

// A Room represents a room in HipChat the Client can join to communicate with
// other members..
type Room struct {
	Id   string
	Name string
}

// NewClient creates a new Client connection from the user name, password and
// resource passed to it.
func NewClient(user, pass, resource string) (*Client, error) {

	c := &Client{
		Username: user,
		Password: pass,
		Resource: resource,
		Id:       user + "@" + host,

		// private
		mentionNames:    make(map[string]string),
		receivedUsers:   make(chan []*User),
		receivedRooms:   make(chan []*Room),
		receivedMessage: make(chan *Message),
		onConnect:       make(chan bool),
	}

	err := c.connect()
	return c, err
}

func (c *Client) connect() error {
	connection, err := xmpp.Dial(host)
	c.connection = connection
	if err != nil {
		return err
	}
	err = c.authenticate()
	if err != nil {
		return err
	}
	go c.listen()
	go func() { c.onConnect <- true }()
	return nil
}

// OnConnect returns a read-only channel of booleans and sends true
// when ever the client connects or reconnects.
func (c *Client) OnConnect() <-chan bool {
	return c.onConnect
}

// Messages returns a read-only channel of Message structs. After joining a
// room, messages will be sent on the channel.
func (c *Client) Messages() <-chan *Message {
	return c.receivedMessage
}

// Rooms returns an slice of Room structs.
func (c *Client) Rooms() []*Room {
	c.requestRooms()
	return <-c.receivedRooms
}

// Users returns a slice of User structs.
func (c *Client) Users() []*User {
	c.requestUsers()
	return <-c.receivedUsers
}

// Status sends a string to HipChat to indicate whether the client is available
// to chat, away or idle.
func (c *Client) Status(s string) {
	c.connection.Presence(c.Id, s)
}

// Join accepts the room id and the name used to display the client in the
// room.
func (c *Client) Join(roomId, resource string) {
	c.connection.MUCPresence(roomId+"/"+resource, c.Id)
}

// Say accepts a room id, the name of the client in the room, and the message
// body and sends the message to the HipChat room.
func (c *Client) Say(to, name, body string) {
	if strings.Contains(to, conf) {
		c.connection.MUCSend(to, c.Id+"/"+name, body)
	} else {
		c.connection.Send(to, c.Id+"/"+name, body)
	}
}

// KeepAlive is meant to run as a goroutine. It sends a single whitespace
// character to HipChat every 60 seconds. This keeps the connection from
// idling after 150 seconds.
func (c *Client) KeepAlive() {
	for _ = range time.Tick(60 * time.Second) {
		c.connection.KeepAlive()
	}
}

func (c *Client) requestRooms() {
	c.connection.Discover(c.Id, conf)
}

func (c *Client) requestUsers() {
	c.connection.Roster(c.Id, host)
}

func (c *Client) authenticate() error {
	c.connection.Stream(c.Id, host)
	for {
		element, err := c.connection.Next()
		if err != nil {
			return err
		}

		switch element.Name.Local + element.Name.Space {
		case "stream" + xmpp.NsStream:
			features := c.connection.Features()
			if features.StartTLS != nil {
				c.connection.StartTLS()
			} else {
				for _, m := range features.Mechanisms {
					if m == "PLAIN" {
						c.connection.Auth(c.Username, c.Password, c.Resource)
					}
				}
			}
		case "proceed" + xmpp.NsTLS:
			c.connection.UseTLS()
			c.connection.Stream(c.Id, host)
		case "iq" + xmpp.NsJabberClient:
			for _, attr := range element.Attr {
				if attr.Name.Local == "type" && attr.Value == "result" {
					return nil // authenticated
				}
			}

			return errors.New("could not authenticate")
		}
	}

	return errors.New("unexpectedly ended auth loop")
}

func (c *Client) listen() {
	for {
		element, err := c.connection.Next()
		if err != nil {
			for m := 0; m < 5; m++ {
				for i := 1; i < 11; i++ {
					time.Sleep(time.Duration(i) * time.Second)
					err = c.connect()
					if err != nil {
						goto Reconnected
					}
				}
				time.Sleep(time.Duration(m) * time.Minute)
			}
			panic(err)
		Reconnected:
			continue
		}

		switch element.Name.Local + element.Name.Space {
		case "iq" + xmpp.NsJabberClient: // rooms and rosters
			//attr := xmpp.ToMap(element.Attr)
			query := c.connection.Query()
			//fmt.Printf("<%v: %#v\n>%v\n\n", element.Name.Local, attr, query)
			switch query.XMLName.Space {
			case xmpp.NsDisco:
				items := make([]*Room, len(query.Items))
				for i, item := range query.Items {
					items[i] = &Room{Id: item.Jid, Name: item.Name}
				}
				c.receivedRooms <- items
			case xmpp.NsIqRoster:
				items := make([]*User, len(query.Items))
				for i, item := range query.Items {
					items[i] = &User{Id: item.Jid, Name: item.Name, MentionName: item.MentionName}
				}
				c.receivedUsers <- items
			}
		case "presence" + xmpp.NsJabberClient:
			//attr := xmpp.ToMap(element.Attr)
			//body := c.connection.Body()
			//fmt.Printf("<%v: %#v\n>%v\n\n", element.Name.Local, attr, body)
		case "message" + xmpp.NsJabberClient:
			attr := xmpp.ToMap(element.Attr)
			if attr["type"] != "groupchat" && attr["type"] != "chat" {
				continue
			}

			// empty body indicates a toggle in typing status
			body := c.connection.Body()
			//fmt.Printf("<%v: %#v\n>%v\n\n", element.Name.Local, attr, body)
			if len(body) == 0 {
				continue
			}

			c.receivedMessage <- &Message{
				ID:   attr["mid"],
				Type: attr["type"],
				From: attr["from"],
				To:   attr["to"],
				Body: body,
			}
		}
	}
}
