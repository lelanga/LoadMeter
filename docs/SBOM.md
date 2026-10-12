# Software bill of materials

Generated from `/home/claude/loadmeter` with the commands below.

## Toolchain

```text
$ go version
go version go1.24.7 linux/amd64
```

`go.mod` declares:

```text
module github.com/lelanga/loadmeter

go 1.24.7
```

## Go modules

```text
$ go list -m all
github.com/lelanga/loadmeter
```

The module list contains only Load Meter itself. `go.mod` has no `require` lines and there is no `go.sum`. Load Meter uses **only the Go standard library**. The web UI is plain JavaScript embedded in the binary and loads nothing from external hosts.

The compiled binary also contains the Go standard library and runtime from the toolchain version above.

## Optional tools used at run time

Load Meter does not require these. It calls them only for the features named:

| Tool | Used for |
|---|---|
| `security` (macOS) | Storing AI keys in the Keychain |
| `secret-tool` (Linux) | Storing AI keys in the Secret Service |
| `open`, `xdg-open`, `rundll32` | `loadmeter serve --open` |

## Reproduce

```bash
cd loadmeter
go version
go list -m all
grep -c '^require' go.mod     # prints 0
```
