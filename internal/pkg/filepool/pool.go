package filepool

import (
	"os"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog/log"
)

func init() {
	prometheus.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "tyr_file_pool_size",
	}, func() float64 {
		return float64(pool.Len())
	}))
}

func onEvict(key cacheKey, value *File) {
	log.Debug().Str("path", key.path).Msg("close file")
	_ = value.File.Close()
}

var pool = expirable.NewLRU[cacheKey, *File](128, onEvict, time.Minute*5)

type cacheKey struct {
	path string
	flag int
	perm os.FileMode
	ttl  time.Duration
}

// Open creates and returns a file item with given file path, flag and opening permission.
// It automatically creates an associated file pointer pool internally when it's called first time.
// It retrieves a file item from the file pointer pool after then.
func Open(path string, flag int, perm os.FileMode, ttl time.Duration) (file *File, err error) {
	key := cacheKey{path: path, flag: flag, perm: perm, ttl: ttl}
	item, ok := pool.Get(key)
	if ok {
		return item, nil
	}

	f, err := os.OpenFile(path, flag, perm)
	if err != nil {
		return nil, err
	}

	return &File{
		File: f,
		key:  key,
	}, nil
}

// File is an item in the pool.
type File struct {
	File *os.File
	key  cacheKey
}

func (f *File) Release() {
	pool.Add(f.key, f)
}
