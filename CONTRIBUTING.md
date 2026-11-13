# Contributing

The `zz_generated_*.go` files are generated from the API files in `shared/`. Do not edit them by hand: change `tools/generate.py` and regenerate. The generator needs Python 3.10 or newer, with no packages.

```bash
python3 tools/generate.py --check
go vet ./...
go test ./...
```

`shared/CONVENTIONS.md` holds the rules every Vojo library follows.
