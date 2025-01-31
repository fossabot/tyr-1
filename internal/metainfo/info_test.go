package metainfo

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/anacrolix/torrent/bencode"
)

func TestMarshalInfo(t *testing.T) {
	var info Info
	b, err := bencode.Marshal(info)
	require.NoError(t, err)
	require.EqualValues(t, "d4:name0:12:piece lengthi0e6:pieces0:e", string(b))
}
