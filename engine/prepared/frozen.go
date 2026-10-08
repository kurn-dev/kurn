package prepared

import (
	"encoding/binary"
	"math/bits"
)

// Roaring's frozen format (roaring v1.9.4 serialization_littleendian.go,
// after CRoaring), from a 32-byte aligned start:
//
//	bitset data  uint64[1024] per bitset container
//	run data     (start u16, length u16) per run, all run containers
//	array data   uint16 per element, all array containers
//	keys         uint16[n]
//	counts       uint16[n]   bitset/array: cardinality-1; run: number of runs
//	typecodes    uint8[n]    1 bitset, 2 array, 3 run
//	header       uint32      cookie 13766 in bits 0-14, n in bits 15-31
//
// frozenView checks the cookie, n <= 65536, typecodes and the total length,
// then reinterprets the regions in place. It does not check that keys
// ascend, that array elements ascend, that a bitset's popcount matches its
// count, or that runs are well formed — roaring's lookups, iterators and
// Maximum assume all of those, and an empty run container makes Maximum
// index out of range. checkFrozen checks every one of them first.
const (
	frozenCookie       = 13766
	frozenMaxConts     = 1 << 16
	frozenBitsetBytes  = 1 << 13 // 1024 uint64 words
	frozenArrayMaxCard = 4096
)

// frozenInfo summarizes a structurally valid frozen bitmap.
type frozenInfo struct {
	containers  int
	cardinality uint64
	max         uint32
}

// frozenLayout is the region map a valid frozen trailer implies.
type frozenLayout struct {
	n                         int // containers
	keysAt, countsAt, typesAt int // trailer array offsets
	nBitset, nRunEl, nArrayEl int64
}

// checkFrozenTrailer validates b's trailer: cookie and byte order, a
// container count in [1, 65536], known typecodes, and a data region of
// exactly the length the typecodes and counts imply. It is O(containers)
// and never reads outside b.
func checkFrozenTrailer(b []byte) (frozenLayout, error) {
	var l frozenLayout
	if len(b) < 4 {
		return l, malformed("frozen bitmap: %d bytes, too short for a header", len(b))
	}
	hdrAt := len(b) - 4
	if be := binary.BigEndian.Uint32(b[hdrAt:]); be&0x7fff == frozenCookie {
		return l, malformed("frozen bitmap: big-endian header")
	}
	header := binary.LittleEndian.Uint32(b[hdrAt:])
	if header&0x7fff != frozenCookie {
		return l, malformed("frozen bitmap: bad cookie %#x", header&0x7fff)
	}
	n := int(header >> 15)
	if n < 1 || n > frozenMaxConts {
		return l, malformed("frozen bitmap: container count %d out of range [1, %d]", n, frozenMaxConts)
	}
	if hdrAt < 5*n {
		return l, malformed("frozen bitmap: %d containers need %d trailer bytes, have %d", n, 5*n, hdrAt)
	}
	l.n = n
	l.typesAt = hdrAt - n
	l.countsAt = l.typesAt - 2*n
	l.keysAt = l.countsAt - 2*n
	// All terms are bounded (n <= 2^16, counts < 2^16), so the arithmetic
	// cannot overflow.
	for i, t := range b[l.typesAt:hdrAt] {
		c := int64(binary.LittleEndian.Uint16(b[l.countsAt+2*i:]))
		switch t {
		case 1:
			l.nBitset++
		case 2:
			l.nArrayEl += c + 1
		case 3:
			l.nRunEl += c
		default:
			return l, malformed("frozen bitmap: container %d has unknown typecode %d", i, t)
		}
	}
	implied := l.nBitset*frozenBitsetBytes + 4*l.nRunEl + 2*l.nArrayEl
	if implied != int64(l.keysAt) {
		return l, malformed("frozen bitmap: typecodes and counts imply %d data bytes, have %d", implied, l.keysAt)
	}
	return l, nil
}

// checkFrozen structurally validates b as one frozen bitmap holding at
// least one value: the trailer (checkFrozenTrailer), then every container's
// content. It never reads outside b and never panics.
func checkFrozen(b []byte) (frozenInfo, error) {
	var fi frozenInfo
	l, err := checkFrozenTrailer(b)
	if err != nil {
		return fi, err
	}
	n, keysAt, countsAt, nBitset, nRunEl := l.n, l.keysAt, l.countsAt, l.nBitset, l.nRunEl
	types := b[l.typesAt : l.typesAt+n]
	count := func(i int) int { return int(binary.LittleEndian.Uint16(b[countsAt+2*i:])) }
	key := func(i int) uint16 { return binary.LittleEndian.Uint16(b[keysAt+2*i:]) }

	// Pass 2: every container's content must be what roaring assumes.
	bitsetAt := 0
	runAt := int(nBitset * frozenBitsetBytes)
	arrayAt := runAt + int(4*nRunEl)
	var prevKey uint16
	for i, t := range types {
		k := key(i)
		if i > 0 && k <= prevKey {
			return fi, malformed("frozen bitmap: container keys not strictly increasing at %d", i)
		}
		prevKey = k
		c := count(i)
		var card uint64
		var hi uint16
		switch t {
		case 1:
			words := b[bitsetAt : bitsetAt+frozenBitsetBytes]
			last := -1
			for w := 0; w < 1024; w++ {
				v := binary.LittleEndian.Uint64(words[8*w:])
				if v != 0 {
					card += uint64(bits.OnesCount64(v))
					last = 64*w + 63 - bits.LeadingZeros64(v)
				}
			}
			if card != uint64(c)+1 {
				return fi, malformed("frozen bitmap: bitset container %d holds %d values, count says %d", i, card, c+1)
			}
			hi = uint16(last)
			bitsetAt += frozenBitsetBytes
		case 2:
			sz := c + 1
			if sz > frozenArrayMaxCard {
				return fi, malformed("frozen bitmap: array container %d holds %d values, over %d", i, sz, frozenArrayMaxCard)
			}
			var prev uint16
			for j := 0; j < sz; j++ {
				v := binary.LittleEndian.Uint16(b[arrayAt+2*j:])
				if j > 0 && v <= prev {
					return fi, malformed("frozen bitmap: array container %d not strictly increasing", i)
				}
				prev = v
			}
			card = uint64(sz)
			hi = prev
			arrayAt += 2 * sz
		case 3:
			if c < 1 {
				return fi, malformed("frozen bitmap: run container %d is empty", i)
			}
			prevLast := -1
			for j := 0; j < c; j++ {
				start := int(binary.LittleEndian.Uint16(b[runAt+4*j:]))
				length := int(binary.LittleEndian.Uint16(b[runAt+4*j+2:]))
				last := start + length
				if last > 0xffff {
					return fi, malformed("frozen bitmap: run container %d: run %d overflows the container", i, j)
				}
				if start <= prevLast {
					return fi, malformed("frozen bitmap: run container %d: runs not ascending and disjoint", i)
				}
				prevLast = last
				card += uint64(length) + 1
			}
			hi = uint16(prevLast)
			runAt += 4 * c
		}
		fi.cardinality += card
		fi.max = uint32(k)<<16 | uint32(hi)
	}
	fi.containers = n
	return fi, nil
}
