package otlp

import (
	"encoding/binary"
	"fmt"
	"math"
)

// A minimal protobuf reader, for tests only.
//
// The encoder writes field numbers from opentelemetry-proto by hand, and a
// wrong one produces a payload that is still valid protobuf — a collector would
// silently drop the field, or attach the value to whatever else claims that
// number. Nothing in the encoder can catch that, and a golden-bytes test would
// only assert that the mistake is reproducible. So the tests decode what was
// written and assert on the structure, which is the only check that actually
// distinguishes "encoded" from "encoded correctly".

type pbField struct {
	num   int
	wire  int
	num64 uint64 // varint and fixed64 payloads
	data  []byte // length-delimited payload
}

func parsePB(b []byte) ([]pbField, error) {
	var out []pbField
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			return nil, fmt.Errorf("bad tag")
		}
		b = b[n:]
		f := pbField{num: int(tag >> 3), wire: int(tag & 7)}
		switch f.wire {
		case wireVarint:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				return nil, fmt.Errorf("bad varint for field %d", f.num)
			}
			f.num64, b = v, b[n:]
		case wireFixed64:
			if len(b) < 8 {
				return nil, fmt.Errorf("short fixed64 for field %d", f.num)
			}
			f.num64, b = binary.LittleEndian.Uint64(b), b[8:]
		case wireBytes:
			l, n := binary.Uvarint(b)
			if n <= 0 || uint64(len(b[n:])) < l {
				return nil, fmt.Errorf("short bytes for field %d", f.num)
			}
			f.data, b = b[n:n+int(l)], b[n+int(l):]
		default:
			return nil, fmt.Errorf("unsupported wire type %d on field %d", f.wire, f.num)
		}
		out = append(out, f)
	}
	return out, nil
}

// pbAll returns every occurrence of a field, which repeated fields need.
func pbAll(fs []pbField, num int) []pbField {
	var out []pbField
	for _, f := range fs {
		if f.num == num {
			out = append(out, f)
		}
	}
	return out
}

// pbOne returns the single occurrence of a field, or false when absent.
func pbOne(fs []pbField, num int) (pbField, bool) {
	all := pbAll(fs, num)
	if len(all) != 1 {
		return pbField{}, false
	}
	return all[0], true
}

// pbSub parses a nested message.
func pbSub(fs []pbField, num int) ([]pbField, error) {
	f, ok := pbOne(fs, num)
	if !ok {
		return nil, fmt.Errorf("field %d absent or repeated", num)
	}
	return parsePB(f.data)
}

func pbString(fs []pbField, num int) string {
	f, ok := pbOne(fs, num)
	if !ok {
		return ""
	}
	return string(f.data)
}

func pbDouble(fs []pbField, num int) (float64, bool) {
	f, ok := pbOne(fs, num)
	if !ok {
		return 0, false
	}
	return math.Float64frombits(f.num64), true
}

func unpackFixed64(b []byte) []uint64 {
	out := make([]uint64, 0, len(b)/8)
	for len(b) >= 8 {
		out = append(out, binary.LittleEndian.Uint64(b))
		b = b[8:]
	}
	return out
}

func unpackDouble(b []byte) []float64 {
	out := make([]float64, 0, len(b)/8)
	for _, v := range unpackFixed64(b) {
		out = append(out, math.Float64frombits(v))
	}
	return out
}
