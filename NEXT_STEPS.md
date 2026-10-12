# Next steps

1. Review and merge draft PR #1 (https://github.com/lelanga/LoadMeter/pull/1) into `main`.
2. Windows: double-click `loadmeter-windows-amd64.exe` (opens the web UI). Report any error text you see.
3. Add API keys via `loadmeter ai set-key <provider>` and run `loadmeter ai test` to live-verify each provider.
4. Run `loadmeter mcp install --client <your chatbot app>` and try it from Claude Desktop, Cursor, VS Code or Gemini CLI.
5. Build and run the Docker image on a machine with Docker; run the macOS binaries once.
6. Try your own JMeter and LoadRunner scripts with `loadmeter import-jmx` / `loadmeter import-lr`; send any report items that look wrong.
7. Then start the 0.2 roadmap items (see ROADMAP.md): live-verified AI, WebSocket/gRPC, signed releases, distributed runs from the UI.

## Second wave (not yet built)
- Extractor parity (G02), assertion parity (G07), functions/variables (G09: comma escaping, nested expansion), loops while/foreach/switch (G06), throughput timers and Poisson arrivals (G08), mix controllers (G10), G13-G20.
- Defects: jmx CSV delimiter export (`internal/jmx/export.go:452`), JTL grpThreads/allThreads columns.
- Browser smoke test of the Failures/Summary UI cards; rebuild dist and jars.
