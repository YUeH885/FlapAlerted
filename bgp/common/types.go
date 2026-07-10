package common

import "encoding/binary"

type AFI uint16

const (
	AFI4 AFI = 1
	AFI6 AFI = 2
)

type SAFI uint8

const (
	UNICAST   SAFI = 1
	MULTICAST SAFI = 2
)

type AsPath []uint32

func PathKey(p AsPath) string {
	if len(p) == 0 {
		return ""
	}
	buf := make([]byte, len(p)*4)
	for i, v := range p {
		binary.LittleEndian.PutUint32(buf[i*4:], v)
	}
	return string(buf)
}

type PathInfo struct {
	Path              AsPath
	AnnouncementCount uint64 `json:"ac"`
	WithdrawalCount   uint64 `json:"wc"`
}
