package turnc

import (
	"context"
	"errors"
	"fmt"
	"net"

	"go.uber.org/zap"

	"github.com/977812671/stun"
	"github.com/977812671/turn"
)

// Allocation reflects TURN Allocation, which is basically an IP:Port on
// TURN server allocated for client.
type Allocation struct {
	log       *zap.Logger
	client    *Client
	relayed   turn.RelayedAddress
	reflexive stun.XORMappedAddress
	perms     []*Permission // protected with client.mux
	minBound  turn.ChannelNumber
	integrity stun.MessageIntegrity
	nonce     stun.Nonce
}

func (a *Allocation) removePermission(p *Permission) {
	a.client.mux.Lock()
	newPerms := make([]*Permission, 0, len(a.perms))
	for _, permission := range a.perms {
		if p == permission {
			continue
		}
		newPerms = append(newPerms, permission)
	}
	a.perms = newPerms
	a.client.mux.Unlock()
}

var errUnauthorised = errors.New("unauthorised")

// allocate expects client.mux locked.
func (c *Client) allocate(req, res *stun.Message) (*Allocation, error) {
	if doErr := c.do(req, res); doErr != nil {
		return nil, doErr
	}
	if res.Type == stun.NewType(stun.MethodAllocate, stun.ClassSuccessResponse) {
		var (
			relayed   turn.RelayedAddress
			reflexive stun.XORMappedAddress
			nonce     stun.Nonce
		)
		// Getting relayed and reflexive addresses from response.
		if err := relayed.GetFrom(res); err != nil {
			return nil, err
		}
		if err := reflexive.GetFrom(res); err != nil && err != stun.ErrAttributeNotFound {
			return nil, err
		}
		// Getting nonce from request.
		if err := nonce.GetFrom(req); err != nil && err != stun.ErrAttributeNotFound {
			return nil, err
		}
		a := &Allocation{
			client:    c,
			log:       c.log,
			reflexive: reflexive,
			relayed:   relayed,
			minBound:  turn.MinChannelNumber,
			integrity: c.integrity,
			nonce:     nonce,
		}
		c.alloc = a
		return a, nil
	}
	// Anonymous allocate failed, trying to authenticate.
	if res.Type.Method != stun.MethodAllocate {
		return nil, fmt.Errorf("unexpected response type %s", res.Type)
	}
	var (
		code stun.ErrorCodeAttribute
	)
	if err := code.GetFrom(res); err != nil {
		return nil, err
	}
	if code.Code != stun.CodeUnauthorized {
		return nil, fmt.Errorf("unexpected error code %d", code)
	}
	return nil, errUnauthorised
}
func (c *Client) AllocateUDP() (*Allocation, error) {
	return c.Allocate(turn.ProtoUDP)
}
func (c *Client) AllocateTCP() (*Allocation, error) {
	return c.Allocate(turn.ProtoTCP)
}

// Allocate creates an allocation for current 5-tuple. Currently there can be
// only one allocation per client, because client wraps one net.Conn.
func (c *Client) Allocate(proto turn.Protocol) (*Allocation, error) {
	var (
		nonce stun.Nonce
		res   = stun.New()
	)
	var protoSetter stun.Setter
	if proto == turn.ProtoUDP {
		protoSetter = turn.RequestedTransportUDP
	} else if proto == turn.ProtoTCP {
		protoSetter = turn.RequestedTransportTCP
	} else {
		return nil, fmt.Errorf("Invalid protocol supplied")
	}

	req, reqErr := stun.Build(stun.TransactionID,
		turn.AllocateRequest, protoSetter,
		stun.Fingerprint,
	)
	if reqErr != nil {
		return nil, reqErr
	}
	a, allocErr := c.allocate(req, res)
	if allocErr == nil {
		return a, nil
	}
	if allocErr != errUnauthorised {
		return nil, allocErr
	}
	// Anonymous allocate failed, trying to authenticate.
	if err := nonce.GetFrom(res); err != nil {
		return nil, err
	}
	if err := c.realm.GetFrom(res); err != nil {
		return nil, err
	}
	c.realm = append([]byte(nil), c.realm...)
	c.integrity = stun.NewLongTermIntegrity(
		c.username.String(), c.realm.String(), c.password,
	)
	// Trying to authorize.
	if reqErr = req.Build(stun.TransactionID,
		turn.AllocateRequest, protoSetter,
		&c.username, &c.realm,
		&nonce,
		&c.integrity, stun.Fingerprint,
	); reqErr != nil {
		return nil, reqErr
	}
	return c.allocate(req, res)
}

func (a *Allocation) allocate(peer turn.PeerAddress) error {
	req := stun.New()
	req.TransactionID = stun.NewTransactionID()
	req.Type = stun.NewType(stun.MethodCreatePermission, stun.ClassRequest)
	req.WriteHeader()
	setters := make([]stun.Setter, 0, 10)
	setters = append(setters, &peer)
	if len(a.integrity) > 0 {
		// Applying auth.
		setters = append(setters,
			a.nonce, a.client.username, a.client.realm, a.integrity,
		)
	}
	setters = append(setters, stun.Fingerprint)
	for _, s := range setters {
		if setErr := s.AddTo(req); setErr != nil {
			return setErr
		}
	}
	res := stun.New()
	if doErr := a.client.do(req, res); doErr != nil {
		return doErr
	}
	if res.Type.Class == stun.ClassErrorResponse {
		var code stun.ErrorCodeAttribute
		err := fmt.Errorf("unexpected error response: %s", res.Type)
		if getErr := code.GetFrom(res); getErr == nil {
			err = fmt.Errorf("unexpected error response: %s (error %s)",
				res.Type, code,
			)
		}
		return err
	}
	return nil
}

// Relayed returns the relayed address for the allocation
func (a *Allocation) Relayed() turn.RelayedAddress {
	return a.relayed
}

// CreateUDP creates new UDP Permission to peer with provided addr.
func (a *Allocation) Create(ip net.IP) (*Permission, error) {
	peer := turn.PeerAddress{
		IP:   ip,
		Port: 0, // Does not matter.
	}
	if err := a.allocate(peer); err != nil {
		return nil, err
	}
	p := &Permission{
		log:         a.log,
		ip:          ip,
		client:      a.client,
		refreshRate: a.client.refreshRate,
	}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.startRefreshLoop()
	a.client.mux.Lock()
	a.perms = append(a.perms, p)
	a.client.mux.Unlock()
	return p, nil
}
