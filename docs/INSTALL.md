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

`install.sh` at the repo root installs the right binary for your system into `~/.local/bin`. It downloads or copies the matching `phoenix-<os>-<arch>` file and names it `phoenix`.

```bash
sh install.sh
```

Make sure `~/.local/bin` is on your `PATH`:

```bash
export PATH="$HOME/.local/bin:$PATH"
phoenix version
```

Read the script before you run it. It is a normal shell script.

## Docker

```bash
docker build -t phoenix .
```

The image runs `phoenix serve`. The web UI binds **loopback only** (`127.0.0.1`) and refuses other addresses. Inside a container, a published port (`-p 7878:7878`) cannot reach a loopback-only listener. Use one of these options instead:

1. Host networking (Linux):

   ```bash
   docker run --rm --network host phoenix
   ```

   Data written inside the container is lost when it stops. Mount a volume for the workspace if you want to keep runs. Check the Dockerfile for the workspace path it uses.

2. Use the CLI commands instead of the UI. Override the command:

   ```bash
   docker run --rm phoenix demo --duration 10s
   ```

Targets on `127.0.0.1` inside the container are the container itself. To test a real system from Docker you must allow its host (see [SECURITY.md](SECURITY.md)).

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
