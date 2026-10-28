package garmin

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"path"
	"strings"
)

// unzipFit takes the bytes Garmin returns for an activity (application/x-zip-
// compressed per the committed spec) and returns the single .fit inside.
//
// The spec lists only 200 for the download and does not say whether a ZIP can
// hold more than one file; the safe default is to take the first .fit.
func unzipFit(raw []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		// Not a ZIP: assume it is already a bare FIT and return it as-is.
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
