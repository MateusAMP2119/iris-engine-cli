package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/MateusAMP2119/iris-lakehouse/internal/api"
	"github.com/MateusAMP2119/iris-lakehouse/internal/config"
)

// connectResult is the machine-readable payload of `iris engine connect`, the
// --json data envelope: the verified host, the role its engine reported, and
// the iris.toml the connection was recorded in. It never carries the token.
type connectResult struct {
	Host   string `json:"host"`
	Role   string `json:"role"`
	Config string `json:"config"`
}

// engineConnect is the handler for `iris engine connect [host]`: point this
// workspace at an existing remote engine instead of provisioning one locally.
// It resolves the host (argument > --host/IRIS_HOST/iris.toml > interactive
// prompt) and the PAT (--token/IRIS_TOKEN > interactive hidden prompt), proves
// the pair against the engine's /healthz -- an unreachable host and a rejected
// PAT are distinct faults, and nothing is recorded on either -- then records
// host and token in the invoking directory's .iris/iris.toml (0600), the
// configuration layer every subsequent command in this workspace resolves
// through.
func (a *app) engineConnect() runE {
	return func(cmd *cobra.Command, args []string) error {
		settings := a.resolveTarget(cmd)
		jsonMode, _ := cmd.Flags().GetBool("json")
		interactive := !jsonMode && a.stdoutTTY() && a.stdinTTY()

		host := ""
		if len(args) == 1 {
			host = strings.TrimSpace(args[0])
		}
		if host == "" {
			host = settings.Host
		}
		if host == "" && interactive {
			line, err := a.connectReadLine("Engine host (host:port, https://host:port for TLS):")
			if err != nil {
				return &fault{code: exitOpFailed, codeStr: "connect_prompt",
					message: fmt.Sprintf("engine connect: read the host answer: %v", err)}
			}
			host = strings.TrimSpace(line)
		}
		if host == "" {
			return a.usage("iris engine connect needs the engine host: pass it as the argument (host:port, or https://host:port for TLS)")
		}

		token := settings.Token
		if token == "" && interactive {
			line, err := a.connectReadSecret("PAT (input hidden):")
			if err != nil {
				return &fault{code: exitOpFailed, codeStr: "connect_prompt",
					message: fmt.Sprintf("engine connect: read the PAT answer: %v", err)}
			}
			token = strings.TrimSpace(line)
		}
		if token == "" {
			return a.usage("iris engine connect needs a PAT for the remote engine: pass --token or set IRIS_TOKEN (mint one on the engine host: iris pat create --scope read, adding control to operate pipelines)")
		}

		target := settings
		target.Host = host
		target.Token = token
		health, err := a.probeRemoteEngine(cmd.Context(), target)
		if err != nil {
			return err
		}

		wd, err := os.Getwd()
		if err != nil {
			return &fault{code: exitOpFailed, codeStr: "connect_workspace",
				message: fmt.Sprintf("engine connect: resolve the current directory: %v", err)}
		}
		tomlPath := filepath.Join(wd, config.DirName, config.FileName)
		if err := config.UpsertTOMLFile(tomlPath, map[string]string{"host": host, "token": token}); err != nil {
			return &fault{code: exitOpFailed, codeStr: "connect_record",
				message: fmt.Sprintf("engine connect: record the connection: %v", err)}
		}

		if jsonMode {
			return json.NewEncoder(a.out).Encode(dataEnvelope{Data: connectResult{
				Host: host, Role: health.Role, Config: tomlPath,
			}})
		}
		p := a.newPainter(false)
		fmt.Fprintf(a.out, "%s\n", p.green(fmt.Sprintf("✓ connected to %s — role: %s", host, health.Role)))
		fmt.Fprintf(a.out, "%s\n", p.green("✓ recorded host and token in "+tomlPath+" (0600)"))
		fmt.Fprintln(a.out)
		fmt.Fprintf(a.out, "Every iris command run in %s now targets the remote engine:\n", wd)
		fmt.Fprintln(a.out, "  iris pipeline list    what runs there")
		fmt.Fprintln(a.out, "  iris run list         its run history")
		fmt.Fprintln(a.out, "Override per invocation with --socket or --host; disconnect by removing the host and token lines from the file.")
		return nil
	}
}

// probeRemoteEngine proves a remote host/token pair against the engine's
// GET /healthz -- the liveness-plus-role probe, PAT-gated on the TCP listener,
// so a 200 verifies both the transport and the token. The failure modes stay
// distinct: an unreachable host is no-daemon (exit 3, like every dial), a
// rejected PAT and any other non-2xx answer are operation failures (exit 4)
// naming what the engine said.
func (a *app) probeRemoteEngine(ctx context.Context, s config.Settings) (api.Health, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	pctx, cancel := context.WithTimeout(ctx, daemonProbeTimeout)
	defer cancel()

	client, base, overTCP := a.daemonHTTPClient(s)
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, base+"/healthz", nil)
	if err != nil {
		return api.Health{}, &fault{code: exitOpFailed, codeStr: "connect_failed",
			message: fmt.Sprintf("engine connect: build the probe request for %s: %v", s.Host, err)}
	}
	if overTCP && s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return api.Health{}, &fault{code: exitNoDaemon, codeStr: "connect_unreachable",
			message: fmt.Sprintf("no engine reachable at %s: %v (check the address; an engine serving TLS needs the https:// form)", s.Host, err)}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode == http.StatusUnauthorized {
		return api.Health{}, &fault{code: exitOpFailed, codeStr: "connect_unauthorized",
			message: fmt.Sprintf("the engine at %s rejected the PAT; mint one on the engine host (iris pat create --scope read) and retry", s.Host)}
	}
	if resp.StatusCode == http.StatusForbidden {
		return api.Health{}, &fault{code: exitOpFailed, codeStr: "connect_forbidden",
			message: fmt.Sprintf("the engine at %s accepted the PAT but it lacks the read scope this connection needs; mint one with iris pat create --scope read (adding control to operate pipelines)", s.Host)}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return api.Health{}, &fault{code: exitOpFailed, codeStr: "connect_failed",
			message: fmt.Sprintf("the engine at %s answered status %d from /healthz", s.Host, resp.StatusCode)}
	}
	var env struct {
		Data api.Health `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return api.Health{}, &fault{code: exitOpFailed, codeStr: "connect_failed",
			message: fmt.Sprintf("the host at %s answered /healthz but not with an engine readout: %v (is it an iris engine?)", s.Host, err)}
	}
	return env.Data, nil
}

// connectReadLine asks one line question through the connectInput seam, falling
// back to a plain read of the process stdin. The prompt is dialogue, so it goes
// to errOut, like every tour question.
func (a *app) connectReadLine(prompt string) (string, error) {
	if a.connectInput != nil {
		return a.connectInput(prompt)
	}
	fmt.Fprintf(a.errOut, "%s ", prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// connectReadSecret asks one hidden line question through the connectSecret
// seam, falling back to a no-echo terminal read (term.ReadPassword) -- or the
// plain line read when stdin is not a terminal, where there is no echo to
// suppress.
func (a *app) connectReadSecret(prompt string) (string, error) {
	if a.connectSecret != nil {
		return a.connectSecret(prompt)
	}
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		fmt.Fprintf(a.errOut, "%s ", prompt)
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(a.errOut)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	return a.connectReadLine(prompt)
}
