package dabackup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func encodedArchive(t *testing.T, format string, headers []tar.Header, bodies []string, trailer []byte) string {
	t.Helper()
	var tarData bytes.Buffer
	tw := tar.NewWriter(&tarData)
	for i, header := range headers {
		header.Size = int64(len(bodies[i]))
		if err := tw.WriteHeader(&header); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(bodies[i])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	tarData.Write(trailer)
	var encoded bytes.Buffer
	var compressor io.WriteCloser
	switch format {
	case ".gz":
		compressor = gzip.NewWriter(&encoded)
	case ".zst":
		var err error
		compressor, err = zstd.NewWriter(&encoded, zstd.WithEncoderConcurrency(1))
		if err != nil {
			t.Fatal(err)
		}
	default:
		compressor = nopWriteCloser{&encoded}
	}
	if _, err := compressor.Write(tarData.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := compressor.Close(); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), "user.admin.customer1.tar"+format)
	if err := os.WriteFile(filename, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return filename
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func TestNativeArchiveFormatsAndMalformedIdentity(t *testing.T) {
	for _, format := range []string{"", ".gz", ".zst"} {
		for _, scenario := range []struct {
			name    string
			headers []tar.Header
			bodies  []string
			trailer []byte
			valid   bool
		}{
			{"valid", []tar.Header{{Name: "backup/user.conf", Typeflag: tar.TypeReg}}, []string{"username=customer1\n"}, nil, true},
			{"duplicate record", []tar.Header{{Name: "backup/user.conf", Typeflag: tar.TypeReg}, {Name: "./backup/user.conf", Typeflag: tar.TypeReg}}, []string{"username=customer1\n", "username=customer1\n"}, nil, false},
			{"duplicate key", []tar.Header{{Name: "backup/user.conf", Typeflag: tar.TypeReg}}, []string{"username=customer1\nusername=victim\n"}, nil, false},
			{"absolute identity", []tar.Header{{Name: "/backup/user.conf", Typeflag: tar.TypeReg}}, []string{"username=customer1\n"}, nil, false},
			{"traversal concealed by cleaning", []tar.Header{{Name: "backup/../backup/user.conf", Typeflag: tar.TypeReg}}, []string{"username=customer1\n"}, nil, false},
			{"non-padding trailer", []tar.Header{{Name: "backup/user.conf", Typeflag: tar.TypeReg}}, []string{"username=customer1\n"}, []byte("hidden second archive"), false},
		} {
			t.Run(format+"/"+scenario.name, func(t *testing.T) {
				archive := encodedArchive(t, format, scenario.headers, scenario.bodies, scenario.trailer)
				err := (Layout{}).ValidateArchive(t.Context(), archive, "customer1")
				if (err == nil) != scenario.valid {
					t.Fatalf("valid=%v got %v", scenario.valid, err)
				}
			})
		}
	}
}

func TestCompressedChecksumIsVerifiedAfterTarEOF(t *testing.T) {
	for _, format := range []string{".gz", ".zst"} {
		t.Run(format, func(t *testing.T) {
			archive := encodedArchive(t, format, []tar.Header{{Name: "backup/user.conf", Typeflag: tar.TypeReg}}, []string{"username=customer1\n"}, nil)
			data, err := os.ReadFile(archive)
			if err != nil {
				t.Fatal(err)
			}
			data[len(data)-1] ^= 0xff
			if err := os.WriteFile(archive, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := (Layout{}).ValidateArchive(t.Context(), archive, "customer1"); err == nil {
				t.Fatal("corrupt compressed trailer accepted")
			}
		})
	}
}

func TestArchiveValidationHonorsCancellation(t *testing.T) {
	archive := encodedArchive(t, ".zst", []tar.Header{{Name: "backup/user.conf", Typeflag: tar.TypeReg}}, []string{"username=customer1\n"}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := (Layout{}).ValidateArchive(ctx, archive, "customer1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestArchiveFilenameRequiresSupportedExtension(t *testing.T) {
	for _, name := range []string{"customer1", "user.admin.customer1.exe", "customer1.tar.zst.gz", "user.admin.customer1..tar", "user.admin.customer1.evil&select1=victim.tar"} {
		if nameMatchesArchive(name, "customer1") {
			t.Fatalf("invalid archive name accepted: %s", name)
		}
	}
}
