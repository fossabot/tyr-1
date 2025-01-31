package core

import (
	"os"
	"path/filepath"
	"time"

	"github.com/docker/go-units"
	"github.com/dustin/go-humanize"

	"tyr/internal/pkg/filepool"
)

const defaultBlockSize = units.KiB * 16

func (d *Download) Start() {
	d.m.Lock()
	if d.done.Load() {
		d.state = Uploading
	} else {
		d.state = Downloading
	}
	d.m.Unlock()
	d.cond.Broadcast()
}

func (d *Download) Stop() {
	d.m.Lock()
	d.state = Stopped
	d.m.Unlock()
	d.cond.Broadcast()
}

func (d *Download) Check() {
	d.m.Lock()
	d.state = Checking
	d.bm.Clear()
	d.m.Unlock()
	d.cond.Broadcast()
}

// Init check existing files
func (d *Download) Init() {
	d.log.Debug().Msg("initializing download")

	d.m.Lock()
	d.state = Checking
	d.m.Unlock()

	err := d.initCheck()
	if err != nil {
		d.setError(err)
		d.log.Err(err).Msg("failed to initCheck torrent data")
	}

	d.ioDown.Reset()

	d.log.Debug().Msgf("done size %s", humanize.IBytes(uint64(d.bm.Count())*uint64(d.info.PieceLength)))

	d.m.Lock()
	if d.bm.Count() == d.info.NumPieces {
		d.state = Uploading
	} else {
		d.state = Downloading
	}
	d.m.Unlock()

	go d.startBackground()
}

func (d *Download) startBackground() {
	d.log.Trace().Msg("start goroutine")

	go d.backgroundReqHandle()
	go d.backgroundResHandler()

	go func() {
		for {
			select {
			case <-d.ctx.Done():
				return
			default:
			}

			d.m.Lock()

		LOOP:
			for {
				switch d.state {
				case Uploading, Downloading:
					break LOOP
				case Stopped, Moving, Checking, Error:
					d.cond.Wait()
				}
			}

			d.m.Unlock()

			d.connectToPeers()

			time.Sleep(time.Second)
		}
	}()

	for {
		select {
		case <-d.ctx.Done():
			return
		default:
		}

		d.m.Lock()
		if d.state == Stopped {
			d.log.Trace().Msg("paused, waiting")
			d.cond.Wait()
		}
		d.m.Unlock()

		d.TryAnnounce()

		time.Sleep(time.Second * 5)
	}
}

type Priority struct {
	Index  uint32
	Weight uint32
}

type PriorityQueue []Priority

func (p *PriorityQueue) Len() int {
	return len(*p)
}

func (p *PriorityQueue) Less(i, j int) bool {
	return (*p)[i].Weight > (*p)[j].Weight
}

func (p *PriorityQueue) Swap(i, j int) {
	(*p)[i], (*p)[j] = (*p)[j], (*p)[i]
}

func (p *PriorityQueue) Push(item Priority) {
	*p = append(*p, item)
}

func (p *PriorityQueue) Pop() Priority {
	old := *p
	n := len(old)
	x := old[n-1]
	*p = old[:n-1]
	return x
}

func (d *Download) backgroundResHandler() {
	for {
		select {
		case <-d.ctx.Done():
			return
		case res := <-d.ResChan:
			d.handleRes(res)
		}
	}
}

func (d *Download) openFile(fileIndex int) (*filepool.File, error) {
	p := filepath.Join(d.basePath, d.info.Files[fileIndex].Path)
	err := os.MkdirAll(filepath.Dir(p), os.ModePerm)
	if err != nil {
		return nil, err
	}

	return filepool.Open(p, os.O_RDWR|os.O_CREATE, os.ModePerm, time.Hour)
}

func (d *Download) readPiece(index uint32) ([]byte, error) {
	pieces := d.pieceInfo[index]
	var buf = make([]byte, d.pieceLength(index))

	var offset int64 = 0
	for _, chunk := range pieces.fileChunks {
		f, err := d.openFile(chunk.fileIndex)
		if err != nil {
			return nil, err
		}

		_, err = f.File.ReadAt(buf[offset:offset+chunk.length], chunk.offsetOfFile)
		if err != nil {
			f.Release()
			return nil, err
		}

		offset += chunk.length
		f.Release()
	}

	return buf, nil
}
