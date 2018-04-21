package rendezvous

import (
	"context"
	"fmt"
	"time"

	pb "github.com/libp2p/go-libp2p-rendezvous/pb"

	ggio "github.com/gogo/protobuf/io"
	logging "github.com/ipfs/go-log"
	host "github.com/libp2p/go-libp2p-host"
	inet "github.com/libp2p/go-libp2p-net"
	peer "github.com/libp2p/go-libp2p-peer"
	pstore "github.com/libp2p/go-libp2p-peerstore"
)

var log = logging.Logger("rendezvous")

const DefaultTTL = 2 * 3600 // 2hr

type Rendezvous interface {
	Register(ctx context.Context, ns string, ttl int) error
	Unregister(ctx context.Context, ns string) error
	Discover(ctx context.Context, ns string, limit int, cookie []byte) ([]Registration, []byte, error)
	DiscoverAsync(ctx context.Context, ns string) (<-chan Registration, error)
}

type Registration struct {
	Peer pstore.PeerInfo
	Ns   string
	Ttl  int
}

func NewRendezvousClient(host host.Host, rp peer.ID) Rendezvous {
	return &client{
		host: host,
		rp:   rp,
	}
}

type client struct {
	host host.Host
	rp   peer.ID
}

func (cli *client) Register(ctx context.Context, ns string, ttl int) error {
	s, err := cli.host.NewStream(ctx, cli.rp, RendezvousProto)
	if err != nil {
		return err
	}
	defer s.Close()

	r := ggio.NewDelimitedReader(s, 1<<20)
	w := ggio.NewDelimitedWriter(s)

	req := newRegisterMessage(ns, pstore.PeerInfo{ID: cli.host.ID(), Addrs: cli.host.Addrs()}, ttl)
	err = w.WriteMsg(req)
	if err != nil {
		return err
	}

	var res pb.Message
	err = r.ReadMsg(&res)
	if err != nil {
		return err
	}

	if res.GetType() != pb.Message_REGISTER_RESPONSE {
		return fmt.Errorf("Unexpected response: %s", res.GetType().String())
	}

	status := res.GetRegisterResponse().GetStatus()
	if status != pb.Message_OK {
		return RendezvousError{Status: status, Text: res.GetRegisterResponse().GetStatusText()}
	}

	return nil
}

func Register(ctx context.Context, rz Rendezvous, ns string) error {
	err := rz.Register(ctx, ns, DefaultTTL)
	if err != nil {
		return err
	}

	go registerRefresh(ctx, rz, ns)
	return nil
}

func registerRefresh(ctx context.Context, rz Rendezvous, ns string) {
	const refresh = DefaultTTL - 30

	for {
		select {
		case <-time.After(refresh * time.Second):
		case <-ctx.Done():
			return
		}

		err := rz.Register(ctx, ns, DefaultTTL)
		if err != nil {
			log.Errorf("Error registering [%s]: %s", ns, err.Error())
		}
	}
}

func (cli *client) Unregister(ctx context.Context, ns string) error {
	s, err := cli.host.NewStream(ctx, cli.rp, RendezvousProto)
	if err != nil {
		return err
	}
	defer s.Close()

	w := ggio.NewDelimitedWriter(s)
	req := newUnregisterMessage(ns, cli.host.ID())
	return w.WriteMsg(req)
}

func (cli *client) Discover(ctx context.Context, ns string, limit int, cookie []byte) ([]Registration, []byte, error) {
	s, err := cli.host.NewStream(ctx, cli.rp, RendezvousProto)
	if err != nil {
		return nil, nil, err
	}
	defer s.Close()

	r := ggio.NewDelimitedReader(s, 1<<20)
	w := ggio.NewDelimitedWriter(s)

	return discoverQuery(ns, limit, cookie, r, w)
}

func discoverQuery(ns string, limit int, cookie []byte, r ggio.Reader, w ggio.Writer) ([]Registration, []byte, error) {

	req := newDiscoverMessage(ns, limit, cookie)
	err := w.WriteMsg(req)
	if err != nil {
		return nil, nil, err
	}

	var res pb.Message
	err = r.ReadMsg(&res)
	if err != nil {
		return nil, nil, err
	}

	if res.GetType() != pb.Message_DISCOVER_RESPONSE {
		return nil, nil, fmt.Errorf("Unexpected response: %s", res.GetType().String())
	}

	status := res.GetDiscoverResponse().GetStatus()
	if status != pb.Message_OK {
		return nil, nil, RendezvousError{Status: status, Text: res.GetDiscoverResponse().GetStatusText()}
	}

	regs := res.GetDiscoverResponse().GetRegistrations()
	result := make([]Registration, 0, len(regs))
	for _, reg := range regs {
		pi, err := pbToPeerInfo(reg.GetPeer())
		if err != nil {
			log.Errorf("Invalid peer info: %s", err.Error())
			continue
		}
		result = append(result, Registration{Peer: pi, Ns: reg.GetNs(), Ttl: int(reg.GetTtl())})
	}

	return result, res.GetDiscoverResponse().GetCookie(), nil
}

func (cli *client) DiscoverAsync(ctx context.Context, ns string) (<-chan Registration, error) {
	s, err := cli.host.NewStream(ctx, cli.rp, RendezvousProto)
	if err != nil {
		return nil, err
	}

	ch := make(chan Registration)
	go discoverAsync(ctx, ns, s, ch)
	return ch, nil
}

func discoverAsync(ctx context.Context, ns string, s inet.Stream, ch chan Registration) {
	defer s.Close()
	defer close(ch)

	r := ggio.NewDelimitedReader(s, 1<<20)
	w := ggio.NewDelimitedWriter(s)

	const batch = 100

	var (
		cookie []byte
		regs   []Registration
		err    error
	)

	for {
		regs, cookie, err = discoverQuery(ns, batch, cookie, r, w)
		if err != nil {
			log.Errorf("Error in discovery [%s]: %s", ns, err.Error())
			return
		}

		for _, reg := range regs {
			select {
			case ch <- reg:
			case <-ctx.Done():
				return
			}
		}

		if len(regs) < batch {
			select {
			case <-time.After(2 * time.Minute):
			case <-ctx.Done():
				return
			}
		}
	}
}

func DiscoverPeers(ctx context.Context, rz Rendezvous, ns string, limit int, cookie []byte) ([]pstore.PeerInfo, []byte, error) {
	regs, cookie, err := rz.Discover(ctx, ns, limit, cookie)
	if err != nil {
		return nil, nil, err
	}

	pinfos := make([]pstore.PeerInfo, len(regs))
	for i, reg := range regs {
		pinfos[i] = reg.Peer
	}

	return pinfos, cookie, nil
}

func DiscoverPeersAsync(ctx context.Context, rz Rendezvous, ns string) (<-chan pstore.PeerInfo, error) {
	rch, err := rz.DiscoverAsync(ctx, ns)
	if err != nil {
		return nil, err
	}

	ch := make(chan pstore.PeerInfo)
	go discoverPeersAsync(ctx, rch, ch)
	return ch, nil
}

func discoverPeersAsync(ctx context.Context, rch <-chan Registration, ch chan pstore.PeerInfo) {
	defer close(ch)
	for {
		select {
		case reg, ok := <-rch:
			if !ok {
				return
			}

			select {
			case ch <- reg.Peer:
			case <-ctx.Done():
				return
			}
		case <-ctx.Done():
			return
		}
	}
}
