# Install Load Meter

Load Meter is a single binary named `loadmeter` (`loadmeter.exe` on Windows). It uses only the Go standard library and has the web UI embedded.

## Build from source

You need Go 1.24 (the module declares `go 1.24.7`).

```bash
git clone <your repo URL> loadmeter
cd loadmeter
go build -o bin/loadmeter ./cmd/loadmeter
bin/loadmeter version
```

Expected output looks like `loadmeter 0.1.0-dev (go1.24.7 linux/amd64)`.

To build for another system, set `GOOS` and `GOARCH`:

```bash
GOOS=windows GOARCH=amd64 go build -o dist/loadmeter-windows-amd64.exe ./cmd/loadmeter
```

## Prebuilt binaries

Prebuilt files live in `dist/`. They are named `loadmeter-<os>-<arch>` (with `.exe` on Windows):

| OS | amd64 | arm64 |
|---|---|---|
| Linux | `loadmeter-linux-amd64` | `loadmeter-linux-arm64` |
| macOS | `loadmeter-darwin-amd64` | `loadmeter-darwin-arm64` |
| Windows | `loadmeter-windows-amd64.exe` | `loadmeter-windows-arm64.exe` |

On Linux and macOS, make the file executable and run it:

```bash
chmod +x dist/loadmeter-linux-amd64
./dist/loadmeter-linux-amd64 version
```

On macOS, the system may block a binary you downloaded. If so, allow it in System Settings, or remove the quarantine flag with `xattr -d com.apple.quarantine <file>`.

## install.sh

`install.sh` at the repo root installs Load Meter into `~/.local/bin` (or `$PREFIX/bin` if you set `PREFIX`).

```bash
sh install.sh
```

What it does:

1. Detects your OS (Linux or macOS) and CPU (amd64 or arm64).
2. If `dist/loadmeter-<os>-<arch>` exists, it checks the file against `dist/SHA256SUMS` (when `sha256sum` is available) and copies it to `<bin>/loadmeter`.
3. If there is no prebuilt file but Go is installed, it builds from source.
4. Otherwise it stops with an error.
5. It prints `loadmeter version` and tells you how to add the folder to your `PATH`.

On Windows, use `dist/loadmeter-windows-<arch>.exe` directly. Double-clicking the .exe (no arguments) starts the web UI and opens your browser; the workspace is `%USERPROFILE%\LoadMeter-Workspace`. Close the console window to stop it.

```bash
export PATH="$HOME/.local/bin:$PATH"
loadmeter version
```

Read the script before you run it. It is a short shell script.

You can check the downloads yourself from the `dist/` folder:

```bash
cd dist && sha256sum -c SHA256SUMS
```

## Docker

```bash
docker build -t loadmeter .
```

The image is built from a Go 1.24 Alpine stage and runs on a minimal distroless image as a non-root user. The entry point is `loadmeter`. The default command is `serve --addr 127.0.0.1:7878 --workspace /data/loadmeter-workspace`, with `LOADMETER_HOME=/data/.loadmeter`. Keep your data in a volume mounted at `/data`.

The web UI binds **loopback only** (`127.0.0.1`) and refuses other addresses. Inside a container, a published port (`-p 7878:7878`) cannot reach a loopback-only listener. Use one of these:

1. Host networking (Linux). The UI is then reachable at the printed URL on your host:

   ```bash
   docker run --rm --network host -v loadmeter-data:/data loadmeter
   ```

2. Use the CLI commands instead of the UI. Anything after the image name replaces the default command:

   ```bash
   docker run --rm --network host -v "$PWD:/data" loadmeter run scenarios/x.json
   docker run --rm -v loadmeter-data:/data loadmeter demo --duration 10s
   ```

`127.0.0.1` inside a container (without host networking) is the container itself. To test a real system from Docker you must allow its host (see [SECURITY.md](SECURITY.md)).

## Where Load Meter keeps files

| What | Where |
|---|---|
| Workspace (scenarios, runs, history, data) | `./loadmeter-workspace`, or `--workspace DIR`, or `$LOADMETER_WORKSPACE` |
| Config: `policy.json`, `ai.json`, `audit.log`, `ai-activity.jsonl`, `keys.json` | `~/.loadmeter`, or `$LOADMETER_HOME` |

## Check the installation: `loadmeter doctor`

```bash
loadmeter doctor
```

It prints one line per check:

| Check | What it tells you |
|---|---|
| runtime | Go version, OS, CPU count |
| binary | Where the binary is and its size |
| safety policy | Path of `policy.json`, `maxVUs`, number of allowlisted hosts |
| temp dir writable | Load Meter can write temporary files |
| loopback networking | Load Meter can open local ports |
| mock server self-test | A full HTTP round trip against the built-in mock server |
| open-file limit | Warns when the limit is below 4096. Raise it with `ulimit -n 65535` before high-concurrency tests |
| AI providers | Which API key variables are set (optional) |

`doctor` exits with code 1 if a check fails.

## Uninstall

Delete the binary. To remove settings and logs, delete `~/.loadmeter`. To remove test data, delete the workspace folder.
