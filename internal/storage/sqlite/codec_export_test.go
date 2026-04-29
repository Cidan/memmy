package sqlitestore

// codec_export_test.go exposes package-internal gob decoders to the
// external _test package via thin aliases. Used by counters_test.go
// to validate counter values against a fresh table walk without
// duplicating the codec.

import "github.com/Cidan/memmy/internal/types"

// DecodeNodeForTest is exposed only to *_test.go files in this package.
func DecodeNodeForTest(b []byte, out *types.Node) error { return decodeNode(b, out) }

// DecodeEdgeForTest is exposed only to *_test.go files in this package.
func DecodeEdgeForTest(b []byte, out *types.MemoryEdge) error { return decodeEdge(b, out) }
