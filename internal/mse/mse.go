package mse

import (
	"io"
	"net"

	"github.com/anacrolix/torrent/mse"

	"tyr/internal/metainfo"
)

type rw struct {
	io.Reader
	io.Writer
}

func ForceCrypto(provided mse.CryptoMethod) mse.CryptoMethod {
	return mse.CryptoMethodRC4
}

func PreferCrypto(provided mse.CryptoMethod) mse.CryptoMethod {
	if provided&mse.CryptoMethodRC4 != 0 {
		return mse.CryptoMethodRC4
	}
	return mse.CryptoMethodPlaintext
}

func keyMatcher(keys []metainfo.Hash) func(f func([]byte) bool) {
	return func(f func([]byte) bool) {
		for _, ih := range keys {
			if !f(ih[:]) {
				break
			}
		}
	}
}

func NewAccept(conn net.Conn, keys []metainfo.Hash, selector mse.CryptoSelector) (net.Conn, error) {
	rw, _, err := mse.ReceiveHandshake(conn, keyMatcher(keys), selector)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	return wrappedConn{rw: rw, Conn: conn}, err
}

func NewConnection(infoHash []byte, conn net.Conn) (net.Conn, error) {
	ret, _, err := mse.InitiateHandshake(conn, infoHash, nil, mse.AllSupportedCrypto)
	if err != nil {
		return nil, err
	}

	return wrappedConn{rw: ret, Conn: conn}, nil
}

var _ io.ReadWriteCloser = wrappedConn{}

type wrappedConn struct {
	net.Conn
	rw io.ReadWriter
}

func (c wrappedConn) Read(b []byte) (n int, err error) {
	return c.rw.Read(b)
}

func (c wrappedConn) Write(b []byte) (n int, err error) {
	return c.rw.Write(b)
}
