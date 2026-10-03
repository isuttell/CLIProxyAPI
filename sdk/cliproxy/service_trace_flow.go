package cliproxy

import (
	"context"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/traceflow"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
)

func (s *Service) startTraceFlow() error {
	if s.cfg == nil || !s.cfg.TraceFlow.Enabled {
		return nil
	}
	resolved, errResolve := s.cfg.TraceFlow.ResolvePaths(s.configPath, s.cfg.AuthDir)
	if errResolve != nil {
		return errResolve
	}
	exporter, errOpen := traceflow.Open(traceFlowRuntimeConfig(resolved))
	if errOpen != nil {
		return fmt.Errorf("cliproxy: start Trace Flow exporter: %w", errOpen)
	}
	s.traceFlow = exporter
	s.traceFlowRequests = newExecutionDrain()
	s.serverOptions = append(s.serverOptions, api.WithMiddleware(s.traceFlowRequests.middleware()))
	s.traceFlowConfig = s.cfg.TraceFlow
	usage.RegisterNamedPlugin("trace-flow", exporter)
	return nil
}

func traceFlowRuntimeConfig(cfg config.TraceFlowConfig) traceflow.Config {
	return traceflow.Config{
		Enabled: cfg.Enabled, Endpoint: cfg.Endpoint, OutboxPath: cfg.OutboxPath,
		APIKeyEnv: cfg.APIKeyEnv, MaxPendingBytes: cfg.MaxPendingBytes, MinFreeBytes: cfg.MinFreeBytes,
	}
}

func (s *Service) validateTraceFlowReload(cfg *config.Config) error {
	if s.traceFlow == nil {
		if cfg.TraceFlow.Enabled {
			return fmt.Errorf("enabling Trace Flow requires a restart")
		}
		return nil
	}
	previous, next := s.traceFlowConfig, cfg.TraceFlow
	previous.Enabled, next.Enabled = false, false
	if previous != next {
		return fmt.Errorf("changing Trace Flow destination, state, key variable or capacity requires a restart")
	}
	return nil
}

func (s *Service) closeTraceFlow(ctx context.Context) error {
	if s.traceFlow == nil {
		return nil
	}
	if errClose := s.traceFlow.Close(ctx); errClose != nil {
		log.WithError(errClose).Error("Trace Flow exporter shutdown failed")
		return errClose
	}
	return nil
}

func (s *Service) waitTraceFlowProducers(ctx context.Context) error {
	if errHandlers := s.traceFlowRequests.wait(ctx); errHandlers != nil {
		return fmt.Errorf("drain execution handlers: %w", errHandlers)
	}
	if s.traceFlow != nil && s.coreManager != nil {
		if errStreams := s.coreManager.SealAndWaitStreamProducers(ctx); errStreams != nil {
			return fmt.Errorf("drain stream producers: %w", errStreams)
		}
	}
	return nil
}

func (s *Service) finishUsageDrain(ctx context.Context) error {
	if errDrain := usage.StopDefaultAndWait(ctx); errDrain != nil {
		return fmt.Errorf("drain usage: %w", errDrain)
	}
	return s.closeTraceFlow(ctx)
}

func (s *Service) drainTraceFlowUsage(ctx context.Context) error {
	if s.traceFlow == nil {
		usage.StopDefault()
		return nil
	}
	if errProducers := s.waitTraceFlowProducers(ctx); errProducers != nil {
		// A failed join must not close capture underneath a producer that is still unwinding.
		go func() {
			if errWait := s.waitTraceFlowProducers(context.Background()); errWait != nil {
				log.WithError(errWait).Error("Trace Flow producer drain failed")
				return
			}
			if errDrain := s.finishUsageDrain(context.Background()); errDrain != nil {
				log.WithError(errDrain).Error("Trace Flow deferred drain failed")
			}
		}()
		return errProducers
	}
	if errDrain := usage.StopDefaultAndWait(ctx); errDrain != nil {
		go func() {
			if errFinish := s.finishUsageDrain(context.Background()); errFinish != nil {
				log.WithError(errFinish).Error("Trace Flow deferred dispatch drain failed")
			}
		}()
		return fmt.Errorf("drain usage: %w", errDrain)
	}
	return s.closeTraceFlow(ctx)
}
