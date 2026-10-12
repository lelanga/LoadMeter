package tools

import (
	"context"

	"github.com/lelanga/loadmeter/internal/service"
)

func init() {
	Register(
		Tool{Name: "start_recording", Confirm: true,
			Description: "Start recording a user journey into a script: starts a local recording proxy (HTTP and HTTPS via Load Meter's local recording CA) and, with browser=true, launches a local Chrome/Chromium/Edge with a temporary profile that uses it. A human then browses the application; use mark_transaction to name steps and stop_recording to create the scenario. Requires confirm=true because it launches a browser on the user's machine. Recording is not limited by the allowlist, but recorded hosts must be allowlisted before load testing.",
			InputSchema: obj(map[string]any{
				"browser":      boolean("Launch a local Chromium-family browser configured for recording (default false: the user points a browser at proxyAddr)"),
				"startUrl":     str("First page to open in the launched browser (http:// or https://)"),
				"includeHosts": strs("Only record these hosts (exact or *.suffix); empty records all except analytics/telemetry"),
				"excludeHosts": strs("Hosts not to record (exact or *.suffix)"),
				"confirm":      boolean("Must be true: a human approved launching the recording"),
			}),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				return s.StartRecording(service.RecordingOptions{Browser: getBool(a, "browser"), StartURL: getStr(a, "startUrl"),
					IncludeHosts: getStrs(a, "includeHosts"), ExcludeHosts: getStrs(a, "excludeHosts")})
			}},
		Tool{Name: "recording_status", ReadOnly: true,
			Description: "Show the active recording: id, proxy address, captured request count, open transaction and recent requests (method, URL without query, status).",
			InputSchema: obj(map[string]any{"since": num("Only return requests with seq greater than this (default 0)")}),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				info := s.ActiveRecording(int(getNum(a, "since")))
				if info == nil {
					return map[string]any{"active": false}, nil
				}
				return map[string]any{"active": true, "recording": info}, nil
			}},
		Tool{Name: "mark_transaction",
			Description: "Start a named transaction in the active recording (requests captured from now on belong to it). Pass an empty name to end the current transaction.",
			InputSchema: obj(map[string]any{"id": str("Recording id from start_recording"), "name": str("Transaction name, e.g. Login; empty ends the transaction")}, "id", "name"),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				return s.MarkTransaction(getStr(a, "id"), getStr(a, "name"))
			}},
		Tool{Name: "stop_recording",
			Description: "Stop the active recording, close the launched browser and save the HAR (owner-only permissions) in the workspace. With toScenario, also import it as a scenario and return correlation candidates and parameter suggestions (same as import_har).",
			InputSchema: obj(map[string]any{"id": str("Recording id from start_recording"), "toScenario": str("Scenario name to create from the recording (optional)")}, "id"),
			Handler: func(ctx context.Context, s *service.Service, a map[string]any) (any, error) {
				return s.StopRecording(getStr(a, "id"), getStr(a, "toScenario"))
			}},
	)
}
