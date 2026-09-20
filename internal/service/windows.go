package service

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

const ServiceName = "ovc-agent"
const ServiceDisplayName = "Open vCenter Agent"
const ServiceDescription = "Open vCenter Hyper-V agent - communicates via RabbitMQ"

// AgentService implements the Windows service interface
type AgentService struct {
	logger  *slog.Logger
	runFunc func(ctx context.Context) error
}

// NewAgentService creates a new Windows service wrapper
func NewAgentService(logger *slog.Logger, runFunc func(ctx context.Context) error) *AgentService {
	return &AgentService{
		logger:  logger,
		runFunc: runFunc,
	}
}

// Execute implements svc.Handler
func (s *AgentService) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const acceptedCmds = svc.AcceptStop | svc.AcceptShutdown

	changes <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start the agent in a goroutine
	errChan := make(chan error, 1)
	go func() {
		errChan <- s.runFunc(ctx)
	}()

	changes <- svc.Status{State: svc.Running, Accepts: acceptedCmds}

	for {
		select {
		case err := <-errChan:
			if err != nil {
				s.logger.Error("agent error", "error", err)
				return true, 1
			}
			return false, 0
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				s.logger.Info("service stop requested")
				changes <- svc.Status{State: svc.StopPending}
				cancel()
				// Wait for runFunc to finish
				select {
				case <-errChan:
				case <-time.After(30 * time.Second):
					s.logger.Warn("timeout waiting for agent shutdown")
				}
				return false, 0
			}
		}
	}
}

// IsWindowsService checks if we're running as a Windows service
func IsWindowsService() bool {
	isService, err := svc.IsWindowsService()
	if err != nil {
		return false
	}
	return isService
}

// RunAsService starts the program as a Windows service
func RunAsService(logger *slog.Logger, runFunc func(ctx context.Context) error) error {
	elog, err := eventlog.Open(ServiceName)
	if err != nil {
		return err
	}
	defer elog.Close()

	elog.Info(1, fmt.Sprintf("%s service starting", ServiceName))

	s := NewAgentService(logger, runFunc)
	err = svc.Run(ServiceName, s)
	if err != nil {
		elog.Error(1, fmt.Sprintf("%s service failed: %v", ServiceName, err))
		return err
	}

	elog.Info(1, fmt.Sprintf("%s service stopped", ServiceName))
	return nil
}

// InstallService installs the Windows service
func InstallService() error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("failed to connect to service manager: %v", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(ServiceName)
	if err == nil {
		s.Close()
		return fmt.Errorf("service %s already exists", ServiceName)
	}

	s, err = m.CreateService(ServiceName, exePath, mgr.Config{
		DisplayName:      ServiceDisplayName,
		Description:      ServiceDescription,
		StartType:        mgr.StartAutomatic,
		DelayedAutoStart: true,
	}, "run")
	if err != nil {
		return fmt.Errorf("failed to create service: %v", err)
	}
	defer s.Close()

	// Set up event logging
	err = eventlog.InstallAsEventCreate(ServiceName, eventlog.Error|eventlog.Warning|eventlog.Info)
	if err != nil {
		s.Delete()
		return fmt.Errorf("failed to set up event log: %v", err)
	}

	return nil
}

// UninstallService removes the Windows service
func UninstallService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("failed to connect to service manager: %v", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(ServiceName)
	if err != nil {
		return fmt.Errorf("service %s not found: %v", ServiceName, err)
	}
	defer s.Close()

	err = s.Delete()
	if err != nil {
		return fmt.Errorf("failed to delete service: %v", err)
	}

	eventlog.Remove(ServiceName)
	return nil
}

// StartService starts the installed Windows service
func StartService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("failed to connect to service manager: %v", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(ServiceName)
	if err != nil {
		return fmt.Errorf("service %s not found: %v", ServiceName, err)
	}
	defer s.Close()

	return s.Start("run")
}

// StopService stops the running Windows service
func StopService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("failed to connect to service manager: %v", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(ServiceName)
	if err != nil {
		return fmt.Errorf("service %s not found: %v", ServiceName, err)
	}
	defer s.Close()

	_, err = s.Control(svc.Stop)
	return err
}
