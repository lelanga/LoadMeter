package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/lelanga/phoenix/internal/har"
	"github.com/lelanga/phoenix/internal/safety"
	"github.com/lelanga/phoenix/internal/service"
)

// runRecord implements `phoenix record` (script recording) and
// `phoenix record ca` (recording CA management).
func runRecord(args []string) error {
	if len(args) > 0 && args[0] == "ca" {
		return recordCA(args[1:])
	}
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	ws := fs.String("workspace", defaultWorkspace(), "workspace directory (recordings are saved in <workspace>/recordings)")
	addr := fs.String("listen", "127.0.0.1:8888", "proxy listen address (loopback only; port 0 picks a free port)")
	out := fs.String("out", "", "also copy the HAR to this file (owner-only permissions)")
	launch := fs.Bool("browser", false, "launch a local Chrome/Chromium/Edge with a temporary profile that uses the proxy (no CA install needed)")
	startURL := fs.String("url", "", "start URL for --browser")
	toScenario := fs.String("to-scenario", "", "on stop, import the recording as this scenario and print correlation candidates")
	insecure := fs.Bool("insecure-upstream", false, "do not verify upstream TLS certificates (only for self-signed test servers you control; audited)")
	var inc, exc stringList
	fs.Var(&inc, "include-host", "only record this host (exact or *.suffix; repeatable)")
	fs.Var(&exc, "exclude-host", "do not record this host (exact or *.suffix; repeatable); analytics/telemetry hosts are excluded by default")
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q (did you mean 'phoenix record ca'?)", fs.Arg(0))
	}
	svc, err := newService(*ws, nil)
	if err != nil {
		return err
	}
	defer svc.Close()
	if *insecure {
		safety.Audit("record.insecure_upstream", map[string]any{"includeHosts": []string(inc)})
		fmt.Fprintln(os.Stderr, "WARNING: --insecure-upstream disables certificate verification for upstream servers. Use it only for test servers you control.")
	}
	info, err := svc.StartRecording(service.RecordingOptions{Listen: *addr, Browser: *launch, StartURL: *startURL,
		IncludeHosts: inc, ExcludeHosts: exc, InsecureUpstream: *insecure})
	if err != nil {
		return err
	}
	fmt.Printf("Recording proxy: http://%s (HTTP and HTTPS, loopback clients only)\n", info.ProxyAddr)
	if info.CACreated {
		fmt.Printf("Created the local recording CA: %s\n", info.CACertPath)
	}
	if info.Browser {
		fmt.Printf("Launched %s with a temporary profile. Browse your application; closing the browser stops the recording.\n", info.BrowserPath)
	} else {
		fmt.Printf("Set http://%s as the HTTP and HTTPS proxy of your browser or client.\n"+
			"For HTTPS, the client must trust %s (see 'phoenix record ca'), or use --browser instead.\n", info.ProxyAddr, info.CACertPath)
	}
	fmt.Println("Type a transaction name and press Enter to start a transaction; an empty line ends it. Press Ctrl+C to stop.")
	fmt.Println("Note: recording is not limited by the allowlist, but recorded hosts must be allowlisted before load testing.")

	done, _ := svc.RecordingDone(info.ID)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	defer signal.Stop(sig)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			name := strings.TrimSpace(sc.Text())
			if _, err := svc.MarkTransaction(info.ID, name); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				continue
			}
			if name == "" {
				fmt.Println("-- transaction ended")
			} else {
				fmt.Printf("-- transaction %q started\n", name)
			}
		}
	}()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	seen := 0
	reason := ""
wait:
	for {
		select {
		case <-sig:
			reason = "interrupted"
			break wait
		case <-done:
			reason = "browser closed or time limit reached"
			break wait
		case <-tick.C:
			a := svc.ActiveRecording(seen)
			if a == nil {
				reason = "recording ended"
				break wait
			}
			for _, r := range a.Requests {
				tx := ""
				if r.Transaction != "" {
					tx = " [" + r.Transaction + "]"
				}
				fmt.Printf("  %3d %-6s %s%s\n", r.Status, r.Method, r.URL, tx)
				seen = r.Seq
			}
		}
	}
	fmt.Printf("stopping (%s)...\n", reason)
	res, err := svc.StopRecording(info.ID, *toScenario)
	if res != nil {
		fmt.Printf("captured %d requests from %d host(s): %s\nHAR saved: %s (owner-only permissions; contains raw credentials)\n",
			res.Captured, len(res.Hosts), strings.Join(res.Hosts, ", "), res.HARFile)
		if *out != "" {
			if b, rerr := os.ReadFile(res.HARFile); rerr == nil {
				if werr := os.WriteFile(*out, b, 0o600); werr != nil {
					fmt.Fprintln(os.Stderr, "error: copying HAR:", werr)
				} else {
					_ = os.Chmod(*out, 0o600)
					fmt.Println("copied to", *out)
				}
			}
		}
		if res.Note != "" {
			fmt.Println("note:", res.Note)
		}
	}
	if err != nil {
		return err
	}
	if res.Import == nil {
		fmt.Printf("next: phoenix import-har %s --name <scenario>   (or use --to-scenario next time)\n", res.HARFile)
		return nil
	}
	imp := res.Import
	fmt.Printf("scenario: %s\n", filepath.Join(svc.WS.ScenariosDir(), imp.File))
	printCompat(imp.Report)
	printCandidates(imp.Correlations, imp.Unexplained)
	if len(imp.Parameters) > 0 {
		fmt.Println("parameter suggestions:")
		for _, p := range imp.Parameters {
			fmt.Printf("  %s  %-14s %-10s mode=%s uses=%d\n", p.ID, p.Column, p.Kind, p.Mode, len(p.Uses))
		}
	}
	fmt.Printf("next: phoenix correlate %s --apply all   then: phoenix run %s\n", imp.File, filepath.Join(svc.WS.ScenariosDir(), imp.File))
	return nil
}

func recordCA(args []string) error {
	fs := flag.NewFlagSet("record ca", flag.ContinueOnError)
	export := fs.String("export", "", "write the CA certificate (PEM) to this file ('-' for stdout)")
	del := fs.Bool("delete", false, "delete the recording CA (certificate and private key)")
	if err := fs.Parse(reorder(fs, args)); err != nil {
		return err
	}
	dir := har.CADir()
	if *del {
		if err := har.DeleteCA(dir); err != nil {
			return err
		}
		safety.Audit("record.ca_deleted", map[string]any{"dir": dir})
		fmt.Printf("deleted the recording CA in %s.\nIf you installed it in an OS or browser trust store, remove \"Phoenix Performance Local Recording CA\" there too.\n", dir)
		return nil
	}
	ca, created, err := har.LoadOrCreateCA(dir)
	if err != nil {
		return err
	}
	if created {
		safety.Audit("record.ca_created", map[string]any{"dir": dir})
	}
	if *export == "-" {
		_, err := os.Stdout.Write(ca.CertPEM)
		return err
	}
	certPath := filepath.Join(dir, "ca.pem")
	if *export != "" {
		if _, err := os.Stat(*export); err == nil {
			return errors.New("refusing to overwrite " + *export)
		}
		if err := os.WriteFile(*export, ca.CertPEM, 0o644); err != nil {
			return err
		}
		certPath = *export
		fmt.Println("exported", *export)
	}
	fmt.Printf(`Recording CA: %s
  subject:  %s
  expires:  %s
  SPKI sha256 (base64): %s
  private key: %s (owner-only; never share it)

You do NOT need to install this CA when you record with 'phoenix record --browser':
the launched browser trusts only this CA's key, only for that session.

To record HTTPS from another browser or client, trust the certificate while recording
and remove it afterwards:
  Linux (system):      sudo cp %[6]s /usr/local/share/ca-certificates/phoenix-recording.crt && sudo update-ca-certificates
  Linux (Chrome/NSS):  certutil -d sql:$HOME/.pki/nssdb -A -t "C,," -n "Phoenix Recording CA" -i %[6]s
  macOS:               security add-trusted-cert -r trustRoot -k ~/Library/Keychains/login.keychain-db %[6]s
  Windows:             certutil -user -addstore Root %[6]s
  Firefox:             Settings > Privacy & Security > Certificates > View Certificates > Authorities > Import
  curl / scripts:      --cacert %[6]s (or SSL_CERT_FILE / NODE_EXTRA_CA_CERTS)

Anyone holding the private key can impersonate any HTTPS site to clients that trust it.
Delete it when you no longer record: phoenix record ca --delete
`, filepath.Join(dir, "ca.pem"), ca.Cert.Subject.CommonName, ca.Cert.NotAfter.Format("2006-01-02"), ca.SPKIHash(), filepath.Join(dir, "ca-key.pem"), certPath)
	return nil
}
