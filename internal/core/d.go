package core

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/types/infohash"
	"github.com/dustin/go-humanize"
	"github.com/puzpuzpuz/xsync/v3"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"go.uber.org/atomic"

	"tyr/internal/meta"
	"tyr/internal/pkg/bm"
	"tyr/internal/pkg/flowrate"
	"tyr/internal/pkg/global"
	"tyr/internal/pkg/heap"
	"tyr/internal/pkg/mempool"
	"tyr/internal/proto"
)

type State uint8

//go:generate stringer -type=State
const Stopped State = 0
const Downloading State = 1
const Uploading State = 2
const Checking State = 3
const Moving State = 4
const Error State = 5

// Download manage a download task
// ctx should be canceled when torrent is removed, not stopped.
type Download struct {
	log               zerolog.Logger
	ctx               context.Context
	err               error
	reqHistory        *xsync.MapOf[uint32, downloadReq]
	cancel            context.CancelFunc
	cond              *sync.Cond
	c                 *Client
	ioDown            *flowrate.Monitor
	ioUp              *flowrate.Monitor
	ResChan           chan proto.ChunkResponse
	conn              *xsync.MapOf[netip.AddrPort, *Peer]
	connectionHistory *xsync.MapOf[netip.AddrPort, connHistory]
	bm                *bm.Bitmap
	pieceData         map[uint32][]*proto.ChunkResponse
	peers             *heap.Heap[peerWithPriority]
	fileOpenMutex     *sync.Cond
	fileOpenCache     map[int]*fileOpenCache
	m                 sync.RWMutex
	basePath          string
	key               string
	downloadDir       string
	tags              []string
	pieceInfo         []pieceFileChunks
	trackers          []TrackerTier
	info              meta.Info
	AddAt             int64
	CompletedAt       atomic.Int64
	downloaded        atomic.Int64
	corrupted         atomic.Int64
	done              atomic.Bool
	uploaded          atomic.Int64
	completed         atomic.Int64
	checkProgress     atomic.Int64
	uploadAtStart     int64
	downloadAtStart   int64
	lazyInitialized   atomic.Bool
	seq               atomic.Bool
	announcePending   atomic.Bool
	pdMutex           sync.RWMutex
	connMutex         sync.RWMutex
	peersMutex        sync.Mutex
	peerID            PeerID
	state             State
	private           bool
}

type fileOpenCache struct {
	file     *os.File
	index    int
	borrowed bool
}

var ErrTorrentNotFound = errors.New("torrent not found")

func (c *Client) ScheduleMove(ih meta.Hash, targetBasePath string) error {
	c.m.RLock()
	d, ok := c.downloadMap[ih]
	c.m.RUnlock()

	if !ok {
		return ErrTorrentNotFound
	}

	err := d.Move(targetBasePath)

	return err
}

func (c *Client) NewDownload(m *metainfo.MetaInfo, info meta.Info, basePath string, tags []string) *Download {
	ctx, cancel := context.WithCancel(context.Background())

	d := &Download{
		ctx:      ctx,
		info:     info,
		cancel:   cancel,
		c:        c,
		log:      log.With().Stringer("info_hash", info.Hash).Logger(),
		state:    Checking,
		peerID:   NewPeerID(),
		tags:     tags,
		basePath: basePath,

		reqHistory: xsync.NewMapOf[uint32, downloadReq](),

		AddAt: time.Now().Unix(),

		ResChan: make(chan proto.ChunkResponse, 1),

		ioDown: flowrate.New(time.Second, time.Second),
		ioUp:   flowrate.New(time.Second, time.Second),

		conn:              xsync.NewMapOf[netip.AddrPort, *Peer](),
		connectionHistory: xsync.NewMapOf[netip.AddrPort, connHistory](),

		peers: heap.New[peerWithPriority](),

		// will use about 1mb per torrent, can be optimized later
		pieceInfo: buildPieceInfos(info),
		pieceData: make(map[uint32][]*proto.ChunkResponse, 20),

		private: info.Private,

		bm: bm.New(info.NumPieces),

		downloadDir: basePath,

		fileOpenMutex: sync.NewCond(&sync.Mutex{}),

		fileOpenCache: make(map[int]*fileOpenCache),
	}

	d.cond = sync.NewCond(&d.m)

	if global.Dev {
		d.seq.Store(true)
		d.peersMutex.Lock()
		d.peers.Push(peerWithPriority{
			addrPort: netip.MustParseAddrPort("192.168.1.3:50025"),
			priority: math.MaxUint32,
		})
		d.peersMutex.Unlock()
		//	piece := lo.Must(lo.Last(d.pieceChunks))
		//	assert.LessOrEqual(piece[len(piece)-1].Length, uint32(defaultBlockSize))
	} else {
		d.setAnnounceList(m)
	}

	d.log.Info().Msg("download created")

	//spew.Dump(d.pieceChunks[0])
	//spew.Dump(d.pieceChunks[len(d.pieceChunks)-1])

	return d
}

func (d *Download) Display() string {
	buf := mempool.Get()
	defer mempool.Put(buf)

	d.m.RLock()
	defer d.m.RUnlock()

	rate := d.ioDown.Status()

	var completed int64

	if !d.bm.Get(d.info.NumPieces - 1) {
		completed = int64(d.bm.Count()) * d.info.PieceLength
	} else {
		completed = int64(d.bm.Count()-1)*d.info.PieceLength + d.info.LastPieceSize
	}

	left := d.info.TotalLength - completed

	var eta time.Duration
	if rate.CurRate != 0 {
		eta = time.Second * time.Duration(left/rate.CurRate)
	}

	if d.state == Downloading {
		if d.info.NumPieces-d.bm.Count() < 10 {
			for i := uint32(0); i < d.info.NumPieces; i++ {
				if !d.bm.Get(i) {
					fmt.Println("missing", i)
				}
			}
		}
	}

	_, _ = fmt.Fprintf(buf, "%11s | %s | %6.1f%% | %8s | %8s | %10s ↓ | %5s | %d",
		d.state,
		d.info.Hash,
		float64(completed*1000/d.info.TotalLength)/10,
		humanize.IBytes(uint64(d.info.TotalLength)),
		humanize.IBytes(uint64(left)),
		rate.RateString(),
		eta.String(),
		d.conn.Size(),
	)

	if d.err != nil {
		_, _ = fmt.Fprintf(buf, "| %v", d.err)
	}

	for _, tier := range d.trackers {
		for _, t := range tier.trackers {
			t.RLock()
			fmt.Fprintf(buf, " ( %d %s )", t.peerCount, t.url)
			if t.err != nil {
				_, _ = fmt.Fprintf(buf, " | %s", t.err)
			}
			t.RUnlock()
		}
	}

	var s []peerDisplay

	d.conn.Range(func(key netip.AddrPort, p *Peer) bool {
		s = append(s, peerDisplay{
			Up:     humanize.IBytes(uint64(p.ioOut.Status().CurRate)),
			Down:   humanize.IBytes(uint64(p.ioIn.Status().CurRate)),
			Client: p.UserAgent.Load(),
			Addr:   key,
		})

		return true
	})

	sort.Slice(s, func(i, j int) bool {
		return s[i].Addr.Compare(s[j].Addr) < 1
	})

	for _, p := range s {
		if p.Client == nil {
			_, _ = fmt.Fprintf(buf, "\n ↓ %6s/s | ↑ %6s/s | %s", p.Down, p.Up, p.Addr)
		} else {
			_, _ = fmt.Fprintf(buf, "\n ↓ %6s/s | ↑ %6s/s | %s | %s", p.Down, p.Up, *p.Client, p.Addr)
		}
	}

	return buf.String()
}

type peerDisplay struct {
	Up     string
	Down   string
	Client *string
	Addr   netip.AddrPort
}

// if download encounter an error must stop downloading/uploading
func (d *Download) setError(err error) {
	d.m.Lock()
	d.err = err
	d.state = Error
	d.m.Unlock()
}

func canonicalName(info metainfo.Info, infoHash infohash.T) string {
	// yes, there are some torrent have this name
	name := info.Name
	if (info.NameUtf8) != "" {
		name = info.NameUtf8
	}

	if name == "" {
		return infoHash.HexString()
	}

	if len(info.Files) != 0 {
		return name
	}
	s := strings.Split(name, ".")
	if len(s) == 0 {
		return name
	}

	return strings.Join(s[:len(s)-1], ".")
}

type connHistory struct {
	lastTry   time.Time
	err       error
	timeout   bool
	connected bool
}
