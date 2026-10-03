package cmd

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/traceflow"
)

type TraceFlowOptions struct {
	Status, Resume                     bool
	Requeue, Rebind, RebindTo, Discard string
	ConfirmDiscard                     bool
}

func (o TraceFlowOptions) Requested() bool {
	return o.Status || o.Resume || o.Requeue != "" || o.Rebind != "" || o.RebindTo != "" || o.Discard != "" || o.ConfirmDiscard
}

func (o TraceFlowOptions) Validate() error {
	count := 0
	for _, selected := range []bool{o.Status, o.Resume, o.Requeue != "", o.Rebind != "", o.Discard != ""} {
		if selected {
			count++
		}
	}
	if count != 1 {
		return fmt.Errorf("select exactly one Trace Flow maintenance operation")
	}
	if (o.Rebind != "") != (o.RebindTo != "") {
		return fmt.Errorf("rebind requires both the old and new binding IDs")
	}
	if o.ConfirmDiscard && o.Discard == "" {
		return fmt.Errorf("discard confirmation requires an explicit selector")
	}
	if o.Discard != "" && !o.ConfirmDiscard {
		return fmt.Errorf("discard requires -trace-flow-confirm-discard")
	}
	return nil
}

func DoTraceFlow(cfg *config.Config, configPath string, options TraceFlowOptions, output io.Writer) (errResult error) {
	if output == nil {
		return fmt.Errorf("Trace Flow maintenance requires an output writer")
	}
	if errValidate := options.Validate(); errValidate != nil {
		return errValidate
	}
	if cfg == nil {
		return fmt.Errorf("Trace Flow maintenance requires configuration")
	}
	resolved, errResolve := cfg.TraceFlow.ResolvePaths(configPath, cfg.AuthDir)
	if errResolve != nil {
		return errResolve
	}
	state, errOpen := traceflow.OpenMaintenance(traceflow.Config{
		Enabled: resolved.Enabled, Endpoint: resolved.Endpoint, OutboxPath: resolved.OutboxPath,
		APIKeyEnv: resolved.APIKeyEnv, MaxPendingBytes: resolved.MaxPendingBytes, MinFreeBytes: resolved.MinFreeBytes,
	})
	if errOpen != nil {
		return errOpen
	}
	defer func() {
		if errClose := state.Close(); errClose != nil {
			if errResult == nil {
				errResult = errClose
			}
		}
	}()
	switch {
	case options.Resume:
		errResult = state.Resume()
	case options.Requeue != "":
		errResult = state.Requeue(options.Requeue)
	case options.Rebind != "":
		if _, errWrite := fmt.Fprintln(output, "Organization identity cannot be verified. Rebinding attests that both bindings belong to the same Organization."); errWrite != nil {
			return errWrite
		}
		errResult = state.Rebind(options.Rebind, options.RebindTo)
	case options.Discard != "":
		errResult = state.Discard(options.Discard, options.ConfirmDiscard)
	}
	if errResult != nil {
		return errResult
	}
	status, errStatus := state.Status()
	if errStatus != nil {
		return errStatus
	}
	return json.NewEncoder(output).Encode(status)
}
