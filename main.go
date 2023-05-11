// Copyright (C) 2023 Sneller, Inc.
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"time"

	"github.com/SnellerInc/sneller/ion/zion/iguana"
	"golang.org/x/sys/cpu"
)

//go:embed testdata/silesia.tar.gz
var silesia []byte

type compressor struct {
	name       string
	avail      func() bool
	compress   func([]byte) []byte
	decompress func(src []byte, deadline time.Time) int
}

const iguanaWindowSize = 256 * 1024

func iguanaCompress(src []byte, ans bool) []byte {
	var out []byte
	threshold := 0.0
	if ans {
		threshold = 1.0
	}
	var enc iguana.Encoder
	for len(src) > 0 {
		mem := src
		if len(mem) > iguanaWindowSize {
			mem = mem[:iguanaWindowSize]
		}
		src = src[len(mem):]
		lenpos := len(out)
		out = append(out, 0, 0, 0)
		var err error
		out, err = enc.Compress(mem, out, float32(threshold))
		if err != nil {
			panic(err)
		}
		winsize := len(out) - 3 - lenpos
		out[lenpos] = byte(winsize & 0xff)
		out[lenpos+1] = byte((winsize >> 8) & 0xff)
		out[lenpos+2] = byte((winsize >> 16) & 0xff)
	}
	return out
}

func iguanaDecompress(src []byte, deadline time.Time) int {
	var tmp []byte
	var err error
	var dec iguana.Decoder
	iters := 0
	for time.Now().Before(deadline) {
		src := src
		for len(src) >= 4 {
			winsize := int(src[0]) + (int(src[1]) << 8) + (int(src[2]) << 16)
			src = src[3:]
			if len(src) < winsize {
				panic("invalid frame")
			}
			mem := src[:winsize]
			src = src[winsize:]
			tmp, err = dec.DecompressTo(tmp[:0], mem)
			if err != nil {
				panic(err)
			}
		}
		iters++
	}
	return iters
}

// corpusReader is an io.Reader
// that is used as stdin for the external decompressors;
// we want to feed the compressed corpus data to the subprocess
// continuously so as to eliminate any exec overhead
type corpusReader struct {
	src      []byte
	deadline time.Time
	iters    int
}

func (c *corpusReader) Read(p []byte) (int, error) {
	// the os package will always end up using the WriterTo implementation
	panic("unexpected")
	return 0, fmt.Errorf("uh oh")
}

func (c *corpusReader) WriteTo(w io.Writer) (int64, error) {
	nn := int64(0)
	for time.Now().Before(c.deadline) {
		n, err := w.Write(c.src)
		c.iters++
		nn += int64(n)
		if err != nil {
			return nn, err
		}
	}
	return nn, nil
}

func decompressCmdline(args ...string) func([]byte, time.Time) int {
	return func(src []byte, deadline time.Time) int {
		cmd := exec.Command(args[0], args[1:]...)
		cr := &corpusReader{src: src, deadline: deadline}
		cmd.Stdin = cr
		cmd.Stdout = nil
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		if err != nil {
			fatalf("running %v: %s", args, err)
		}
		return cr.iters
	}
}

func compressCmdline(args ...string) func([]byte) []byte {
	return func(src []byte) []byte {
		var out bytes.Buffer
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Stdin = bytes.NewReader(src)
		cmd.Stdout = &out
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		if err != nil {
			fatalf("running %v: %s", args, err)
		}
		return out.Bytes()
	}
}

func lookPath(name string) func() bool {
	return func() bool {
		name, _ = exec.LookPath(name)
		return name != ""
	}
}

var zstdDecompress = decompressCmdline("zstd", "--single-thread", "-d", "-c")
var zstdAvail = lookPath("zstd")

var lz4Decompress = decompressCmdline("lz4", "-d", "-c")
var lz4Avail = lookPath("lz4")

var gzipDecompress = decompressCmdline("gzip", "-d", "-c")
var gzipAvail = lookPath("gzip")

func benchmark(c *compressor, data []byte) {
	if !c.avail() {
		logf("skipping %v (not available)\n", c.name)
		return
	}
	compressed := c.compress(data)
	start := time.Now()
	deadline := start.Add(3 * time.Second)
	iters := c.decompress(compressed, deadline)
	elapsed := time.Since(start)
	ratio := float64(len(compressed)) / float64(len(data))
	wrote := int64(len(data)) * int64(iters)

	multiplier := float64(1024*1024*1024) / float64(time.Second)
	gibps := float64(wrote) / (float64(elapsed) * multiplier)
	fmt.Printf("%s, %.3g, %.3g\n", c.name, ratio, gibps)
}

var compressors = []compressor{
	{
		name:  "iguana_avx512_ans",
		avail: func() bool { return cpu.X86.HasAVX512VBMI2 },
		compress: func(src []byte) []byte {
			return iguanaCompress(src, true)
		},
		decompress: iguanaDecompress,
	},
	{
		name:  "iguana_avx512_noans",
		avail: func() bool { return cpu.X86.HasAVX512VBMI2 },
		compress: func(src []byte) []byte {
			return iguanaCompress(src, false)
		},
		decompress: iguanaDecompress,
	},
	{
		name:       "zstd-9",
		avail:      zstdAvail,
		compress:   compressCmdline("zstd", "-c", "-9"),
		decompress: zstdDecompress,
	},
	{
		name:       "zstd-1",
		avail:      zstdAvail,
		compress:   compressCmdline("zstd", "-c", "-1"),
		decompress: zstdDecompress,
	},
	{
		name:       "zstd-18",
		avail:      zstdAvail,
		compress:   compressCmdline("zstd", "-c", "-18"),
		decompress: zstdDecompress,
	},
	{
		name:       "lz4-1",
		avail:      lz4Avail,
		compress:   compressCmdline("lz4", "-c", "-1"),
		decompress: lz4Decompress,
	},
	{
		name:       "lz4-9",
		avail:      lz4Avail,
		compress:   compressCmdline("lz4", "-c", "-9"),
		decompress: lz4Decompress,
	},
	{
		name:       "gzip-9",
		avail:      gzipAvail,
		compress:   compressCmdline("gzip", "-c", "-9"),
		decompress: gzipDecompress,
	},
	{
		name:       "gzip-1",
		avail:      gzipAvail,
		compress:   compressCmdline("gzip", "-c", "-1"),
		decompress: gzipDecompress,
	},
}

func fatalf(f string, args ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", args...)
	os.Exit(1)
}

var dashv bool

func logf(f string, args ...any) {
	if dashv {
		fmt.Fprintf(os.Stderr, f+"\n", args...)
	}
}

func main() {
	var dashname string
	var dashfile string
	flag.BoolVar(&dashv, "v", false, "verbose")
	flag.StringVar(&dashname, "name", "", "regex for compressors to run (or empty for all)")
	flag.StringVar(&dashfile, "f", "", "file to benchmark (default: internal silesia.tar corpus)")
	flag.Parse()

	var rx *regexp.Regexp
	if dashname != "" {
		var err error
		rx, err = regexp.Compile(dashname)
		if err != nil {
			fatalf("compiling -name: %s", err)
		}
	}

	var buf []byte
	var err error
	if dashfile != "" {
		buf, err = os.ReadFile(dashfile)
		if err != nil {
			fatalf("reading -f=%q: %s", dashfile, err)
		}
	} else {
		r, err := gzip.NewReader(bytes.NewReader(silesia))
		if err != nil {
			fatalf("unzipping silesia data: %s", err)
		}
		buf, err = io.ReadAll(r)
		if err != nil {
			fatalf("unzipping silesia data: %s", err)
		}
		r.Close()
	}

	fmt.Println("name, ratio, decompression speed (GiB/s)")
	for i := range compressors {
		if rx != nil && !rx.MatchString(compressors[i].name) {
			continue
		}
		benchmark(&compressors[i], buf)
	}
}
