# Install Phoenix Performance

Phoenix is a single binary named `phoenix` (`phoenix.exe` on Windows). It uses only the Go standard library and has the web UI embedded.

## Build from source

You need Go 1.24 (the module declares `go 1.24.7`).

```bash
git clone <your repo URL> phoenix
cd phoenix
go build -o bin/phoenix ./cmd/phoenix
bin/phoenix version
```

Expected output looks like `phoenix 0.1.0-dev (go1.24.7 linux/amd64)`.

To build for another system, set `GOOS` and `GOARCH`:

```bash
GOOS=windows GOARCH=amd64 go build -o dist/phoenix-windows-amd64.exe ./cmd/phoenix
```

## Prebuilt binaries

Prebuilt files live in `dist/`. They are named `phoenix-<os>-<arch>` (with `.exe` on Windows):

| OS | amd64 | arm64 |
|---|---|---|
| Linux | `phoenix-linux-amd64` | `phoenix-linux-arm64` |
| macOS | `phoenix-darwin-amd64` | `phoenix-darwin-arm64` |
| Windows | `phoenix-windows-amd64.exe` | `phoenix-windows-arm64.exe` |

On Linux and macOS, make the file executable and run it:

```bash
chmod +x dist/phoenix-linux-amd64
./dist/phoenix-linux-amd64 version
```

On macOS, the system may block a binary you downloaded. If so, allow it in System Settings, or remove the quarantine flag with `xattr -d com.apple.quarantine <file>`.

## install.sh

`install.sh` at the repo root installs Phoenix into `~/.local/bin` (or `$PREFIX/bin` if you set `PREFIX`).

```bash
sh install.sh
```

What it does:

1. Detects your OS (Linux or macOS) and CPU (amd64 or arm64).
2. If `dist/phoenix-<os>-<arch>` exists, it checks the file against `dist/SHA256SUMS` (when `sha256sum` is available) and copies it to `<bin>/phoenix`.
3. If there is no prebuilt file but Go is installed, it builds from source.
4. Otherwise it stops with an error.
5. It prints `phoenix version` and tells you how to add the folder to your `PATH`.

On Windows, use `dist/phoenix-windows-<arch>.exe` directly.

```bash
export PATH="$HOME/.local/bin:$PATH"
phoenix version
```

Read the script before you run it. It is a short shell script.

You can check the downloads yourself from the `dist/` folder:

```bash
cd dist && sha256sum -c SHA256SUMS
```

## Docker

```bash
docker build -t phoenix .
```

The image is built from a Go 1.24 Alpine stage and runs on a minimal distroless image as a non-root user. The entry point is `phoenix`. The default command is `serve --addr 127.0.0.1:7878 --workspace /data/phoenix-workspace`, with `PHOENIX_HOME=/data/.phoenix`. Keep your data in a volume mounted at `/data`.

The web UI binds **loopback only** (`127.0.0.1`) and refuses other addresses. Inside a container, a published port (`-p 7878:7878`) cannot reach a loopback-only listener. Use one of these:

1. Host networking (Linux). The UI is then reachable at the printed URL on your host:

   ```bash
   docker run --rm --network host -v phoenix-data:/data phoenix
   ```

2. Use the CLI commands instead of the UI. Anything after the image name replaces the default command:

   ```bash
   docker run --rm --network host -v "$PWD:/data" phoenix run scenarios/x.json
   docker run --rm -v phoenix-data:/data phoenix demo --duration 10s
   ```

`127.0.0.1` inside a container (without host networking) is the container itself. To test a real system from Docker you must allow its host (see [SECURITY.md](SECURITY.md)).

## Where Phoenix keeps files

| What | Where |
|---|---|
| Workspace (scenarios, runs, history, data) | `./phoenix-workspace`, or `--workspace DIR`, or `$PHOENIX_WORKSPACE` |
| Config: `policy.json`, `ai.json`, `audit.log`, `ai-activity.jsonl`, `keys.json` | `~/.phoenix`, or `$PHOENIX_HOME` |

## Check the installation: `phoenix doctor`

```bash
phoenix doctor
```

It prints one line per check:

| Check | What it tells you |
|---|---|
| runtime | Go version, OS, CPU count |
| binary | Where the binary is and its size |
| safety policy | Path of `policy.json`, `maxVUs`, number of allowlisted hosts |
| temp dir writable | Phoenix can write temporary files |
| loopback networking | Phoenix can open local ports |
| mock server self-test | A full HTTP round trip against the built-in mock server |
| open-file limit | Warns when the limit is below 4096. Raise it with `ulimit -n 65535` before high-concurrency tests |
| AI providers | Which API key variables are set (optional) |

`doctor` exits with code 1 if a check fails.

## Uninstall

Delete the binary. To remove settings and logs, delete `~/.phoenix`. To remove test data, delete the workspace folder.
