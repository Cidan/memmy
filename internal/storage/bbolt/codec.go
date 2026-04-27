package bboltstore

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"math"

	"github.com/Cidan/memmy/internal/types"
)

// We use gob for structured records and raw little-endian float32 bytes
// for vectors. Gob is stdlib-only and tolerant to backwards-compatible
// field additions; vectors are frozen-shape so a hand-rolled binary form
// is faster and tighter than gob.

func encodeNode(n *types.Node) ([]byte, error)         { return gobEncode(n) }
func encodeMessage(m *types.Message) ([]byte, error)   { return gobEncode(m) }
func encodeEdge(e *types.MemoryEdge) ([]byte, error)   { return gobEncode(e) }
func encodeHNSWRecord(r *types.HNSWRecord) ([]byte, error) {
	return gobEncode(r)
}
func encodeHNSWMeta(m *types.HNSWMeta) ([]byte, error) { return gobEncode(m) }
func encodeTenantInfo(t *types.TenantInfo) ([]byte, error) {
	return gobEncode(t)
}

func decodeNode(b []byte, out *types.Node) error             { return gobDecode(b, out) }
func decodeMessage(b []byte, out *types.Message) error       { return gobDecode(b, out) }
func decodeEdge(b []byte, out *types.MemoryEdge) error       { return gobDecode(b, out) }
func decodeHNSWRecord(b []byte, out *types.HNSWRecord) error { return gobDecode(b, out) }
func decodeHNSWMeta(b []byte, out *types.HNSWMeta) error     { return gobDecode(b, out) }
func decodeTenantInfo(b []byte, out *types.TenantInfo) error { return gobDecode(b, out) }

func gobEncode(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(v); err != nil {
		return nil, fmt.Errorf("gob encode: %w", err)
	}
	return buf.Bytes(), nil
}

func gobDecode(b []byte, out any) error {
	if err := gob.NewDecoder(bytes.NewReader(b)).Decode(out); err != nil {
		return fmt.Errorf("gob decode: %w", err)
	}
	return nil
}

// encodeVector serializes a []float32 as raw little-endian bytes
// (4 bytes per component, no header). Length is implied by the
// configured Dim.
func encodeVector(v []float32) []byte {
	out := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(f))
	}
	return out
}

// decodeVector parses a raw little-endian []float32 of dim components
// from b. Returns an error if b's length isn't dim*4.
func decodeVector(b []byte, dim int) ([]float32, error) {
	if len(b) != dim*4 {
		return nil, fmt.Errorf("vector length mismatch: have %d bytes, want %d", len(b), dim*4)
	}
	out := make([]float32, dim)
	decodeVectorInto(b, out)
	return out, nil
}

// decodeVectorInto fills out from b (must be pre-sized to dim).
func decodeVectorInto(b []byte, out []float32) {
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
}

// uint32LE / uint32FromLE are tiny helpers for fixed-width integers.
func putUint32LE(buf []byte, v uint32) { binary.LittleEndian.PutUint32(buf, v) }
func uint32FromLE(buf []byte) uint32   { return binary.LittleEndian.Uint32(buf) }
