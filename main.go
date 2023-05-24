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
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/SnellerInc/sneller/ion/zion/iguana"
	"golang.org/x/sys/cpu"
)

//go:embed testdata/silesia.tar.gz
var silesia []byte

type compressor struct {
	name  string
	avail func() bool
	// bench should produce the cmdline for
	// running benchmarks against the given filepath
	bench func(infile string) []string
	// parse should read the input lines
	// and convert them into a compressed size
	// and throughput (in MB/s)
	parse func(lines []string) (int64, float64)
}

const iguanaWindowSize = 256 * 1024

func iguanaCompress(src []byte, threshold float64) []byte {
	var out []byte
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

func iguanaDecompress(dec *iguana.Decoder, dst, src []byte) ([]byte, error) {
	var err error
	for len(src) >= 4 {
		winsize := int(src[0]) + (int(src[1]) << 8) + (int(src[2]) << 16)
		src = src[3:]
		if len(src) < winsize {
			panic("invalid frame")
		}
		mem := src[:winsize]
		src = src[winsize:]
		dst, err = dec.DecompressTo(dst[:0], mem)
		if err != nil {
			return dst, err
		}
	}
	return dst[:0], nil
}

func benchMain() {
	var threshold float64
	flag.Float64Var(&threshold, "t", 1.0, "entropy coding threshold")
	flag.Parse()

	args := flag.Args()
	if len(args) != 1 {
		fatalf("usage: %s [-t threshold] <file>\n", os.Args[0])
	}

	buf, err := os.ReadFile(args[0])
	if err != nil {
		fatalf("reading file: %s", err)
	}

	comp := iguanaCompress(buf, threshold)
	start := time.Now()
	var tmp []byte
	var min time.Duration
	var dec iguana.Decoder
	deadline := start.Add(3 * time.Second)
	for time.Now().Before(deadline) {
		istart := time.Now()
		tmp, err = iguanaDecompress(&dec, tmp[:0], comp)
		if err != nil {
			fatalf("decompression error: %s", err)
		}
		dur := time.Since(istart)
		if min == 0 || dur < min {
			min = dur
		}
	}
	multiplier := (1e12) / float64(time.Second)
	// convert bytes / ns -> million bytes / second
	mbps := (float64(len(buf)) / float64(min)) * multiplier
	fmt.Printf("%d %.4g MB/s\n", len(comp), mbps)
}

func lookPath(name string) func() bool {
	return func() bool {
		name, _ = exec.LookPath(name)
		return name != ""
	}
}

var zstdAvail = lookPath("zstd")

var lz4Avail = lookPath("lz4")

func benchmark(c *compressor, filename string) (int64, float64) {
	cmdline := c.bench(filename)
	cmd := exec.Command(cmdline[0], cmdline[1:]...)
	buf, err := cmd.CombinedOutput()
	if err != nil {
		fatalf("%s: %s %s", cmdline[0], err, buf)
	}
	var lines []string
	cutset := "\r\n"
	for x := bytes.IndexAny(buf, cutset); x >= 0; x = bytes.IndexAny(buf, cutset) {
		lines = append(lines, string(buf[:x]))
		buf = buf[x+1:]
	}
	return c.parse(lines)
}

func benchSelf(threshold string) func(string) []string {
	return func(filename string) []string {
		return []string{
			selfexe(),
			"igbench",
			"-t=" + threshold,
			filename,
		}
	}
}

func benchProg(args ...string) func(string) []string {
	return func(filename string) []string {
		return append(args, filename)
	}
}

var (
	lookupSelf sync.Once
	exename    string
)

// return current executable path
func selfexe() string {
	if runtime.GOOS == "windows" {
		// no /proc/self/exe
		return os.Args[0]
	}
	lookupSelf.Do(func() {
		name, err := os.Readlink("/proc/self/exe")
		if err != nil {
			fatalf("couldn't read /proc/self/exe: %s", err)
		}
		exename = name
	})
	return exename
}

func readLast(rx string) func([]string) (int64, float64) {
	re, err := regexp.Compile(rx)
	if err != nil {
		panic(err)
	}
	return func(lines []string) (int64, float64) {
		// match the last line that has the right contents for the regexp:
		var matches []string
		for i := len(lines) - 1; i >= 0; i-- {
			matches = re.FindStringSubmatch(lines[i])
			if len(matches) == 3 {
				break
			}
		}
		if len(matches) != 3 {
			fatalf("unexpected lines: %#v", lines)
		}
		size, err := strconv.ParseInt(matches[1], 0, 64)
		if err != nil {
			fatalf("bad output size %s", matches[1])
		}
		rate, err := strconv.ParseFloat(matches[2], 64)
		if err != nil {
			fatalf("bad output rate %q", matches[2])
		}
		return size, rate
	}
}

// intended to work with zstd -b# and lz4 -b# output:
var lz4Parse = readLast(`->\s*([0-9]+) \(.*\),\s*[0-9\.]+ MB/s\s*,\s*([0-9\.]+) MB/s`)
var selfParse = readLast(`([0-9]+) ([0-9\.]+) MB/s`)

var compressors = []compressor{
	{
		name:  "iguana_avx512_ans",
		avail: func() bool { return cpu.X86.HasAVX512VBMI2 },
		bench: benchSelf("1"),
		parse: selfParse,
	},
	{
		name:  "iguana_avx512_noans",
		avail: func() bool { return cpu.X86.HasAVX512VBMI2 },
		bench: benchSelf("0"),
		parse: selfParse,
	},
	{
		name:  "zstd-9",
		avail: zstdAvail,
		bench: benchProg("zstd", "-b9"),
		parse: lz4Parse,
	},
	{
		name:  "zstd-1",
		avail: zstdAvail,
		bench: benchProg("zstd", "-b1"),
		parse: lz4Parse,
	},
	{
		name:  "zstd-18",
		avail: zstdAvail,
		bench: benchProg("zstd", "-b18"),
		parse: lz4Parse,
	},
	{
		name:  "lz4-1",
		avail: lz4Avail,
		bench: benchProg("lz4", "-b1"),
		parse: lz4Parse,
	},
	{
		name:  "lz4-9",
		avail: lz4Avail,
		bench: benchProg("lz4", "-b9"),
		parse: lz4Parse,
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
	if len(os.Args) > 1 && os.Args[1] == "igbench" {
		os.Args = append(os.Args[:1], os.Args[2:]...)
		benchMain()
		os.Exit(0)
	}
	var dashfile string
	flag.BoolVar(&dashv, "v", false, "verbose")
	flag.StringVar(&dashfile, "f", "", "file to benchmark (default: internal silesia.tar corpus)")
	flag.Parse()
	if flag.NArg() > 0 {
		fatalf("unexpected args: %v", flag.Args())
	}

	if dashfile == "" {
		tmpfile, err := os.CreateTemp("", "silesia.tar")
		if err != nil {
			fatalf("creating silesia tmpfile: %s", err)
		}
		r, err := gzip.NewReader(bytes.NewReader(silesia))
		if err != nil {
			fatalf("unzipping silesia data: %s", err)
		}
		_, err = io.Copy(tmpfile, r)
		if err != nil {
			fatalf("writing silesia data: %s", err)
		}
		r.Close()
		tmpfile.Close()
		dashfile = tmpfile.Name()
		defer os.Remove(dashfile)
	}

	info, err := os.Stat(dashfile)
	if err != nil {
		fatalf("stat %s: %s", dashfile, err)
	}
	isize := info.Size()

	fmt.Println("name, compression ratio, decompression speed (MB/s)")
	for i := range compressors {
		name := compressors[i].name
		if !compressors[i].avail() {
			logf("skipping %v (not available)\n", name)
			continue
		}
		osize, rate := benchmark(&compressors[i], dashfile)
		fmt.Printf("%s, %.3g, %.3g\n", name, float64(isize)/float64(osize), rate)
	}
}
