# Compressor Benchmark Tool

`compbench` is a simple tool that runs some compression benchmarks
and prints them in CSV format.

Currently, the tool supports `zstd`, `lz4`, and `iguana`.

## Run

Just `go install` and invoke the tool.
The [silesia test corpus](https://sun.aei.polsl.pl/~sdeor/index.php?page=silesia) is embedded into the tool.

```console
$ go install github.com/SnellerInc/compbench@latest
$ compbench
```
