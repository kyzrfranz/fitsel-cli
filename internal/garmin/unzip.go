package garmin

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// errInvalidFit is returned when bytes that are not a ZIP do not form a valid
// bare FIT file (see validFitHeader). It is a per-activity error, so a caller
// that treats download failures per-activity skips the activity rather than
// uploading it — an HTML error page or a truncated Garmin body must never
// reach fitsel as an activity.
var errInvalidFit = errors.New("not a valid FIT file")

// validFitHeader reports whether raw begins with a valid FIT file header,
// without checking the CRC bytes (the FIT protocol's CRC is not recoverable
// here and the header alone is enough to reject an HTML page or a truncated
// body).
//
// A FIT file is, little-endian:
//
//	byte 0          header size (12 or 14)
//	byte 1          protocol version
//	bytes 2-3       profile version (uint16)
//	bytes 4-7       data size (uint32), the length of the data records,
//	                excluding the header and the two trailing CRC bytes
//	bytes 8-11      ASCII ".FIT"
//	bytes 12-13     header CRC (only for a 14-byte header; 0x0000 allowed)
//	...data...
//	bytes N-2..N-1  record CRC (uint16)
//
// so a well-formed file is exactly headerSize + data size + 2 bytes long.
func validFitHeader(raw []byte) bool {
	if len(raw) < 12 {
		return false
	}
	headerSize := raw[0]
	if headerSize != 12 && headerSize != 14 {
		return false
	}
	if string(raw[8:12]) != ".FIT" {
		return false
	}
	dataSize := binary.LittleEndian.Uint32(raw[4:8])
	return int(headerSize)+int(dataSize)+2 == len(raw)
}

// unzipFit takes the bytes Garmin returns for an activity (application/x-zip-
// compressed per the committed spec) and returns the single .fit inside.
//
// The spec lists only 200 for the download and does not say whether a ZIP can
// hold more than one file; the safe default is to take the first .fit.
func unzipFit(raw []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		// Not a ZIP: it should be a bare FIT. Validate it before returning,
		// so an HTML error page or a truncated body is an error rather than
		// being uploaded to fitsel as an activity.
		if !validFitHeader(raw) {
			return nil, fmt.Errorf("activity %w", errInvalidFit)
		}
		return raw, nil
	}
	for _, f := range zr.File {
		if strings.EqualFold(path.Ext(f.Name), ".fit") {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("no .fit file in activity archive")
}
