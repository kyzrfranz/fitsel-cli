package garmin

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// makeFit builds a valid bare FIT file: a 12-byte header (header size 12, an
// arbitrary protocol/profile version, the data length, and the ".FIT"
// signature) followed by the data and a two-byte record CRC. The CRC is not
// checked by the client; only the header layout and the total length matter.
func makeFit(data []byte) []byte {
	buf := make([]byte, 12+len(data)+2)
	buf[0] = 12 // header size
	binary.LittleEndian.PutUint32(buf[4:8], uint32(len(data)))
	copy(buf[8:12], ".FIT")
	copy(buf[12:], data)
	return buf
}

// makeFit14 builds a valid bare FIT with a 14-byte header.
func makeFit14(data []byte) []byte {
	buf := make([]byte, 14+len(data)+2)
	buf[0] = 14 // 14-byte header
	binary.LittleEndian.PutUint32(buf[4:8], uint32(len(data)))
	copy(buf[8:12], ".FIT")
	copy(buf[14:], data) // data starts after the 14-byte header
	return buf
}

// zipFit wraps data in a single-file ZIP holding "<name>.fit", as Garmin does.
func zipFit(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create(name)
	if err != nil {
		t.Fatalf("create zip entry: %v", err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatalf("write zip entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return buf.Bytes()
}

func TestUnzipFitValidBareFit(t *testing.T) {
	fit := makeFit([]byte("real-fit-payload"))
	got, err := unzipFit(fit)
	if err != nil {
		t.Fatalf("unzipFit: %v", err)
	}
	if !bytes.Equal(got, fit) {
		t.Fatalf("bare FIT returned modified bytes")
	}
}

func TestUnzipFitValidZip(t *testing.T) {
	fit := makeFit([]byte("zip-fit-payload"))
	got, err := unzipFit(zipFit(t, "activity.fit", fit))
	if err != nil {
		t.Fatalf("unzipFit: %v", err)
	}
	if !bytes.Equal(got, fit) {
		t.Fatalf("zip FIT returned wrong bytes")
	}
}

func TestUnzipFitHTMLBodyRejected(t *testing.T) {
	body := []byte("<html><body>500 Internal Server Error</body></html>")
	_, err := unzipFit(body)
	if err == nil {
		t.Fatal("expected an error for an HTML body, got nil")
	}
	if !errors.Is(err, errInvalidFit) {
		t.Fatalf("expected an invalid-FIT error, got %T: %v", err, err)
	}
}

func TestUnzipFitEmptyBodyRejected(t *testing.T) {
	if _, err := unzipFit(nil); !errors.Is(err, errInvalidFit) {
		t.Fatalf("nil body: expected errInvalidFit, got %v", err)
	}
	if _, err := unzipFit([]byte{}); !errors.Is(err, errInvalidFit) {
		t.Fatalf("empty body: expected errInvalidFit, got %v", err)
	}
}

func TestUnzipFitZipWithoutFitRejected(t *testing.T) {
	body := zipFit(t, "activity.txt", []byte("not a fit"))
	_, err := unzipFit(body)
	if err == nil {
		t.Fatal("expected an error for a ZIP without a .fit, got nil")
	}
}

func TestUnzipFitTruncatedBodyRejected(t *testing.T) {
	fit := makeFit([]byte("this-payload-is-longer-than-the-claimed-data-size"))
	// The header claims a data size of 12 bytes, but the payload is longer, so
	// the total length no longer equals header size + data size + 2.
	fit[0] = 12
	binary.LittleEndian.PutUint32(fit[4:8], 12)
	if validFitHeader(fit) {
		t.Fatal("a mismatched-length file should not validate")
	}
	if _, err := unzipFit(fit); !errors.Is(err, errInvalidFit) {
		t.Fatalf("truncated/mismatched body: expected errInvalidFit, got %v", err)
	}
}

func TestUnzipFitWrongHeaderSizeRejected(t *testing.T) {
	fit := makeFit([]byte("payload"))
	fit[0] = 20 // header size neither 12 nor 14
	if _, err := unzipFit(fit); !errors.Is(err, errInvalidFit) {
		t.Fatalf("wrong header size: expected errInvalidFit, got %v", err)
	}
}

func TestUnzipFitBadSignatureRejected(t *testing.T) {
	fit := makeFit([]byte("payload"))
	fit[8] = 'X' // ".FIT" signature corrupted
	if _, err := unzipFit(fit); !errors.Is(err, errInvalidFit) {
		t.Fatalf("bad signature: expected errInvalidFit, got %v", err)
	}
}

// TestUnzipFit14ByteHeader proves a 14-byte-header FIT is accepted: the header
// size field is one of the two valid values and the length check still holds.
func TestUnzipFit14ByteHeader(t *testing.T) {
	fit := makeFit14([]byte("fourteen-byte-header-payload"))
	if !validFitHeader(fit) {
		t.Fatal("a well-formed 14-byte-header FIT should validate")
	}
	if _, err := unzipFit(fit); err != nil {
		t.Fatalf("unzipFit 14-byte header: %v", err)
	}
}

// TestValidFitHeaderDirect exercises the classifier across the documented cases.
func TestValidFitHeaderDirect(t *testing.T) {
	badHeaderSize := makeFit([]byte("x"))
	badHeaderSize[0] = 20
	badSignature := makeFit([]byte("x"))
	badSignature[8] = 'Z'
	tooShort := makeFit([]byte("hello"))[:11]

	cases := []struct {
		name string
		raw  []byte
		want bool
	}{
		{"valid", makeFit([]byte("hello")), true},
		{"valid-14", makeFit14([]byte("hello")), true},
		{"too-short", tooShort, false},
		{"html", []byte("<html>"), false},
		{"bad-header-size", badHeaderSize, false},
		{"bad-signature", badSignature, false},
	}
	for _, c := range cases {
		if got := validFitHeader(c.raw); got != c.want {
			t.Errorf("%s: validFitHeader=%v want %v", c.name, got, c.want)
		}
	}
}
