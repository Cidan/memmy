package bboltstore

// codec_export_test.go exposes the package-internal gob decoders to the
// external _test package via a thin alias. Used by counters_test.go to
// validate counter values against a fresh bucket walk without
// duplicating the codec.

import "github.com/Cidan/memmy/internal/types"

// DecodeNodeForTest is exposed only to *_test.go files in this directory.
func DecodeNodeForTest(b []byte, out *types.Node) error { return decodeNode(b, out) }

// DecodeEdgeForTest is exposed only to *_test.go files in this directory.
func DecodeEdgeForTest(b []byte, out *types.MemoryEdge) error { return decodeEdge(b, out) }
