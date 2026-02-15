# ProfMate

Collects a pprof target and prints where CPU and memory went.

```sh
go build -o bin/profmate ./cmd/profmate
bin/profmate inspect http://localhost:6060 --duration 30s
```

Go 1.24+. Works offline when PROFMATE_MODEL is unset.
