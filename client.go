package turnc

import (
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/977812671/stun"
	"github.com/977812671/turn"
)

// Client for TURN server.
//
// Provides transparent net.Conn interfaces to remote peers.
type Client struct {
	log         *zap.Logger
	con         net.Conn
	conClose    bool
	stun        STUNClient
	mux         sync.RWMutex
	username    stun.Username
	password    string
	realm       stun.Realm
	integrity   stun.MessageIntegrity
	alloc       *Allocation // the only allocation
	refreshRate time.Duration
	done        chan struct{}
}

// Options contains available config for TURN  client.
type Options struct {
	Conn net.Conn
	STUN STUNClient  // optional STUN client
	Log  *zap.Logger // defaults to Nop

	// Long-term integrity.
	Username string
	Password string

	// STUN client options.
	RTO          time.Duration
	NoRetransmit bool

	// TURN options.
	RefreshRate     time.Duration
	RefreshDisabled bool

	// ConnManualClose disables connection automatic close on Close().
	ConnManualClose bool
}

// RefreshRate returns current rate of refresh requests.
func (c *Client) RefreshRate() time.Duration { return c.refreshRate }

const defaultRefreshRate = time.Minute

// New creates and initializes new TURN client.
func New(o Options) (*Client, error) {
	if o.Conn == nil {
		return nil, errors.New("connection not provided")
	}
	if o.Log == nil {
		o.Log = zap.NewNop()
	}
	c := &Client{
		password: o.Password,
		log:      o.Log,
		conClose: true,
	}
	if o.ConnManualClose {
		o.Log.Debug("manual close is enabled")
		c.conClose = false
	}
	if o.STUN == nil {
		// Setting up de-multiplexing.
		m := newMultiplexer(o.Conn, c.log)
		go m.discardData() // discarding any non-stun/turn data
		o.Conn = bypassWriter{
			reader: m.turnL,
			writer: m.conn,
		}
		// Starting STUN client on multiplexed connection.
		var err error
		stunOptions := []stun.ClientOption{
			stun.WithHandler(c.stunHandler),
		}
		if o.NoRetransmit {
			stunOptions = append(stunOptions, stun.WithNoRetransmit)
		}
		if o.RTO > 0 {
			stunOptions = append(stunOptions, stun.WithRTO(o.RTO))
		}
		o.STUN, err = stun.NewClient(bypassWriter{
			reader: m.stunL,
			writer: m.conn,
		}, stunOptions...)
		if err != nil {
			return nil, err
		}
	}
	c.done = make(chan struct{})
	c.stun = o.STUN
	c.con = o.Conn
	c.refreshRate = defaultRefreshRate
	if o.RefreshRate > 0 {
		c.refreshRate = o.RefreshRate
	}
	if o.RefreshDisabled {
		c.refreshRate = 0
	}
	if o.Username != "" {
		c.username = stun.NewUsername(o.Username)
	}
	go c.readUntilClosed()
	return c, nil
}

// STUNClient abstracts STUN protocol interaction.
type STUNClient interface {
	Indicate(m *stun.Message) error
	Do(m *stun.Message, f func(e stun.Event)) error
	Close() error
}

var dataIndication = stun.NewType(stun.MethodData, stun.ClassIndication)

func (c *Client) stunHandler(e stun.Event) {
	if e.Error != nil {
		// Just ignoring.
		return
	}
	if e.Message.Type != dataIndication {
		return
	}
	var (
		data turn.Data
		addr turn.PeerAddress
	)
	if err := e.Message.Parse(&data, &addr); err != nil {
		c.log.Error("failed to parse while handling incoming STUN message", zap.Error(err))
		return
	}
	c.mux.RLock()
	for i := range c.alloc.perms {
		for j := range c.alloc.perms[i].conn {
			if !turn.Addr(c.alloc.perms[i].conn[j].peerAddr).Equal(turn.Addr(addr)) {
				continue
			}
			if _, err := c.alloc.perms[i].conn[j].peerL.Write(data); err != nil {
				c.log.Error("failed to write", zap.Error(err))
			}
		}

	}
	c.mux.RUnlock()
}

func (c *Client) handleChannelData(data *turn.ChannelData) {
	c.log.Debug("handleChannelData", zap.Int("n", int(data.Number)))
	c.mux.RLock()
	for i := range c.alloc.perms {
		for j := range c.alloc.perms[i].conn {
			if data.Number != c.alloc.perms[i].conn[j].Binding() {
				continue
			}
			if _, err := c.alloc.perms[i].conn[j].peerL.Write(data.Data); err != nil {
				c.log.Error("failed to write", zap.Error(err))
			}
		}
	}
	c.mux.RUnlock()
}

func (c *Client) readUntilClosed() {
	buf := make([]byte, 2048)
	datBuf := make([]byte, 0)
	for {
		n, err := c.con.Read(buf)
		if err != nil {
			if err == io.EOF {
				continue
			}
			c.log.Debug("read error", zap.Error(err))
			c.log.Info("connection closed")
			break
		}

		data := append(datBuf, buf[:n]...)
		if !turn.IsChannelData(data) {
			continue
		}
		cData := &turn.ChannelData{
			Raw: make([]byte, len(data)),
		}
		copy(cData.Raw, data)
		if err := cData.Decode(); err != nil {
			// channelData length != len(Data)
			// readmore
			fmt.Println("channelData length != len(Data)")
			//panic(err)
		} else {
			datBuf = make([]byte, 0)
			go c.handleChannelData(cData)
		}
	}
	close(c.done)
}

func (c *Client) sendData(buf []byte, peerAddr *turn.PeerAddress) (int, error) {
	err := c.stun.Indicate(stun.MustBuild(stun.TransactionID,
		stun.NewType(stun.MethodSend, stun.ClassIndication),
		turn.Data(buf), peerAddr,
	))
	if err == nil {
		return len(buf), nil
	}
	return 0, err
}

func (c *Client) sendChan(buf []byte, n turn.ChannelNumber) (int, error) {
	if !n.Valid() {
		return 0, turn.ErrInvalidChannelNumber
	}
	d := &turn.ChannelData{
		Data:   buf,
		Number: n,
	}
	d.Encode()
	return c.con.Write(d.Raw)
}

func (c *Client) do(req, res *stun.Message) error {
	var stunErr error
	if doErr := c.stun.Do(req, func(e stun.Event) {
		if e.Error != nil {
			stunErr = e.Error
			return
		}
		if res == nil {
			return
		}
		if err := e.Message.CloneTo(res); err != nil {
			stunErr = err
		}
	}); doErr != nil {
		return doErr
	}
	return stunErr
}

func (c *Client) Close() error {
	if !c.conClose {
		// TODO(ernado): Cleanup all resources.
		return nil
	}
	c.log.Error("closing connection")
	if err := c.con.Close(); err != nil {
		return err
	}
	if err := c.stun.Close(); err != nil {
		c.log.Error("failed to close stun client", zap.Error(err))
	}
	<-c.done
	c.log.Error("done signaled")
	return nil
}
