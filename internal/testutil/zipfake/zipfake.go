// Package zipfake writes syntactically valid zip archives whose central
// directory lies about the uncompressed sizes of its entries: payloads stay
// tiny while the declared sizes are forged, for testing zip-bomb guards.
// It is a package (not an unexported test helper) because the guards under
// test live in their own package: transfer (database bundles).
package zipfake

import (
	"encoding/binary"
	"hash/crc32"
	"math"
	"os"
	"testing"
)

// Entry is one stored (uncompressed) zip entry. Declared is the
// uncompressed size the central directory claims; with Zip64 the claim
// moves to a zip64 extra record so values beyond the uint32 range can be
// forged. Local file headers always carry the honest sizes: Go's zip
// reader takes sizes from the central directory, which is what the guards
// under test sum.
type Entry struct {
	Name     string
	Payload  []byte
	Declared uint64
	Zip64    bool
}

// Write creates the archive at path, failing the test on any error.
func Write(t *testing.T, path string, entries []Entry) {
	t.Helper()
	le := binary.LittleEndian
	var buf []byte
	type centralEntry struct {
		name     string
		crc      uint32
		comp     uint32
		declared uint64
		zip64    bool
		localOff uint32
	}
	var central []centralEntry
	for _, e := range entries {
		if !e.Zip64 && e.Declared > math.MaxUint32 {
			t.Fatalf("zipfake: entry %q declares %d bytes, over the plain uint32 field; set Zip64", e.Name, e.Declared)
		}
		crc := crc32.ChecksumIEEE(e.Payload)
		comp := uint32(len(e.Payload))
		central = append(central, centralEntry{e.Name, crc, comp, e.Declared, e.Zip64, uint32(len(buf))})
		version := uint16(20)
		if e.Zip64 {
			version = 45 // zip64
		}
		buf = le.AppendUint32(buf, 0x04034b50) // local file header
		buf = le.AppendUint16(buf, version)    // version needed
		buf = le.AppendUint16(buf, 0)          // flags
		buf = le.AppendUint16(buf, 0)          // method: stored
		buf = le.AppendUint16(buf, 0)          // mod time
		buf = le.AppendUint16(buf, 0)          // mod date
		buf = le.AppendUint32(buf, crc)
		buf = le.AppendUint32(buf, comp)
		buf = le.AppendUint32(buf, comp) // honest uncompressed size
		buf = le.AppendUint16(buf, uint16(len(e.Name)))
		buf = le.AppendUint16(buf, 0) // extra length
		buf = append(buf, e.Name...)
		buf = append(buf, e.Payload...)
	}
	cdOff := uint32(len(buf))
	for _, e := range central {
		version := uint16(20)
		comp32, decl32 := e.comp, uint32(e.declared)
		var extraLen uint16
		if e.zip64 {
			version = 45
			comp32, decl32 = math.MaxUint32, math.MaxUint32 // sizes move to the extra record
			extraLen = 20
		}
		buf = le.AppendUint32(buf, 0x02014b50) // central directory header
		buf = le.AppendUint16(buf, version)    // version made by
		buf = le.AppendUint16(buf, version)    // version needed
		buf = le.AppendUint16(buf, 0)          // flags
		buf = le.AppendUint16(buf, 0)          // method: stored
		buf = le.AppendUint16(buf, 0)          // mod time
		buf = le.AppendUint16(buf, 0)          // mod date
		buf = le.AppendUint32(buf, e.crc)
		buf = le.AppendUint32(buf, comp32)
		buf = le.AppendUint32(buf, decl32)
		buf = le.AppendUint16(buf, uint16(len(e.name)))
		buf = le.AppendUint16(buf, extraLen)
		buf = le.AppendUint16(buf, 0) // comment length
		buf = le.AppendUint16(buf, 0) // disk number
		buf = le.AppendUint16(buf, 0) // internal attrs
		buf = le.AppendUint32(buf, 0) // external attrs
		buf = le.AppendUint32(buf, e.localOff)
		buf = append(buf, e.name...)
		if e.zip64 {
			// zip64 extra record: header 0x0001, 16-byte body with both sizes.
			buf = le.AppendUint16(buf, 0x0001)
			buf = le.AppendUint16(buf, 16)
			buf = le.AppendUint64(buf, e.declared)     // uncompressed
			buf = le.AppendUint64(buf, uint64(e.comp)) // compressed
		}
	}
	cdSize := uint32(len(buf)) - cdOff
	buf = le.AppendUint32(buf, 0x06054b50) // end of central directory
	buf = le.AppendUint16(buf, 0)          // disk
	buf = le.AppendUint16(buf, 0)          // central dir disk
	buf = le.AppendUint16(buf, uint16(len(central)))
	buf = le.AppendUint16(buf, uint16(len(central)))
	buf = le.AppendUint32(buf, cdSize)
	buf = le.AppendUint32(buf, cdOff)
	buf = le.AppendUint16(buf, 0) // comment length
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		t.Fatal(err)
	}
}
