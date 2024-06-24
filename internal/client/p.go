package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"sync"
	"time"

	"go.uber.org/atomic"

	"github.com/dchest/uniuri"
	"github.com/fatih/color"
	"github.com/puzpuzpuz/xsync/v3"
	"github.com/rs/zerolog"
	"github.com/samber/lo"

	"tyr/internal/pkg/bm"
	"tyr/internal/pkg/empty"
	"tyr/internal/pkg/flowrate"
	"tyr/internal/pkg/global"
	"tyr/internal/pkg/unsafe"
	"tyr/internal/proto"
)

type PeerID [20]byte

func (i PeerID) AsString() string {
	return unsafe.Str(i[:])
}

var emptyPeerID PeerID

func (i PeerID) Zero() bool {
	return i == emptyPeerID
}

var peerIDChars = []byte("0123456789abcdefghijklmnopqrstuvwxyz" +
	"ABCDEFGHIJKLMNOPQRSTUVWXYZ!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~")

func NewPeerID() (peerID PeerID) {
	copy(peerID[:], global.PeerIDPrefix)
	copy(peerID[8:], uniuri.NewLenCharsBytes(12, peerIDChars))
	return
}

func NewOutgoingPeer(conn net.Conn, d *Download, addr netip.AddrPort) *Peer {
	return newPeer(conn, d, addr, emptyPeerID, false, false)
}

func NewIncomingPeer(conn net.Conn, d *Download, addr netip.AddrPort, h proto.Handshake) *Peer {
	return newPeer(conn, d, addr, h.PeerID, true, h.FastExtension)
}

func newPeer(
	conn net.Conn,
	d *Download,
	addr netip.AddrPort,
	peerID PeerID,
	skipHandshake bool,
	fast bool,
) *Peer {
	ctx, cancel := context.WithCancel(context.Background())
	l := d.log.With().Stringer("addr", addr)
	var ua string
	if !peerID.Zero() {
		ua = parsePeerID(peerID)
		l = l.Str("peer_id", url.QueryEscape(peerID.AsString()))
	}

	p := &Peer{
		ctx:                  ctx,
		log:                  l.Logger(),
		supportFastExtension: fast,
		Conn:                 conn,
		d:                    d,
		cancel:               cancel,
		bitfieldSize:         (d.info.NumPieces + 7) / 8,
		Bitmap:               bm.New(d.info.NumPieces),
		ioOut:                flowrate.New(time.Second, time.Second),
		ioIn:                 flowrate.New(time.Second, time.Second),
		Address:              addr,
		//ResChan:   make(chan req.Response, 1),
		requests: xsync.NewMapOf[proto.ChunkRequest, empty.Empty](),
	}

	p.QueueLimit.Store(250)

	if ua != "" {
		p.UserAgent.Store(&ua)
	}

	go p.start(skipHandshake)
	return p
}

var ErrPeerSendInvalidData = errors.New("peer send invalid data")

type Peer struct {
	log                       zerolog.Logger
	ctx                       context.Context
	Conn                      net.Conn
	d                         *Download
	lastSend                  atomic.Pointer[time.Time]
	cancel                    context.CancelFunc
	Bitmap                    *bm.Bitmap
	requests                  *xsync.MapOf[proto.ChunkRequest, empty.Empty]
	ioOut                     *flowrate.Monitor
	ioIn                      *flowrate.Monitor
	UserAgent                 atomic.Pointer[string]
	Address                   netip.AddrPort
	peerChoked                atomic.Bool
	peerInterested            atomic.Bool
	imChoked                  atomic.Bool
	imInterested              atomic.Bool
	QueueLimit                atomic.Uint32
	closed                    atomic.Bool
	m                         sync.Mutex
	wm                        sync.Mutex
	bitfieldSize              uint32
	supportFastExtension      bool
	supportExtensionHandshake bool
}

func (p *Peer) Response(res proto.ChunkResponse) {
	err := p.sendEvent(Event{
		Event: proto.Piece,
		Res:   res,
	})
	if err != nil {
		p.close()
	}
	return
}

func (p *Peer) Request(req proto.ChunkRequest) {
	if p.requests.Size() > int(p.QueueLimit.Load()) {
		p.log.Trace().Msg("too many pending requests")
		return
	}

	_, exist := p.requests.LoadOrStore(req, empty.Empty{})
	if exist {
		p.log.Trace().Msg("requests already sent")
		return
	}

	p.log.Trace().Any("req", req).Msg("send piece request")
	err := p.sendEvent(Event{
		Event: proto.Request,
		Req:   req,
	})
	if err != nil {
		p.close()
	}
	return
}

func (p *Peer) Have(index uint32) {
	if p.Bitmap.Get(index) {
		return
	}

	err := p.sendEvent(Event{
		Index: index,
		Event: proto.Have,
	})
	if err != nil {
		p.close()
	}
}

func (p *Peer) close() {
	p.log.Trace().Msg("close")
	if p.closed.CompareAndSwap(false, true) {
		p.cancel()
		p.d.conn.Delete(p.Address)
		p.d.c.sem.Release(1)
		p.d.c.connectionCount.Sub(1)
		_ = p.Conn.Close()
	}
}

func (p *Peer) start(skipHandshake bool) {
	p.log.Trace().Msg("start")
	defer p.close()

	if err := proto.SendHandshake(p.Conn, p.d.info.Hash, NewPeerID()); err != nil {
		p.log.Trace().Err(err).Msg("failed to send handshake to peer")
		return
	}

	if !skipHandshake {
		h, err := proto.ReadHandshake(p.Conn)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				p.log.Trace().Err(err).Msg("failed to read handshake from peer")
			}
			return
		}
		if h.InfoHash != p.d.info.Hash {
			p.log.Trace().Msgf("peer info hash mismatch %x", h.InfoHash)
			return
		}
		p.supportFastExtension = h.FastExtension
		p.log = p.log.With().Str("peer_id", url.QueryEscape(string(h.PeerID[:]))).Logger()
		p.log.Trace().Msg("connect to peer")
		ua := parsePeerID(h.PeerID)
		p.UserAgent.Store(&ua)
	}

	if p.supportFastExtension {
		p.log.Trace().Msg("allow supportFastExtension extension")
	}

	var err error
	if p.supportFastExtension && p.d.bm.Count() == 0 {
		err = p.sendEvent(Event{Event: proto.HaveNone})
	} else if p.supportFastExtension && p.d.bm.Count() == p.d.info.NumPieces {
		err = p.sendEvent(Event{Event: proto.HaveAll})
	} else {
		err = p.sendEvent(Event{Event: proto.Bitfield, Bitmap: p.d.bm})
	}

	if err != nil {
		p.log.Trace().Err(err).Msg("failed to send bitfield")
		return
	}

	go p.keepAlive()

	for {
		if p.ctx.Err() != nil {
			return
		}

		event, err := p.DecodeEvents()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				p.log.Trace().Err(err).Msg("failed to decode event")
			}
			return
		}

		if event.Ignored {
			continue
		}

		p.log.Trace().Msgf("receive %s event", color.BlueString(event.Event.String()))

		switch event.Event {
		case proto.Bitfield:
			p.Bitmap.OR(event.Bitmap)
			if p.Bitmap.WithAndNot(p.d.bm).Count() != 0 {
				if p.imInterested.CompareAndSwap(false, true) {
					err = p.sendEvent(Event{Event: proto.Interested})
					if err != nil {
						return
					}
				}
			} else {
				if p.imInterested.CompareAndSwap(true, false) {
					err = p.sendEvent(Event{Event: proto.NotInterested})
					if err != nil {
						return
					}
				}
			}
		case proto.Have:
			p.Bitmap.Set(event.Index)
		case proto.Interested:
			p.peerInterested.Store(true)
		case proto.NotInterested:
			p.peerInterested.Store(false)
		case proto.Choke:
			p.peerChoked.Store(true)
		case proto.Unchoke:
			p.peerChoked.Store(false)
		case proto.Piece:
			if !p.resIsValid(event.Res) {
				p.log.Trace().Msg("failed to validate response")
				// send response without requests
				return
			}

			p.ioIn.Update(len(event.Res.Data))
			p.d.ResChan <- event.Res
		case proto.Request:
			//p.reqChan <- event.Req

		case proto.Extended:
			if event.ExtHandshake.V.Set {
				p.UserAgent.Store(&event.ExtHandshake.V.Value)
			}
			if event.ExtHandshake.QueueLength.Set {
				p.QueueLimit.Store(event.ExtHandshake.QueueLength.Value)
			}

		// TODO
		case proto.Cancel:
		case proto.Port:
		case proto.Suggest:
		case proto.HaveAll:
			p.Bitmap.Fill()
		case proto.HaveNone:
			p.Bitmap.Clear()
		case proto.Reject:
		case proto.AllowedFast:
		// currently unsupported

		// currently ignored
		case proto.BitCometExtension:
		}

		go func() {
			switch event.Event {
			case proto.Have, proto.HaveAll, proto.Bitfield:
				if p.Bitmap.WithAndNot(p.d.bm).Count() != 0 {
					err = p.sendEvent(Event{Event: proto.Interested})
					if err != nil {
						return
					}
				}
			}
		}()
	}
}

func (p *Peer) sendEvent(e Event) error {
	p.wm.Lock()
	defer p.wm.Unlock()
	p.log.Trace().Msgf("send %s", color.BlueString(e.Event.String()))

	_ = p.Conn.SetWriteDeadline(time.Now().Add(time.Minute * 3))

	p.lastSend.Store(lo.ToPtr(time.Now()))

	if e.keepAlive {
		return proto.SendKeepAlive(p.Conn)
	}

	switch e.Event {
	case proto.Choke:
		return proto.SendChoke(p.Conn)
	case proto.Unchoke:
		return proto.SendUnchoke(p.Conn)
	case proto.Interested:
		return proto.SendInterested(p.Conn)
	case proto.NotInterested:
		return proto.SendNotInterested(p.Conn)
	case proto.Have:
		return proto.SendHave(p.Conn, e.Index)
	case proto.Bitfield:
		return proto.SendBitfield(p.Conn, e.Bitmap)
	case proto.Request:
		return proto.SendRequest(p.Conn, e.Req)
	case proto.Piece:
		p.ioOut.Update(len(e.Res.Data))
		return proto.SendPiece(p.Conn, e.Res)
	case proto.Cancel:
		return proto.SendCancel(p.Conn, e.Req)
	case proto.Port:
		return proto.SendPort(p.Conn, e.Port)
	case proto.Suggest:
		return proto.SendSuggest(p.Conn, e.Index)
	case proto.HaveAll, proto.HaveNone:
		return proto.SendNoPayload(p.Conn, e.Event)
	case proto.AllowedFast:
		return proto.SendIndexOnly(p.Conn, e.Event, e.Index)
	case proto.Reject:
		return proto.SendReject(p.Conn, e.Req)
	case proto.Extended, proto.BitCometExtension:
		panic("unexpected event")
	}

	return nil
}

func (p *Peer) resIsValid(res proto.ChunkResponse) bool {
	r := proto.ChunkRequest{
		PieceIndex: res.PieceIndex,
		Begin:      res.Begin,
		Length:     uint32(len(res.Data)),
	}

	if _, ok := p.requests.LoadAndDelete(r); !ok {
		return false
	}

	return true
}

func (p *Peer) decodePiece(size uint32) (Event, error) {
	payload, err := proto.ReadPiecePayload(p.Conn, size)
	if err != nil {
		return Event{}, err
	}

	return Event{Event: proto.Piece, Res: payload}, nil
}

func parsePeerID(id PeerID) string {
	if id[0] == '-' && id[7] == '-' {
		if id[1] == 'q' && id[2] == 'B' {
			if id[6] == '0' {
				return fmt.Sprintf("qBittorrent %d.%d.%d", id[3]-'0', id[4]-'0', id[5]-'0')
			}

			return fmt.Sprintf("qBittorrent %d.%d.%d.%d", id[3]-'0', id[4]-'0', id[5]-'0', id[6]-'0')
		}

		// TODO
		return fmt.Sprintf("%s", id[1:6])
	}

	return string(id[:6])
}
