package metainfo

import (
	"bufio"
	"crypto/sha1"
	"io"
	"os"

	"github.com/anacrolix/torrent/bencode"
)

type MetaInfo struct {
	InfoBytes    bencode.Bytes `bencode:"info,omitempty"`          // BEP 3
	Announce     string        `bencode:"announce,omitempty"`      // BEP 3
	AnnounceList AnnounceList  `bencode:"announce-list,omitempty"` // BEP 12
	Comment      string        `bencode:"comment,omitempty"`

	//CreatedBy    string        `bencode:"created by,omitempty"`
	//Encoding     string        `bencode:"encoding,omitempty"`
	//UrlList UrlList `bencode:"url-list,omitempty"` // BEP 19 WebSeeds

	// Where's this specified? Mentioned at
	// https://wiki.theory.org/index.php/BitTorrentSpecification: (optional) the creation time of
	// the torrent, in standard UNIX epoch format (integer, seconds since 1-Jan-1970 00:00:00 UTC)
	//CreationDate null.Null[bencode.Bytes] `bencode:"creation date,omitempty,ignore_unmarshal_type_error"`
}

// Load a MetaInfo from an io.Reader. Returns a non-nil error in case of failure.
func Load(r io.Reader) (*MetaInfo, error) {
	var mi MetaInfo
	d := bencode.NewDecoder(r)
	err := d.Decode(&mi)
	if err != nil {
		return nil, err
	}
	return &mi, nil
}

func LoadFromFile(filename string) (*MetaInfo, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var buf bufio.Reader
	buf.Reset(f)
	return Load(&buf)
}

func (mi MetaInfo) UnmarshalInfo() (info Info, err error) {
	err = bencode.Unmarshal(mi.InfoBytes, &info)
	return
}

func (mi *MetaInfo) HashInfoBytes() Hash {
	return sha1.Sum(mi.InfoBytes)
}

// Encode to bencoded form.
func (mi MetaInfo) Write(w io.Writer) error {
	return bencode.NewEncoder(w).Encode(mi)
}

func (mi *MetaInfo) UpvertedAnnounceList() AnnounceList {
	if mi.AnnounceList.OverridesAnnounce(mi.Announce) {
		return mi.AnnounceList
	}
	if mi.Announce != "" {
		return [][]string{{mi.Announce}}
	}
	return nil
}
