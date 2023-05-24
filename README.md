# Compressor Benchmark Tool

`compbench` is a simple tool that runs some compression benchmarks
and prints them in CSV format.

Currently, the tool supports `zstd`, `lz4`, and `iguana`.

## Usage

Just `go install` and invoke the tool.
The [silesia test corpus](https://sun.aei.polsl.pl/~sdeor/index.php?page=silesia) is embedded into the tool.

```console
$ go install github.com/SnellerInc/compbench@latest
$ compbench
```

If you do not have the requisite binaries installed (`zstd`, `lz4`, etc.),
then the tool will skip running benchmarks for those algorithms.

You can use `-f=filename` to override the internal test corpus
with a file from the local filesystem.
The tool reads the entire test corpus file into memory,
so it should be smaller than the available memory,
and large enough to effectively amortize the overhead of `exec`-ing
the compressor and decompressor sub-processes.
Files on the order of tens to hundreds of megabytes work well.
