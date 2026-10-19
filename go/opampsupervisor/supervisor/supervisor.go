// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0
//
// Derived from opentelemetry-collector-contrib's cmd/opampsupervisor. Changed here: the OpAMP
// client is the shared core's (coreClient, go/opamp in platform mode) instead of opamp-go's, with
// its state file in the storage directory; it handles OpAMP connection settings offers itself, so
// onOpampConnectionSettings and its in-memory credentials are gone. A start that can't write the
// Collector's config fails instead of running it on a stale one, and a rollback that can't be
// written leaves the config state as it was. The collector executables the server offers are
// installed, with a rollback if the new one isn't healthy, and removed when no longer offered
// (agent_update.go). A remote config
// agent::remote_config_policy refuses is FAILED, and is neither run nor kept; without a policy,
// the start says the remote config is unrestricted. The files holding what the server sent are
// sealed with storage.state_key (sealed_storage.go). The Supervisor's own telemetry ignores
// OTEL_EXPORTER_OTLP_* variables (telemetry_env.go). The local OpAMP server and the bootstrap
// one take the collector only with a token made at each start (localserver.go), and its messages
// only with the Supervisor's instance UID; a request that never became a connection doesn't keep
// later collectors out. A Restart command is carried out on the agent loop.

package supervisor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/client"
	"github.com/open-telemetry/opamp-go/client/types"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/open-telemetry/opamp-go/server"
	serverTypes "github.com/open-telemetry/opamp-go/server/types"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/config/configtelemetry"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/service/telemetry/otelconftelemetry"
	"go.opentelemetry.io/contrib/bridges/otelzap"
	telemetryconfig "go.opentelemetry.io/contrib/otelconf/v0.3.0"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/multierr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/protobuf/proto"
	gyaml "gopkg.in/yaml.v3"

	"github.com/pydantic/opamp-clients/go/opamp"
	"github.com/pydantic/opamp-clients/go/opampsupervisor/internal/metadata"
	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/commander"
	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/config"
	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/extensions"
	supervisorTelemetry "github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/telemetry"
	"github.com/pydantic/opamp-clients/go/opampsupervisor/supervisor/verifier"
)

var (
	//go:embed templates/nooppipeline.yaml
	noopPipelineTpl string

	//go:embed templates/extratelemetryconfig.yaml
	extraConfigTpl string

	//go:embed templates/opampextension.yaml
	opampextensionTpl string

	//go:embed templates/owntelemetry.yaml
	ownTelemetryTpl string

	//go:embed templates/nooptelemetry.yaml
	noopTelemetry string

	lastRecvRemoteConfigFile       = "last_recv_remote_config.dat"
	lastWorkingRemoteConfigFile    = "last_working_remote_config.dat"
	lastRecvOwnTelemetryConfigFile = "last_recv_own_telemetry_config.dat"

	errNonMatchingInstanceUID = errors.New("received collector instance UID does not match expected UID set by the supervisor")
)

const (
	persistentStateFileName     = "persistent_state.yaml"
	agentConfigFileName         = "effective.yaml"
	agentLogFileName            = "agent.log"
	AllowNoPipelinesFeatureGate = "service.AllowNoPipelines"
)

const maxBufferedCustomMessages = 10

type configState struct {
	// Supervisor-assembled config to be given to the Collector.
	mergedConfig string
	// true if the server provided configmap was empty
	configMapIsEmpty bool
	// Changed here: the remote config the merged config was composed from (nil: none), which
	// the outcome of giving it to the Collector is reported about.
	remote *protobufs.AgentRemoteConfig
}

// agentHealth is a health report from the Collector, and which of its connections to the local
// OpAMP server sent it (0: none held it any more).
type agentHealth struct {
	*protobufs.ComponentHealth
	connection uint64
}

func (c *configState) equal(other *configState) bool {
	return other.mergedConfig == c.mergedConfig && other.configMapIsEmpty == c.configMapIsEmpty
}

type agentStartStatus string

var (
	agentStarting    agentStartStatus = "starting"
	agentNotStarting agentStartStatus = "notStarting"
)

type telemetrySettings struct {
	component.TelemetrySettings
	loggerProvider log.LoggerProvider
}

// Supervisor implements supervising of OpenTelemetry Collector and uses OpAMPClient
// to work with an OpAMP Server.
type Supervisor struct {
	runCtx       context.Context
	runCtxCancel context.CancelFunc

	pidProvider pidProvider

	// Commander that starts/stops the Agent process.
	commander *commander.Commander

	// Supervisor's own config.
	config config.Supervisor

	// configWriteMu is used to synchronize writes to the agent config file.
	// This ensures that configuration write through the OnConnect callback
	configWriteMu sync.Mutex

	agentDescription    *atomic.Value
	availableComponents *atomic.Value

	// Supervisor's persistent state
	persistentState *persistentState

	noopPipelineTemplate         *template.Template
	opampextensionTemplate       *template.Template
	extraTelemetryConfigTemplate *template.Template
	ownTelemetryTemplate         *template.Template

	// agentConn is the collector's connection to the local OpAMP server, while it is connected.
	agentConn localConnection

	// A config section to be added to the Collector's config to fetch its own telemetry.
	// TODO: store this persistently so that when starting we can compose the effective
	// config correctly.
	// https://github.com/open-telemetry/opentelemetry-collector-contrib/issues/21078
	agentConfigOwnTelemetrySection *atomic.Value

	// Internal config state for agent use. See the [configState] struct for more details.
	cfgState *atomic.Value

	// Final effective config of the Collector, as reported by the agent.
	// Stores the full *protobufs.EffectiveConfig so all named config files
	// are preserved when forwarding to the OpAMP server.
	effectiveConfig *atomic.Value

	// Last received remote config.
	remoteConfig atomic.Pointer[protobufs.AgentRemoteConfig]
	// Last known-good remote config.
	lastWorkingRemoteConfig atomic.Pointer[protobufs.AgentRemoteConfig]
	// If true, we are starting from a last known-good remote configuration.
	// This is used to help control behavior that must be different in this scenario.
	usingLastWorkingRemoteConfig atomic.Bool

	// A channel to indicate there is a new config to apply.
	hasNewConfig      chan struct{}
	connectionEnv     atomic.Pointer[map[string]string]
	connectionUpdates chan connectionUpdate
	// configApplyTimeout is the maximum time to wait for the agent to apply a new config.
	// After this time passes without the agent reporting health as OK, the agent is considered unhealthy.
	configApplyTimeout time.Duration
	// lastHealthFromClient is the last health status of the agent received from the client.
	lastHealthFromClient atomic.Pointer[agentHealth]
	// pendingConnectionSettings is the hash of the connection settings the collector's next
	// apply answers.
	pendingConnectionSettings atomic.Pointer[[]byte]
	// configRetargets says a remote config with the same body as the running one arrived: the
	// agent loop reports for it the running config's outcome.
	configRetargets chan struct{}

	// healthMu guards lastReportedHealth and its publication, so that a health
	// update and an OpAMP client replacement cannot interleave and lose the
	// newer value.
	healthMu sync.Mutex
	// lastReportedHealth is the last health the Supervisor published to the OpAMP
	// server, whether it originated from the agent or from process supervision.
	// A replacement OpAMP client is seeded from it, so that accepting connection
	// settings does not report a running agent as unhealthy. It is deliberately
	// distinct from lastHealthFromClient, which holds agent-reported health only
	// and takes part in config apply timeout decisions.
	lastReportedHealth *protobufs.ComponentHealth

	// The OpAMP client to connect to the OpAMP Server.
	opampClient client.OpAMPClient

	// storageSealer seals the files in the storage directory that hold what the server sent;
	// nil without storage.state_key (sealed_storage.go).
	storageSealer opamp.StateSealer

	// packageVerifier checks the signature of a collector executable the server offers, and
	// agentInstalls takes each to the agent loop, which installs it, or removes the one installed
	// (agent_update.go). packagedAgent is agent.executable as configured: the collector run when
	// the server has installed none.
	packageVerifier verifier.Verifier
	agentInstalls   chan agentInstall
	packagedAgent   string

	// restartRequests takes the server's Restart commands to the agent loop, which carries them
	// out. One pending covers any that come before the loop takes it.
	restartRequests chan struct{}

	doneChan chan struct{}
	agentWG  sync.WaitGroup

	customMessageToServer chan *protobufs.CustomMessage
	customMessageWG       sync.WaitGroup

	// agentReady is true if the agent has started and is fully ready.
	agentReady atomic.Bool
	// agentReadyChan is a channel that can be used to wait for the agent to
	// start in case [agentReady] is false.
	agentReadyChan chan struct{}

	// agentRestarting is true if the agent is restarting.
	agentRestarting atomic.Bool

	// The OpAMP server to communicate with the Collector's OpAMP extension
	opampServer     server.OpAMPServer
	opampServerPort int
	// localServerToken is the bearer token the collector's opamp extension sends the local
	// OpAMP server (and the bootstrap one), new at each Start: it is in the extension config
	// the Supervisor writes, and nowhere the Supervisor sends or logs.
	localServerToken string

	// The HTTP server for health check endpoint
	healthCheckServer   *http.Server
	healthCheckServerWG sync.WaitGroup

	telemetrySettings telemetrySettings

	featureGates map[string]struct{}
	metrics      *supervisorTelemetry.Metrics

	// heartbeatInterval is the interval the OpAMP client is configured to send heartbeats.
	// Default is 30 seconds but can be overridden by the OpAMP server with an OpAMPConnectionSettings message.
	heartbeatIntervalSeconds uint64

	// initialOpampConnSuccess indicates whether the Supervisor's
	// OpAMP client achieved a successful connection to the OpAMP server
	// at least once.
	initialOpampConnSuccess atomic.Bool

	// extensions hosts configured supervisor extensions. nil when none are configured.
	extensions *extensions.Extensions

	// passthroughLogBuffer keeps the latest Collector log lines when passthrough logging is enabled.
	passthroughLogBuffer *logRingBuffer
	passthroughLogMu     sync.Mutex
}

func NewSupervisor(ctx context.Context, logger *zap.Logger, cfg config.Supervisor) (*Supervisor, error) {
	s := &Supervisor{
		pidProvider:                    defaultPIDProvider{},
		hasNewConfig:                   make(chan struct{}, 1),
		configRetargets:                make(chan struct{}, 1),
		connectionUpdates:              make(chan connectionUpdate, 1),
		agentInstalls:                  make(chan agentInstall),
		restartRequests:                make(chan struct{}, 1),
		agentConfigOwnTelemetrySection: &atomic.Value{},
		cfgState:                       &atomic.Value{},
		effectiveConfig:                &atomic.Value{},
		agentDescription:               &atomic.Value{},
		availableComponents:            &atomic.Value{},
		doneChan:                       make(chan struct{}),
		customMessageToServer:          make(chan *protobufs.CustomMessage, maxBufferedCustomMessages),
		featureGates:                   map[string]struct{}{},
		agentReady:                     atomic.Bool{},
		agentReadyChan:                 make(chan struct{}, 1),
		metrics:                        &supervisorTelemetry.Metrics{},
		heartbeatIntervalSeconds:       30,
		initialOpampConnSuccess:        atomic.Bool{},
	}

	s.runCtx, s.runCtxCancel = context.WithCancel(ctx)

	// Validate extensions feature gate before continuing
	if len(cfg.Extensions) > 0 && !metadata.OpampsupervisorExtensionsFeatureGate.IsEnabled() {
		return nil, fmt.Errorf(
			"extensions are configured but the %q feature gate is not enabled; enable it with --feature-gates=%s",
			metadata.OpampsupervisorExtensionsFeatureGate.ID(), metadata.OpampsupervisorExtensionsFeatureGate.ID(),
		)
	}

	if err := s.createTemplates(); err != nil {
		return nil, err
	}

	if err := confmap.Validate(cfg); err != nil {
		return nil, fmt.Errorf("error validating config: %w", err)
	}
	s.config = cfg

	if err := os.MkdirAll(s.config.Storage.Directory, 0o700); err != nil {
		return nil, fmt.Errorf("error creating storage dir: %w", err)
	}

	var err error

	s.telemetrySettings, err = initTelemetrySettings(ctx, logger, s.config.Telemetry)
	if err != nil {
		return nil, err
	}

	s.metrics, err = supervisorTelemetry.NewMetrics(s.telemetrySettings.MeterProvider)
	if err != nil {
		return nil, fmt.Errorf("error creating internal metrics: %w", err)
	}

	s.configApplyTimeout = s.config.Agent.ConfigApplyTimeout

	// Changed here: say so, in the local log, when the server's remote config is unlimited.
	if logger != nil && s.config.Capabilities.AcceptsRemoteConfig && !s.config.Agent.RemoteConfigPolicy.Enabled() {
		logger.Warn("The remote config is unrestricted (no agent::remote_config_policy): " +
			"the server may have the collector read any file it can and send it anywhere")
	}

	return s, nil
}

func initTelemetrySettings(ctx context.Context, logger *zap.Logger, cfg config.Telemetry) (telemetrySettings, error) {
	readers := cfg.Metrics.Readers
	if cfg.Metrics.Level == configtelemetry.LevelNone {
		readers = []telemetryconfig.MetricReader{}
	}

	resourceCfg, err := buildSupervisorResourceConfig(ctx, logger, &cfg.Resource)
	if err != nil {
		return telemetrySettings{}, err
	}
	pcommonRes, err := resourceConfigToPcommon(ctx, resourceCfg)
	if err != nil {
		return telemetrySettings{}, err
	}

	var sdk telemetryconfig.SDK
	err = withoutOTLPExporterEnv(logger, func() error {
		var err error
		sdk, err = telemetryconfig.NewSDK(
			telemetryconfig.WithContext(ctx),
			telemetryconfig.WithOpenTelemetryConfiguration(
				telemetryconfig.OpenTelemetryConfiguration{
					MeterProvider: &telemetryconfig.MeterProvider{
						Readers: readers,
					},
					TracerProvider: &telemetryconfig.TracerProvider{
						Processors: cfg.Traces.Processors,
					},
					LoggerProvider: &telemetryconfig.LoggerProvider{
						Processors: cfg.Logs.Processors,
					},
					Resource: resourceCfg,
				},
			),
		)
		return err
	})
	if err != nil {
		return telemetrySettings{}, err
	}

	var lp log.LoggerProvider
	if len(cfg.Logs.Processors) > 0 {
		lp = sdk.LoggerProvider()
		logger = logger.WithOptions(zap.WrapCore(func(c zapcore.Core) zapcore.Core {
			core, err := zapcore.NewIncreaseLevelCore(zapcore.NewTee(
				c,
				otelzap.NewCore("github.com/pydantic/opamp-clients/go/opampsupervisor",
					otelzap.WithLoggerProvider(lp),
				),
			), zap.NewAtomicLevelAt(cfg.Logs.Level))
			if err != nil {
				panic(err)
			}
			return core
		}))
	}

	return telemetrySettings{
		component.TelemetrySettings{
			Logger:         logger,
			TracerProvider: sdk.TracerProvider(),
			MeterProvider:  sdk.MeterProvider(),
			Resource:       pcommonRes,
		},
		lp,
	}, nil
}

func (s *Supervisor) Start(ctx context.Context) error {
	var err error

	s.runCtx, s.runCtxCancel = context.WithCancel(ctx)

	// Changed here: the local OpAMP server (and the bootstrap one) takes the collector with this
	// token only. It had no authentication: anything that could reach it could speak for the
	// collector.
	if s.localServerToken, err = newLocalServerToken(); err != nil {
		return err
	}

	// Start extensions first for any subsequent actions that use them
	if len(s.config.Extensions) > 0 {
		var exts *extensions.Extensions
		exts, err = extensions.New(
			s.runCtx,
			s.config.Extensions,
			extensions.Factories(),
			s.telemetrySettings.TelemetrySettings,
		)
		if err != nil {
			return fmt.Errorf("failed to create extensions: %w", err)
		}
		if err = exts.Start(s.runCtx); err != nil {
			return fmt.Errorf("failed to start extensions: %w", err)
		}
		s.extensions = exts
	}

	if err = s.startHealthCheckServer(); err != nil {
		return fmt.Errorf("failed to start health check server: %w", err)
	}

	s.persistentState, err = loadOrCreatePersistentState(s.persistentStateFilePath(), s.config.Agent.InstanceID, s.telemetrySettings.Logger)
	if err != nil {
		return err
	}

	// Changed here: the Supervisor installs the collector executables the server offers.
	if s.config.Capabilities.AcceptsPackages {
		if s.packageVerifier, err = s.newPackageVerifier(); err != nil {
			return err
		}
		s.useInstalledAgent()
	}

	if err = s.getFeatureGates(); err != nil {
		return fmt.Errorf("could not get feature gates from the Collector: %w", err)
	}

	if err = s.getBootstrapInfo(); err != nil {
		return fmt.Errorf("could not get bootstrap info from the Collector: %w", err)
	}

	s.telemetrySettings.Logger.Info("Supervisor starting",
		zap.String("id", s.persistentState.InstanceID.String()))

	if err = s.startOpAMP(); err != nil {
		return fmt.Errorf("cannot start OpAMP client: %w", err)
	}

	s.configWriteMu.Lock()
	err = s.loadAndWriteInitialMergedConfig()
	s.configWriteMu.Unlock()
	if err != nil {
		return fmt.Errorf("failed loading initial config: %w", err)
	}

	flags := []string{
		"--config", s.agentConfigFilePath(),
	}
	featureGateFlag := s.getFeatureGateFlag()
	if len(featureGateFlag) > 0 {
		flags = append(flags, featureGateFlag...)
	}
	s.commander, err = commander.NewCommander(
		s.telemetrySettings.Logger,
		filepath.Join(s.config.Storage.Directory, agentLogFileName),
		s.config.Agent,
		flags...,
	)
	if err != nil {
		return err
	}
	s.commander.SetPassthroughLogHook(s.appendPassthroughLogLine)
	s.commander.SetEnvironmentProvider(func() map[string]string {
		if env := s.connectionEnv.Load(); env != nil {
			return *env
		}
		return nil
	})

	s.agentWG.Go(func() {
		s.runAgentProcess()
	})

	s.customMessageWG.Go(func() {
		s.forwardCustomMessagesToServerLoop()
	})

	return nil
}

func (s *Supervisor) getFeatureGates() error {
	cmd, err := commander.NewCommander(
		s.telemetrySettings.Logger,
		filepath.Join(s.config.Storage.Directory, agentLogFileName),
		s.config.Agent,
		"featuregate",
	)
	if err != nil {
		return err
	}

	stdout, _, err := cmd.StartOneShot()
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(bytes.NewBuffer(stdout))

	// First line only contains headers, discard it.
	_ = scanner.Scan()
	for scanner.Scan() {
		line := scanner.Text()
		before, _, _ := strings.Cut(line, " ")
		flag := before

		if flag == AllowNoPipelinesFeatureGate {
			s.featureGates[AllowNoPipelinesFeatureGate] = struct{}{}
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "reading standard input:", err)
	}

	return nil
}

func (s *Supervisor) createTemplates() error {
	var err error

	if s.noopPipelineTemplate, err = template.New("nooppipeline").Parse(noopPipelineTpl); err != nil {
		return err
	}
	if s.extraTelemetryConfigTemplate, err = template.New("extratelemetryconfig").Parse(extraConfigTpl); err != nil {
		return err
	}
	if s.opampextensionTemplate, err = template.New("opampextension").Parse(opampextensionTpl); err != nil {
		return err
	}
	// Changed here: the server's endpoints and headers go into the config as YAML strings the
	// collector expands nothing in. They went in as written: a " or \ broke the config (which
	// was saved, and failed the next start), a \x escape changed the credential, and ${env:...}
	// or ${file:...} read the host's secrets into the request the server receives.
	ownTelemetryFuncs := template.FuncMap{"yamlString": yamlString}
	if s.ownTelemetryTemplate, err = template.New("owntelemetry").Funcs(ownTelemetryFuncs).Parse(ownTelemetryTpl); err != nil {
		return err
	}

	return nil
}

// getBootstrapInfo obtains the Collector's agent description by
// starting a Collector with a specific config that only starts
// an OpAMP extension, obtains the agent description, then
// shuts down the Collector. This only needs to happen
// once per Collector binary.
func (s *Supervisor) getBootstrapInfo() (err error) {
	_, span := s.getTracer().Start(s.runCtx, "GetBootstrapInfo")
	defer span.End()

	s.opampServerPort, err = s.getSupervisorOpAMPServerPort()
	if err != nil {
		span.SetStatus(codes.Error, fmt.Sprintf("Could not get supervisor opamp service port: %v", err))
		return err
	}

	bootstrapConfig, err := s.composeNoopConfig()
	if err != nil {
		span.SetStatus(codes.Error, fmt.Sprintf("Could not compose noop config config: %v", err))
		return err
	}

	err = os.WriteFile(s.agentConfigFilePath(), bootstrapConfig, 0o600)
	if err != nil {
		span.SetStatus(codes.Error, fmt.Sprintf("Failed to write agent config: %v", err))
		return fmt.Errorf("failed to write agent config: %w", err)
	}

	srv := server.New(newLoggerFromZap(s.telemetrySettings.Logger, "opamp-server"))

	done := make(chan error, 1)
	var connected atomic.Bool
	var doneReported atomic.Bool

	// Start a one-shot server to get the Collector's agent description
	// and available components using the Collector's OpAMP extension.
	err = srv.Start(flattenedSettings{
		endpoint: fmt.Sprintf("localhost:%d", s.opampServerPort),
		onConnecting: func(req *http.Request) (bool, int) {
			if !s.authorizedLocalRequest(req) {
				return false, http.StatusUnauthorized
			}
			connected.Store(true)
			return true, http.StatusOK
		},
		onMessage: func(_ serverTypes.Connection, message *protobufs.AgentToServer) *protobufs.ServerToAgent {
			response := &protobufs.ServerToAgent{}

			if !slices.Equal(message.GetInstanceUid(), s.persistentState.InstanceID[:]) {
				done <- fmt.Errorf(
					"the Collector's instance ID (%s) does not match with the instance ID set by the Supervisor (%s): %w",
					hex.EncodeToString(message.GetInstanceUid()),
					s.persistentState.InstanceID.String(),
					errNonMatchingInstanceUID,
				)
				return response
			}

			if message.GetAvailableComponents() != nil {
				s.setAvailableComponents(message.AvailableComponents)
			}

			if message.AgentDescription != nil {
				s.setAgentDescription(message.AgentDescription)
			}

			// agent description must be defined
			_, ok := s.agentDescription.Load().(*protobufs.AgentDescription)
			if !ok {
				return response
			}

			// if available components have not been reported, agent description is sufficient to continue
			availableComponents, availableComponentsOk := s.availableComponents.Load().(*protobufs.AvailableComponents)
			if availableComponentsOk {
				// must have a full list of components if available components have been reported
				if availableComponents.GetComponents() != nil {
					if !doneReported.Load() {
						done <- nil
						doneReported.Store(true)
					}
				} else {
					// if we don't have a full component list, ask for it
					response.Flags = uint64(protobufs.ServerToAgentFlags_ServerToAgentFlags_ReportAvailableComponents)
				}
				return response
			}

			// need to only report done once, not on each message - otherwise, we get a hung thread
			if !doneReported.Load() {
				done <- nil
				doneReported.Store(true)
			}

			return response
		},
	}.toServerSettings())
	if err != nil {
		span.SetStatus(codes.Error, fmt.Sprintf("Could not start OpAMP server: %v", err))
		return err
	}

	defer func() {
		if stopErr := srv.Stop(s.runCtx); stopErr != nil {
			err = errors.Join(err, fmt.Errorf("error when stopping the opamp server: %w", stopErr))
		}
	}()

	flags := []string{
		"--config", s.agentConfigFilePath(),
	}
	featuregateFlag := s.getFeatureGateFlag()
	if len(featuregateFlag) > 0 {
		flags = append(flags, featuregateFlag...)
	}
	cmd, err := commander.NewCommander(
		s.telemetrySettings.Logger,
		filepath.Join(s.config.Storage.Directory, agentLogFileName),
		s.config.Agent,
		flags...,
	)
	if err != nil {
		span.SetStatus(codes.Error, fmt.Sprintf("Could not start Agent: %v", err))
		return err
	}

	if err = cmd.Start(s.runCtx); err != nil {
		span.SetStatus(codes.Error, fmt.Sprintf("Could not start Agent: %v", err))
		return err
	}

	defer func() {
		if stopErr := cmd.Stop(s.runCtx); stopErr != nil {
			err = errors.Join(err, fmt.Errorf("error when stopping the collector: %w", stopErr))
		}
	}()

	select {
	case <-time.After(s.config.Agent.BootstrapTimeout):
		if connected.Load() {
			msg := "collector connected but never responded with an AgentDescription message"
			span.SetStatus(codes.Error, msg)
			return errors.New(msg)
		} else {
			msg := "collector's OpAMP client never connected to the Supervisor"
			span.SetStatus(codes.Error, msg)
			return errors.New(msg)
		}
	case err = <-done:
		if errors.Is(err, errNonMatchingInstanceUID) {
			// try to report the issue to the OpAMP server
			// Changed here: the client starts with the unhealthy status (startOpAMPClient
			// publishes the last reported health), so its first message carries it; set after
			// the start, it could be lost to the stop right after. And the bootstrap fails
			// whether or not the report is made: the report's outcome replaced the mismatch, so
			// a report made let Start go on.
			s.healthMu.Lock()
			s.lastReportedHealth = &protobufs.ComponentHealth{Healthy: false, LastError: err.Error()}
			s.healthMu.Unlock()
			if startOpAMPErr := s.startOpAMPClient(); startOpAMPErr == nil {
				defer func(s *Supervisor) {
					if stopErr := s.stopOpAMPClient(); stopErr != nil {
						s.telemetrySettings.Logger.Error("Could not stop OpAmp client", zap.Error(stopErr))
					}
				}(s)
			} else {
				s.telemetrySettings.Logger.Error("Could not start OpAMP client to report health to server", zap.Error(startOpAMPErr))
			}
		}
		if err != nil {
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, "")
		}
		return err
	}
}

func (s *Supervisor) startOpAMP() error {
	if err := s.startOpAMPClient(); err != nil {
		return err
	}

	if err := s.startOpAMPServer(); err != nil {
		return err
	}

	return nil
}

func (s *Supervisor) startOpAMPClient() error {
	// determine if we need to load a TLS config or not
	var tlsConfig *tls.Config
	parsedURL, err := url.Parse(s.config.Server.Endpoint)
	if err != nil {
		return fmt.Errorf("parse server endpoint: %w", err)
	}
	if parsedURL.Scheme == "wss" || parsedURL.Scheme == "https" {
		tlsConfig, err = s.config.Server.TLS.LoadTLSConfig(s.runCtx)
		if err != nil {
			return err
		}
	}
	switch parsedURL.Scheme {
	case "ws", "wss", "http", "https":
		// The shared core's client, over either transport. Changed here: it keeps the OpAMP
		// connection settings the server issues in the storage directory, sealed with state_key.
		if err := s.loadStorageSealer(); err != nil {
			return err
		}
		stateFile := filepath.Join(s.config.Storage.Directory, coreClientStateFile)
		c := newCoreClient(s.telemetrySettings.Logger.Named("opamp-client"), stateFile, s.storageSealer)
		if s.config.Capabilities.AcceptsPackages {
			c.installPackage = s.onInstallPackage
		}
		s.opampClient = c
	default:
		return fmt.Errorf("unsupported scheme in server endpoint: %q", parsedURL.Scheme)
	}

	s.telemetrySettings.Logger.Debug("Connecting to OpAMP server...", zap.String("endpoint", s.config.Server.Endpoint), zap.Any("headers", s.config.Server.OpaqueHeaders()))
	settings := types.StartSettings{
		OpAMPServerURL:     s.config.Server.Endpoint,
		Header:             s.config.Server.Headers,
		TLSConfig:          tlsConfig,
		InstanceUid:        types.InstanceUid(s.persistentState.InstanceID),
		RemoteConfigStatus: s.persistentState.GetLastRemoteConfigStatus(),
		Callbacks: types.Callbacks{
			OnConnect: s.onConnect,
			OnConnectFailed: func(_ context.Context, err error) {
				s.telemetrySettings.Logger.Error("Failed to connect to the OpAMP server", zap.Error(err))
			},
			OnError: func(_ context.Context, err *protobufs.ServerErrorResponse) {
				s.telemetrySettings.Logger.Error("Server returned an error response", zap.String("message", err.ErrorMessage))
			},
			OnMessage: s.onMessage,
			// OpAMP connection settings offers are the core client's: verified, kept and used
			// across restarts without the Supervisor.
			OnCommand: func(_ context.Context, command *protobufs.ServerToAgentCommand) error {
				cmdType := command.GetType()
				if *cmdType.Enum() == protobufs.CommandType_CommandType_Restart {
					return s.handleRestartCommand()
				}
				return nil
			},
			SaveRemoteConfigStatus: func(context.Context, *protobufs.RemoteConfigStatus) {
				// TODO: https://github.com/open-telemetry/opentelemetry-collector-contrib/issues/21079
			},
			GetEffectiveConfig: func(context.Context) (*protobufs.EffectiveConfig, error) {
				return s.createEffectiveConfigMsg(), nil
			},
		},
	}

	// Find and use auth extension if configured with non-empty reference
	if s.config.Server.Auth != (component.ID{}) {
		headerFunc, err := extensions.MakeHeadersFunc(
			s.telemetrySettings.Logger,
			s.config.Server.Auth,
			s.extensions.GetExtensions(),
		)
		if err != nil {
			return fmt.Errorf("failed to create auth header function: %w", err)
		}
		settings.HeaderFunc = headerFunc
	}

	// Changed here: without the collector's description (a bootstrap that failed before it came,
	// reporting why) the client describes the agent with the configured attributes only. It
	// panicked.
	ad, ok := s.agentDescription.Load().(*protobufs.AgentDescription)
	if !ok {
		ad = s.configuredAgentDescription()
	}
	if err := s.opampClient.SetAgentDescription(ad); err != nil {
		return err
	}

	// Replacing the client must not reset the agent's health. The agent reports
	// aggregate status changes only, so health published here is not corrected
	// until the agent's health changes again, which for a healthy agent means not
	// until its process restarts. When no health has been published yet this is
	// the initial client, and unhealthy is published as before.
	if err := s.publishLastReportedHealth(); err != nil {
		return err
	}

	if ac, ok := s.availableComponents.Load().(*protobufs.AvailableComponents); ok && ac != nil {
		if err := s.opampClient.SetAvailableComponents(ac); err != nil {
			return err
		}
	}

	capabilities := s.config.Capabilities
	if len(s.config.Agent.ConnectionEnv) == 0 {
		capabilities.AcceptsOtherConnectionSettings = false
	}
	supportedCapabilities := capabilities.SupportedCapabilities()
	if err := s.opampClient.SetCapabilities(&supportedCapabilities); err != nil {
		return err
	}

	// Set heartbeat interval if the agent supports it
	if s.config.Capabilities.ReportsHeartbeat {
		d := time.Duration(s.heartbeatIntervalSeconds) * time.Second
		settings.HeartbeatInterval = &d
	}

	s.telemetrySettings.Logger.Debug("Starting OpAMP client...")
	if err := s.opampClient.Start(s.runCtx, settings); err != nil {
		return err
	}
	s.telemetrySettings.Logger.Debug("OpAMP client started.")

	return nil
}

func (s *Supervisor) startHealthCheckServer() error {
	if s.config.HealthCheck.Port() == 0 {
		return nil
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		if s.persistentState == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("persistent state is nil"))
			return
		}

		cfg, ok := s.cfgState.Load().(*configState)
		if !ok {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("config state is nil"))
			return
		}
		if cfg == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("config state is nil"))
			return
		}

		w.WriteHeader(http.StatusOK)
	})

	healthCheckServerPort := s.config.HealthCheck.Port()
	server, err := s.config.HealthCheck.ServerConfig.ToServer(
		s.runCtx,
		nil,
		s.telemetrySettings.TelemetrySettings,
		mux,
	)
	if err != nil {
		return fmt.Errorf("failed to create health check server: %w", err)
	}
	s.healthCheckServer = server

	listener, err := s.config.HealthCheck.ServerConfig.ToListener(s.runCtx)
	if err != nil {
		return fmt.Errorf("failed to listen on port %d: %w", healthCheckServerPort, err)
	}

	s.healthCheckServerWG.Go(func() {
		s.telemetrySettings.Logger.Debug("Starting health check server", zap.Int64("port", healthCheckServerPort))
		if err := s.healthCheckServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			s.telemetrySettings.Logger.Error("Health check server failed", zap.Error(err))
		}
	})

	return nil
}

type nopHost struct{}

var _ component.Host = nopHost{}

func (nopHost) GetExtensions() map[component.ID]component.Component {
	return nil
}

// startOpAMPServer starts an OpAMP server that will communicate
// with an OpAMP extension running inside a Collector to receive
// data from inside the Collector. The internal server's lifetime is not
// matched to the Collector's process, but may be restarted
// depending on information received by the Supervisor from the remote
// OpAMP server.
func (s *Supervisor) startOpAMPServer() error {
	s.opampServer = server.New(newLoggerFromZap(s.telemetrySettings.Logger, "opamp-server"))

	var err error
	s.opampServerPort, err = s.getSupervisorOpAMPServerPort()
	if err != nil {
		return err
	}

	s.telemetrySettings.Logger.Debug("Starting OpAMP server...")

	// Changed here: one collector at a time, held by its connection from OnConnected to
	// OnConnectionClose (agentConn). Taken in OnConnecting, a request that never became a
	// connection (one that failed to decode, a failed upgrade) held it for good, and every
	// collector connection after it was refused.
	err = s.opampServer.Start(flattenedSettings{
		endpoint: fmt.Sprintf("localhost:%d", s.opampServerPort),
		onConnecting: func(req *http.Request) (bool, int) {
			if !s.authorizedLocalRequest(req) {
				return false, http.StatusUnauthorized
			}
			if s.agentConn.busy() {
				return false, http.StatusConflict
			}
			return true, http.StatusOK
		},
		onConnected: func(conn serverTypes.Connection) {
			if !s.agentConn.claim(conn) {
				// Another connection was made since this one's OnConnecting.
				s.telemetrySettings.Logger.Warn("Refusing a second connection to the local OpAMP server: a collector is connected")
				_ = conn.Disconnect()
			}
		},
		onMessage: func(conn serverTypes.Connection, message *protobufs.AgentToServer) *protobufs.ServerToAgent {
			if !s.agentConn.holds(conn) {
				return &protobufs.ServerToAgent{}
			}
			return s.handleAgentOpAMPMessage(conn, message)
		},
		onConnectionClose: s.agentConn.release,
	}.toServerSettings())
	if err != nil {
		return err
	}

	s.telemetrySettings.Logger.Debug("OpAMP server started.")

	return nil
}

func (s *Supervisor) handleAgentOpAMPMessage(conn serverTypes.Connection, message *protobufs.AgentToServer) *protobufs.ServerToAgent {
	ctx, span := s.getTracer().Start(s.runCtx, "handleAgentOpAMPMessage")
	defer span.End()

	// Changed here: the collector's messages are taken with the Supervisor's instance UID only;
	// it was checked at bootstrap alone.
	if id := s.persistentState.instanceID(); !slices.Equal(message.GetInstanceUid(), id[:]) {
		s.telemetrySettings.Logger.Warn("Ignoring a message to the local OpAMP server with another instance UID",
			zap.String("instance_uid", hex.EncodeToString(message.GetInstanceUid())),
			zap.String("expected_instance_uid", id.String()))
		return &protobufs.ServerToAgent{}
	}

	s.telemetrySettings.Logger.Debug("Received OpAMP message from the agent")
	if message.AgentDescription != nil {
		s.setAgentDescription(message.AgentDescription)
		ad := s.agentDescription.Load().(*protobufs.AgentDescription)
		if err := s.opampClient.SetAgentDescription(ad); err != nil {
			span.SetStatus(codes.Error, fmt.Sprintf("Could not update agent description: %s", err.Error()))
			s.telemetrySettings.Logger.Error("The OpAMP client failed to update the agent description", zap.Error(err))
		}
	}

	if message.EffectiveConfig != nil {
		configMap := message.EffectiveConfig.GetConfigMap()
		if configMap == nil || len(configMap.GetConfigMap()) == 0 {
			s.telemetrySettings.Logger.Debug("Received effective config with empty config map, skipping")
		} else {
			span.AddEvent("Received effectiveConfig")
			s.telemetrySettings.Logger.Debug("Received effective config from agent")
			s.effectiveConfig.Store(message.EffectiveConfig)
			err := s.opampClient.UpdateEffectiveConfig(ctx)
			if err != nil {
				span.SetStatus(codes.Error, fmt.Sprintf("Could not update effective config: %s", err.Error()))
				s.telemetrySettings.Logger.Error("The OpAMP client failed to update the effective config", zap.Error(err))
			}
		}
	}

	// Proxy client capabilities to server
	if message.CustomCapabilities != nil {
		span.AddEvent("Received customCapabilities")
		err := s.opampClient.SetCustomCapabilities(message.CustomCapabilities)
		if err != nil {
			span.SetStatus(codes.Error, fmt.Sprintf("Failed to send custom capabilities to OpAMP server: %s", err.Error()))
			s.telemetrySettings.Logger.Error("Failed to send custom capabilities to OpAMP server")
		}
	}

	// Proxy agent custom messages to server
	if message.CustomMessage != nil {
		select {
		case s.customMessageToServer <- message.CustomMessage:
		default:
			s.telemetrySettings.Logger.Warn(
				"Buffer full, skipping forwarding custom message to server",
				zap.String("capability", message.CustomMessage.Capability),
				zap.String("type", message.CustomMessage.Type),
			)
		}
	}

	if message.Health != nil {
		span.AddEvent("Received health", trace.WithAttributes(attribute.KeyValue{
			Key:   "health",
			Value: attribute.BoolValue(message.Health.Healthy),
		}))
		s.telemetrySettings.Logger.Debug("Received health status from agent", zap.Bool("healthy", message.Health.Healthy))
		connection, _ := s.agentConn.held(conn)
		s.lastHealthFromClient.Store(&agentHealth{ComponentHealth: message.Health, connection: connection})
		err := s.SetHealth(message.Health)
		if err != nil {
			s.telemetrySettings.Logger.Error("Could not report health to OpAMP server", zap.Error(err))
		}
		if !s.agentReady.Load() && message.Health.Healthy {
			s.usingLastWorkingRemoteConfig.Store(false)
			s.markAgentReady()
		}
	}

	return &protobufs.ServerToAgent{}
}

func (s *Supervisor) forwardCustomMessagesToServerLoop() {
	for {
		select {
		case cm := <-s.customMessageToServer:
			for {
				sendingChan, err := s.opampClient.SendCustomMessage(cm)
				switch {
				case errors.Is(err, types.ErrCustomMessagePending):
					s.telemetrySettings.Logger.Debug("Custom message pending, waiting to send...")
					<-sendingChan
					continue
				case err == nil: // OK
					s.telemetrySettings.Logger.Debug("Custom message forwarded to server.")
				default:
					s.telemetrySettings.Logger.Error("Failed to send custom message to OpAMP server")
				}
				break
			}
		case <-s.doneChan:
			return
		}
	}
}

// setAgentDescription sets the agent description, merging in any user-specified attributes from the supervisor configuration.
func (s *Supervisor) setAgentDescription(ad *protobufs.AgentDescription) {
	ad.IdentifyingAttributes = applyKeyValueOverrides(s.config.Agent.Description.IdentifyingAttributes, ad.IdentifyingAttributes)
	ad.NonIdentifyingAttributes = applyKeyValueOverrides(s.config.Agent.Description.NonIdentifyingAttributes, ad.NonIdentifyingAttributes)
	s.agentDescription.Store(ad)
}

// configuredAgentDescription is the agent description the Supervisor's configuration gives,
// without the collector's.
func (s *Supervisor) configuredAgentDescription() *protobufs.AgentDescription {
	return &protobufs.AgentDescription{
		IdentifyingAttributes:    applyKeyValueOverrides(s.config.Agent.Description.IdentifyingAttributes, nil),
		NonIdentifyingAttributes: applyKeyValueOverrides(s.config.Agent.Description.NonIdentifyingAttributes, nil),
	}
}

// setAvailableComponents sets the available components of the OpAMP agent
func (s *Supervisor) setAvailableComponents(ac *protobufs.AvailableComponents) {
	s.availableComponents.Store(ac)
}

// applyKeyValueOverrides merges the overrides map into the array of key value pairs.
// If a key from overrides already exists in the array of key value pairs, it is overwritten by the value from the overrides map.
// An array of KeyValue pair is returned, with each key value pair having a distinct key.
func applyKeyValueOverrides(overrides map[string]string, orig []*protobufs.KeyValue) []*protobufs.KeyValue {
	kvMap := make(map[string]*protobufs.KeyValue, len(orig)+len(overrides))

	for _, kv := range orig {
		kvMap[kv.Key] = kv
	}

	for k, v := range overrides {
		kvMap[k] = &protobufs.KeyValue{
			Key: k,
			Value: &protobufs.AnyValue{
				Value: &protobufs.AnyValue_StringValue{
					StringValue: v,
				},
			},
		}
	}

	// Sort keys for stable output, makes it easier to test.
	keys := make([]string, 0, len(kvMap))
	for k := range kvMap {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	kvOut := make([]*protobufs.KeyValue, 0, len(kvMap))
	for _, k := range keys {
		v := kvMap[k]
		kvOut = append(kvOut, v)
	}

	return kvOut
}

func (s *Supervisor) stopOpAMPClient() error {
	s.telemetrySettings.Logger.Debug("Stopping OpAMP client...")
	ctx, cancel := context.WithTimeout(s.runCtx, 5*time.Second)
	defer cancel()
	err := s.opampClient.Stop(ctx)
	// TODO(srikanthccv): remove context.DeadlineExceeded after https://github.com/open-telemetry/opamp-go/pull/213
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	s.telemetrySettings.Logger.Debug("OpAMP client stopped.")

	return nil
}

func (s *Supervisor) onConnect(ctx context.Context) {
	s.telemetrySettings.Logger.Info("Connected to the OpAMP server.")
	s.metrics.SetCollectorFallbackStatus(ctx, false)

	if s.initialOpampConnSuccess.Load() {
		return
	}
	s.initialOpampConnSuccess.Store(true)

	fallbackEnabled := s.config.Agent.FallbackEnabled()
	if !fallbackEnabled {
		return
	}
	s.configWriteMu.Lock()
	err := s.loadAndWriteInitialMergedConfig()
	s.configWriteMu.Unlock()
	if err != nil {
		s.telemetrySettings.Logger.Error("failed loading initial config", zap.Error(err))
		return
	}
	select {
	case s.hasNewConfig <- struct{}{}:
	default:
	}
}

func (s *Supervisor) addSpecialConfigFiles() []string {
	configFiles := slices.Clone(s.config.Agent.ConfigFiles)

	missingSpecialConfigFiles := make(map[config.SpecialConfigFile]struct{}, len(config.SpecialConfigFiles))
	for _, file := range config.SpecialConfigFiles {
		missingSpecialConfigFiles[file] = struct{}{}
	}
	for _, file := range configFiles {
		if strings.HasPrefix(file, "$") {
			delete(missingSpecialConfigFiles, config.SpecialConfigFile(file))
		}
	}

	// if missing builtin, add it to the beginning
	if _, ok := missingSpecialConfigFiles[config.SpecialConfigFileOwnTelemetry]; ok {
		configFiles = slices.Insert(configFiles, 0, string(config.SpecialConfigFileOwnTelemetry))
	}

	// if missing opamp extension, add it to the end
	if _, ok := missingSpecialConfigFiles[config.SpecialConfigFileOpAMPExtension]; ok {
		configFiles = append(configFiles, string(config.SpecialConfigFileOpAMPExtension))
	}

	// if missing remote config, add it to the end
	if _, ok := missingSpecialConfigFiles[config.SpecialConfigFileRemoteConfig]; ok {
		configFiles = append(configFiles, string(config.SpecialConfigFileRemoteConfig))
	}
	return configFiles
}

func (s *Supervisor) composeNoopPipeline() ([]byte, error) {
	var cfg bytes.Buffer

	if !s.isFeatureGateSupported(AllowNoPipelinesFeatureGate) {
		err := s.noopPipelineTemplate.Execute(&cfg, map[string]any{})
		if err != nil {
			return nil, err
		}
	}

	return cfg.Bytes(), nil
}

func (s *Supervisor) createRemoteConfigComposers(incomingConfig *protobufs.AgentRemoteConfig) []configComposer {
	if !s.config.Capabilities.AcceptsRemoteConfig {
		return []configComposer{}
	}

	hasIncomingConfigMap := len(incomingConfig.GetConfig().GetConfigMap()) != 0
	remoteConfigComposers := []configComposer{}
	if hasIncomingConfigMap {
		c := incomingConfig.GetConfig()

		// Sort to make sure the order of merging is stable.
		var names []string
		for name := range c.ConfigMap {
			if name == "" {
				// skip instance config
				continue
			}
			names = append(names, name)
		}

		sort.Strings(names)

		// Append instance config as the last item.
		names = append(names, "")

		// Merge received configs.
		for _, name := range names {
			item := c.ConfigMap[name]
			if item == nil {
				continue
			}
			if remoteConfigDeclaresResourceAttributes(item.Body) {
				remoteConfigComposers = append(remoteConfigComposers, composeClearResourceAttributesConfig)
			}
			remoteConfigComposers = append(remoteConfigComposers, func() []byte {
				return item.Body
			})
		}
		remoteConfigComposers = append(remoteConfigComposers, s.composeRequiredResourceAttributesConfig)
	}
	return remoteConfigComposers
}

func (s *Supervisor) composeNoopConfig() ([]byte, error) {
	cfg, err := s.composeNoopPipeline()
	if err != nil {
		return nil, err
	}
	conf, err := config.NewConfFromYAML(cfg)
	if err != nil {
		return nil, err
	}
	if err := config.MergeConfFromYAML(conf, s.composeOpAMPExtensionConfig()); err != nil {
		return nil, err
	}
	// The bootstrap Collector is stopped as soon as it has reported its
	// AgentDescription, so its internal metrics are never collected. Disabling
	// them keeps the Collector's default reader from binding localhost:8888,
	// which fails the bootstrap when that port is already in use.
	if err := config.MergeConfFromYAML(conf, []byte(noopTelemetry)); err != nil {
		return nil, err
	}

	return config.MarshalConfToYAML(conf)
}

func (s *Supervisor) composeOwnTelemetryConfig() []byte {
	ownMetricsCfg, ok := s.agentConfigOwnTelemetrySection.Load().(string)
	if !ok {
		return nil
	}
	return []byte(ownMetricsCfg)
}

func (s *Supervisor) composeExtraTelemetryConfig() []byte {
	var cfg bytes.Buffer
	resourceAttrs := map[string]string{}
	ad := s.agentDescription.Load().(*protobufs.AgentDescription)
	for _, attr := range ad.IdentifyingAttributes {
		resourceAttrs[attr.Key] = attr.Value.GetStringValue()
	}
	for _, attr := range ad.NonIdentifyingAttributes {
		resourceAttrs[attr.Key] = attr.Value.GetStringValue()
	}
	attrNames := make([]string, 0, len(resourceAttrs))
	for name := range resourceAttrs {
		attrNames = append(attrNames, name)
	}
	sort.Strings(attrNames)

	resourceAttributeValues := make([]resourceAttributeTemplateValue, 0, len(attrNames))
	for _, name := range attrNames {
		resourceAttributeValues = append(resourceAttributeValues, resourceAttributeTemplateValue{
			Name:  name,
			Value: resourceAttrs[name],
		})
	}
	tplVars := map[string]any{
		"ResourceAttributes": resourceAttributeValues,
		"SupervisorPort":     s.opampServerPort,
	}
	err := s.extraTelemetryConfigTemplate.Execute(
		&cfg,
		tplVars,
	)
	if err != nil {
		s.telemetrySettings.Logger.Error("Could not compose local config", zap.Error(err))
		return nil
	}

	return cfg.Bytes()
}

func (s *Supervisor) composeRequiredResourceAttributesConfig() []byte {
	cfg, err := gyaml.Marshal(map[string]any{
		"service": map[string]any{
			"telemetry": map[string]any{
				"resource": map[string]any{
					"attributes": []map[string]any{
						{
							"name":  "service.instance.id",
							"value": s.persistentState.InstanceID.String(),
						},
					},
				},
			},
		},
	})
	if err != nil {
		s.telemetrySettings.Logger.Error("Could not compose required resource attributes config", zap.Error(err))
		return nil
	}

	return cfg
}

func composeClearResourceAttributesConfig() []byte {
	return []byte("service:\n  telemetry:\n    resource:\n      " + config.ClearResourceAttributesKey + ": true\n")
}

func remoteConfigDeclaresResourceAttributes(body []byte) bool {
	conf, err := config.NewConfFromYAML(body)
	if err != nil {
		return false
	}
	return conf.IsSet("service::telemetry::resource::attributes")
}

func (s *Supervisor) composeOpAMPExtensionConfig() []byte {
	orphanPollInterval := 5 * time.Second
	if s.config.Agent.OrphanDetectionInterval > 0 {
		orphanPollInterval = s.config.Agent.OrphanDetectionInterval
	}

	var cfg bytes.Buffer
	tplVars := map[string]any{
		"InstanceUid":                s.persistentState.InstanceID.String(),
		"SupervisorPort":             s.opampServerPort,
		"PID":                        s.pidProvider.PID(),
		"PPIDPollInterval":           orphanPollInterval,
		"ReportsAvailableComponents": s.config.Capabilities.ReportsAvailableComponents,
		"IncludeResourceAttributes":  s.config.Agent.Description.IncludeResourceAttributes,
		"LocalServerToken":           s.localServerToken,
	}
	err := s.opampextensionTemplate.Execute(
		&cfg,
		tplVars,
	)
	if err != nil {
		s.telemetrySettings.Logger.Error("Could not compose local config", zap.Error(err))
		return nil
	}

	return cfg.Bytes()
}

type configComposer func() []byte

type resourceAttributeTemplateValue struct {
	Name  string
	Value string
}

func (s *Supervisor) composeAgentConfigFiles(incomingConfig *protobufs.AgentRemoteConfig, configFiles []string) ([]byte, error) {
	conf := confmap.New()

	specialConfigComposers := map[config.SpecialConfigFile][]configComposer{
		config.SpecialConfigFileOwnTelemetry:   {s.composeOwnTelemetryConfig, s.composeExtraTelemetryConfig},
		config.SpecialConfigFileOpAMPExtension: {s.composeOpAMPExtensionConfig},
		config.SpecialConfigFileRemoteConfig:   s.createRemoteConfigComposers(incomingConfig),
	}

	for _, file := range configFiles {
		// The special config files should always be valid yaml and most of them
		// will be due to the typing in the Supervisor's config struct and its
		// validation function.
		// The special config file for the remote configuration is the exception
		// here: it could be invalid yaml. In case it is, the confmap loader
		// will return an error.
		// Changed here: a normal config file that can't be read or merged fails the compose, as
		// the fallback configs do. It was left out: the collector ran without it (its exporter,
		// say), and the remote config was reported APPLIED.
		if strings.HasPrefix(file, "$") {
			cfgProviders := specialConfigComposers[config.SpecialConfigFile(file)]
			for _, cfgProvider := range cfgProviders {
				cfgBytes := cfgProvider()
				err := config.MergeConfFromYAML(conf, cfgBytes)
				if err != nil {
					s.telemetrySettings.Logger.Error("Could not merge special config file", zap.String("specialConfig", file), zap.Error(err))
					return nil, err
				}
			}
			continue
		}

		incomingConf, err := config.RetrieveURIAsConf(file, s.telemetrySettings.Logger)
		if err != nil {
			return nil, fmt.Errorf("could not read local config file %s: %w", file, err)
		}

		err = config.MergeConf(conf, incomingConf)
		if err != nil {
			return nil, fmt.Errorf("could not merge local config file %s: %w", file, err)
		}
	}

	if err := normalizeTelemetryResourceConfig(conf); err != nil {
		s.telemetrySettings.Logger.Error("Could not normalize telemetry resource config", zap.Error(err))
		return []byte(""), err
	}

	b, err := config.MarshalConfToYAML(conf)
	if err != nil {
		s.telemetrySettings.Logger.Error("Could not marshal merged local config files", zap.Error(err))
		return []byte(""), err
	}
	return b, nil
}

func normalizeTelemetryResourceConfig(conf *confmap.Conf) error {
	rawResourceCfg := conf.Get("service::telemetry::resource")
	if rawResourceCfg == nil {
		return nil
	}

	resourceMap, ok := rawResourceCfg.(map[string]any)
	if !ok {
		return nil
	}

	var resourceCfg otelconftelemetry.ResourceConfig
	if err := confmap.NewFromStringMap(resourceMap).Unmarshal(&resourceCfg); err != nil {
		return err
	}

	if len(resourceCfg.Attributes) == 0 || len(resourceCfg.LegacyAttributes) == 0 {
		return nil
	}

	seen := make(map[string]int, len(resourceCfg.Attributes))
	for idx, attr := range resourceCfg.Attributes {
		seen[attr.Name] = idx
	}

	legacyNames := make([]string, 0, len(resourceCfg.LegacyAttributes))
	for name, value := range resourceCfg.LegacyAttributes {
		if value == nil {
			continue
		}
		if idx, exists := seen[name]; exists {
			resourceCfg.Attributes[idx].Value = value
			continue
		}
		legacyNames = append(legacyNames, name)
	}
	sort.Strings(legacyNames)

	for _, name := range legacyNames {
		resourceCfg.Attributes = append(resourceCfg.Attributes, telemetryconfig.AttributeNameValue{
			Name:  name,
			Value: resourceCfg.LegacyAttributes[name],
		})
	}
	resourceCfg.LegacyAttributes = nil

	normalized := confmap.New()
	if err := normalized.Marshal(resourceCfg); err != nil {
		return err
	}

	conf.Delete("service::telemetry::resource")
	return conf.Merge(confmap.NewFromStringMap(map[string]any{
		"service": map[string]any{
			"telemetry": map[string]any{
				"resource": normalized.ToStringMap(),
			},
		},
	}))
}

// loadAndWriteInitialMergedConfig loads and writes the initial config by
// merging the last received remote config, last received own metrics config,
// and any local configs.
func (s *Supervisor) loadAndWriteInitialMergedConfig() error {
	if !s.initialOpampConnSuccess.Load() && !s.hasAgentConfigState() && s.config.Agent.FallbackEnabled() && !s.hasPersistedRemoteConfig() {
		err := s.loadAndWriteFallbackConfig()
		if err != nil {
			return fmt.Errorf("could not switch to fallback config: %w", err)
		}
		s.metrics.SetCollectorFallbackStatus(s.runCtx, true)
		return nil
	}

	// load the last received remote config
	s.loadRemoteConfig()

	// load the last received own telemetry config
	s.loadLastReceivedOwnTelemetryConfig()

	// compose the initial merged config
	_, err := s.composeMergedConfig(s.remoteConfig.Load())
	if err != nil {
		return fmt.Errorf("could not compose initial merged config: %w", err)
	}

	// write the initial merged config to disk
	if err := s.writeAgentConfig(); err != nil {
		return fmt.Errorf("could not write the initial merged config: %w", err)
	}

	return nil
}

// loadRemoteConfig loads the last received remote config from file if the capability is supported.
func (s *Supervisor) loadRemoteConfig() {
	if !s.config.Capabilities.AcceptsRemoteConfig {
		s.telemetrySettings.Logger.Debug("Remote config is not supported, will not attempt to load config from file")
		return
	}

	if s.config.Agent.AutomaticConfigRollback {
		lastWorkingRemoteConfig, err := s.loadPersistedRemoteConfig(lastWorkingRemoteConfigFile)
		switch {
		case err == nil && lastWorkingRemoteConfig != nil:
			s.lastWorkingRemoteConfig.Store(lastWorkingRemoteConfig)
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			s.telemetrySettings.Logger.Error("Cannot parse last working remote config", zap.Error(err))
		}
	}

	lastReceivedRemoteConfig, err := s.loadPersistedRemoteConfig(lastRecvRemoteConfigFile)
	switch {
	case err == nil && lastReceivedRemoteConfig != nil:
		startup := lastReceivedRemoteConfig
		if s.config.Agent.AutomaticConfigRollback {
			startup = s.remoteConfigForStartup(lastReceivedRemoteConfig)
		}
		// Changed here: one a tightened policy refuses isn't run; the server sends another.
		if err := s.checkRemoteConfigPolicy(startup); err != nil {
			s.telemetrySettings.Logger.Error("Not running the saved remote config", zap.Error(err))
			s.usingLastWorkingRemoteConfig.Store(false)
			break
		}
		s.remoteConfig.Store(startup)
	case errors.Is(err, os.ErrNotExist):
		s.telemetrySettings.Logger.Info("No last received remote config found")
	default:
		s.telemetrySettings.Logger.Error("error while reading last received config", zap.Error(err))
	}
}

// checkRemoteConfigPolicy refuses a remote config agent::remote_config_policy doesn't allow.
func (s *Supervisor) checkRemoteConfigPolicy(rc *protobufs.AgentRemoteConfig) error {
	files := rc.GetConfig().GetConfigMap()
	for _, name := range slices.Sorted(maps.Keys(files)) {
		if err := s.config.Agent.RemoteConfigPolicy.Check(name, files[name].GetBody()); err != nil {
			return err
		}
	}
	return nil
}

func (s *Supervisor) loadPersistedRemoteConfig(fileName string) (*protobufs.AgentRemoteConfig, error) {
	cfg, err := s.readStorageFile(fileName)
	if err != nil {
		return nil, err
	}

	config := &protobufs.AgentRemoteConfig{}
	if err := proto.Unmarshal(cfg, config); err != nil {
		return nil, fmt.Errorf("parse %s: %w", fileName, err)
	}

	return config, nil
}

func (s *Supervisor) remoteConfigForStartup(lastReceivedRemoteConfig *protobufs.AgentRemoteConfig) *protobufs.AgentRemoteConfig {
	if lastReceivedRemoteConfig == nil {
		return nil
	}

	lastReceivedStatus := s.persistentState.GetLastRemoteConfigStatus()
	if lastReceivedStatus == nil || lastReceivedStatus.Status != protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED {
		return lastReceivedRemoteConfig
	}
	if !bytes.Equal(lastReceivedStatus.LastRemoteConfigHash, lastReceivedRemoteConfig.GetConfigHash()) {
		return lastReceivedRemoteConfig
	}

	lastWorkingRemoteConfig := s.lastWorkingRemoteConfig.Load()
	if lastWorkingRemoteConfig == nil {
		return lastReceivedRemoteConfig
	}

	s.usingLastWorkingRemoteConfig.Store(true)
	return lastWorkingRemoteConfig
}

// restoreLastWorkingRemoteConfig attempts to restore the last working remote
// config if is available. In case the restoration works, it signals to the
// Supervisor that there's new configuration.
func (s *Supervisor) restoreLastWorkingRemoteConfig() bool {
	restored, err := s.composeWithLastWorkingRemoteConfig()
	if err != nil {
		s.telemetrySettings.Logger.Error("Could not restore last working remote config", zap.Error(err))
		return false
	}
	if restored {
		select {
		case s.hasNewConfig <- struct{}{}:
		default:
		}
	}
	return restored
}

// composeWithLastWorkingRemoteConfig composes the Collector configuration
// with the last working remote config if it is available and automatic
// rollback is enabled.
func (s *Supervisor) composeWithLastWorkingRemoteConfig() (bool, error) {
	if !s.config.Agent.AutomaticConfigRollback {
		return false, nil
	}

	// If the last working remote config is already applied and failing, restoring
	// it again would restart the agent in a tight loop, bypassing the restart
	// backoff.
	if s.usingLastWorkingRemoteConfig.Load() {
		return false, nil
	}

	lastWorkingRemoteConfig := s.lastWorkingRemoteConfig.Load()
	if lastWorkingRemoteConfig == nil {
		return false, nil
	}

	clonedConfig := proto.Clone(lastWorkingRemoteConfig).(*protobufs.AgentRemoteConfig)
	previous := s.cfgState.Load()
	if _, err := s.composeMergedConfig(clonedConfig); err != nil {
		return false, fmt.Errorf("compose last working remote config: %w", err)
	}

	if err := s.writeAgentConfig(); err != nil {
		// The Collector still has the config it was given before: keep the state saying so.
		if previous != nil {
			s.cfgState.Store(previous)
		}
		return false, fmt.Errorf("write last working remote config: %w", err)
	}

	s.remoteConfig.Store(clonedConfig)
	s.usingLastWorkingRemoteConfig.Store(true)

	return true, nil
}

func (s *Supervisor) hasPersistedRemoteConfig() bool {
	if !s.config.Capabilities.AcceptsRemoteConfig {
		return false
	}
	_, err := os.Stat(filepath.Join(s.config.Storage.Directory, lastRecvRemoteConfigFile))
	return err == nil
}

// loadLastReceivedOwnTelemetryConfig loads the last received own telemetry config from file if the capability is supported.
func (s *Supervisor) loadLastReceivedOwnTelemetryConfig() {
	// If none of the own telemetry capabilities are supported, do nothing.
	if !s.config.Capabilities.ReportsOwnMetrics && !s.config.Capabilities.ReportsOwnTraces && !s.config.Capabilities.ReportsOwnLogs {
		s.telemetrySettings.Logger.Debug("Own telemetry is not supported, will not attempt to load config from file")
		return
	}

	// Try to load the last received own metrics config if it exists.
	var lastRecvOwnTelemetryConfig []byte
	var err error
	lastRecvOwnTelemetryConfig, err = s.readStorageFile(lastRecvOwnTelemetryConfigFile)
	if err == nil {
		set := &protobufs.ConnectionSettingsOffers{}
		err = proto.Unmarshal(lastRecvOwnTelemetryConfig, set)
		if err != nil {
			s.telemetrySettings.Logger.Error("Cannot parse last received own telemetry config", zap.Error(err))
		} else {
			_, _ = s.setupOwnTelemetry(s.runCtx, set)
		}
	}
}

// createEffectiveConfigMsg create an EffectiveConfig with the content of the
// current effective config.
// Changed here: without the local server's token. The effective config is what the collector
// reports it loaded, which it redacts; until it reports, the Supervisor has none to send. (The
// merged config it wrote is not effective yet, and holds every secret in it.) A server's empty
// config map runs nothing, so it is reported as an empty one.
func (s *Supervisor) createEffectiveConfigMsg() *protobufs.EffectiveConfig {
	if cfgState, ok := s.cfgState.Load().(*configState); ok && cfgState.configMapIsEmpty {
		return &protobufs.EffectiveConfig{ConfigMap: &protobufs.AgentConfigMap{}}
	}
	if cfg, ok := s.effectiveConfig.Load().(*protobufs.EffectiveConfig); ok && cfg != nil {
		return s.redactLocalServerTokenInConfig(cfg)
	}
	return nil
}

func (*Supervisor) updateOwnTelemetryData(data map[string]any, signal string, settings *protobufs.TelemetryConnectionSettings) map[string]any {
	if settings == nil || settings.DestinationEndpoint == "" {
		return data
	}
	data[fmt.Sprintf("%sEndpoint", signal)] = settings.DestinationEndpoint
	data[fmt.Sprintf("%sHeaders", signal)] = []protobufs.Header{}

	if settings.Headers != nil {
		data[fmt.Sprintf("%sHeaders", signal)] = settings.Headers.Headers
	}
	return data
}

// setupOwnTelemetry composes the collector's config with the own telemetry settings. Changed
// here: it says when it can't, and then keeps the settings it had.
func (s *Supervisor) setupOwnTelemetry(_ context.Context, settings *protobufs.ConnectionSettingsOffers) (configChanged bool, err error) {
	var cfg bytes.Buffer

	data := s.updateOwnTelemetryData(map[string]any{}, "Metrics", settings.GetOwnMetrics())
	data = s.updateOwnTelemetryData(data, "Logs", settings.GetOwnLogs())
	data = s.updateOwnTelemetryData(data, "Traces", settings.GetOwnTraces())

	if len(data) == 0 {
		s.telemetrySettings.Logger.Debug("Disabling own telemetry pipeline in the config")
	} else {
		err := s.ownTelemetryTemplate.Execute(&cfg, data)
		if err != nil {
			s.telemetrySettings.Logger.Error("Could not setup own telemetry", zap.Error(err))
			return false, err
		}
	}
	// Changed here: the server's own telemetry destinations are held to the remote config
	// policy too. They went around it.
	if err := s.config.Agent.RemoteConfigPolicy.Check(string(config.SpecialConfigFileOwnTelemetry), cfg.Bytes()); err != nil {
		s.telemetrySettings.Logger.Error("Refusing the own telemetry settings", zap.Error(err))
		return false, err
	}
	previous := s.agentConfigOwnTelemetrySection.Load()
	s.agentConfigOwnTelemetrySection.Store(cfg.String())

	// Need to recalculate the Agent config so that the metric config is included in it.
	configChanged, err = s.composeMergedConfig(s.remoteConfig.Load())
	if err != nil {
		s.telemetrySettings.Logger.Error("Error composing merged config for own metrics. Ignoring agent self metrics config", zap.Error(err))
		if previous != nil {
			s.agentConfigOwnTelemetrySection.Store(previous)
		}
		return false, err
	}

	return configChanged, nil
}

// yamlString is v as a YAML double-quoted string that the collector's config resolver reads as
// v: a JSON string (one YAML reads), with each $ doubled ($$ is a literal $ there).
func yamlString(v string) string {
	b, _ := json.Marshal(strings.ReplaceAll(v, "$", "$$"))
	return string(b)
}

// unsupportedOwnTelemetry says which offered own telemetry settings the Supervisor can't apply:
// it gives the collector an endpoint and headers only. Changed here: they were left out.
func unsupportedOwnTelemetry(offers *protobufs.ConnectionSettingsOffers) error {
	for _, own := range []struct {
		name string
		set  *protobufs.TelemetryConnectionSettings
	}{{"own_metrics", offers.GetOwnMetrics()}, {"own_traces", offers.GetOwnTraces()}, {"own_logs", offers.GetOwnLogs()}} {
		var unsupported []string
		if own.set.GetTls() != nil {
			unsupported = append(unsupported, "tls")
		}
		if own.set.GetCertificate() != nil {
			unsupported = append(unsupported, "certificate")
		}
		if own.set.GetProxy() != nil {
			unsupported = append(unsupported, "proxy")
		}
		if len(unsupported) > 0 {
			return connectionSettingsError(fmt.Sprintf("The Supervisor can't apply %s's %s settings.", own.name, strings.Join(unsupported, " and ")))
		}
	}
	return nil
}

// composeMergedConfig composes the merged config from multiple sources:
// 1) the remote config from OpAMP Server
// 2) the own metrics config section
// 3) the local override config that is hard-coded in the Supervisor.
func (s *Supervisor) composeMergedConfig(incomingConfig *protobufs.AgentRemoteConfig) (configChanged bool, err error) {
	// Changed here: a remote config the policy refuses never reaches the collector.
	if err := s.checkRemoteConfigPolicy(incomingConfig); err != nil {
		return false, err
	}
	conf := confmap.New()

	configFiles := s.addSpecialConfigFiles()

	hasIncomingConfigMap := len(incomingConfig.GetConfig().GetConfigMap()) != 0
	if s.config.Capabilities.AcceptsRemoteConfig && !hasIncomingConfigMap {
		// Add noop pipeline
		var noopConfig []byte
		noopConfig, err = s.composeNoopPipeline()
		if err != nil {
			return false, fmt.Errorf("could not compose noop pipeline: %w", err)
		}

		if err = config.MergeConfFromYAML(conf, noopConfig); err != nil {
			return false, fmt.Errorf("could not merge noop pipeline: %w", err)
		}
	}

	agentConfigBytes, err := s.composeAgentConfigFiles(incomingConfig, configFiles)
	if err != nil {
		return false, err
	}

	err = config.MergeConfFromYAML(conf, agentConfigBytes)
	if err != nil {
		return false, err
	}

	// The merged final result is our new merged config.
	newMergedConfigBytes, err := config.MarshalConfToYAML(conf)
	if err != nil {
		return false, err
	}

	// Check if supervisor's merged config is changed.
	newConfigState := &configState{
		mergedConfig:     string(newMergedConfigBytes),
		configMapIsEmpty: (incomingConfig != nil && !hasIncomingConfigMap),
		remote:           incomingConfig,
	}

	configChanged = false

	oldConfigStateInterface := s.cfgState.Load()
	var oldConfigState *configState
	if oldConfigStateInterface != nil {
		oldConfigState = oldConfigStateInterface.(*configState)
	}

	if oldConfigState == nil || !oldConfigState.equal(newConfigState) {
		s.telemetrySettings.Logger.Debug("Merged config changed.")

		// Validate BEFORE storing to prevent race condition where other goroutines read invalid config
		if s.config.Agent.ValidateConfig && !newConfigState.configMapIsEmpty {
			if err := s.validateConfig(newConfigState.mergedConfig); err != nil {
				s.telemetrySettings.Logger.Warn(
					"New configuration failed validation, keeping previous config. "+
						"Note: Validation may fail for valid configs if resources (e.g., ports) are temporarily unavailable.",
					zap.Error(err),
				)
				return false, fmt.Errorf("configuration validation failed: %w", err)
			}
		}

		configChanged = true
	}
	// Only stored after successful validation (or if validation is disabled/skipped). Changed
	// here: unchanged too, so that it names the remote config it now comes from.
	s.cfgState.Store(newConfigState)

	return configChanged, nil
}

// composeFallbackConfig composes the agent configuration using the fallback config files
// (merged in order) instead of the remote config. This is used when the OpAMP server is unreachable.
func (s *Supervisor) composeFallbackConfig() (configChanged bool, err error) {
	if len(s.config.Agent.StartupFallbackConfigs) == 0 {
		return false, errors.New("no fallback configs configured")
	}

	conf := confmap.New()
	// Load and merge fallback config files in order. Unlike regular config files,
	// fallback config load failures are fatal because there is no alternate source.
	for _, fallbackPath := range s.config.Agent.StartupFallbackConfigs {
		fallbackConf, readErr := config.RetrieveURIAsConf(fallbackPath, s.telemetrySettings.Logger)
		if readErr != nil {
			if errors.Is(readErr, config.ErrConfigFileNotFound) {
				return false, fmt.Errorf("could not read fallback config file: %w", readErr)
			}
			return false, fmt.Errorf("could not load fallback config: %w", readErr)
		}

		if err = config.MergeConf(conf, fallbackConf); err != nil {
			return false, fmt.Errorf("could not load fallback config: %w", err)
		}
	}

	if extraCfgErr := s.loadFallbackExtraConfigs(conf); extraCfgErr != nil {
		return false, extraCfgErr
	}

	newMergedConfigBytes, err := config.MarshalConfToYAML(conf)
	if err != nil {
		return false, err
	}

	newConfigState := &configState{
		mergedConfig:     string(newMergedConfigBytes),
		configMapIsEmpty: false, // Fallback config is never empty
	}

	configChanged = false

	oldConfigState := s.cfgState.Swap(newConfigState)
	if oldConfigState == nil || !oldConfigState.(*configState).equal(newConfigState) {
		s.telemetrySettings.Logger.Debug("Merged config changed (using fallback).")
		configChanged = true
	}

	return configChanged, nil
}

func (s *Supervisor) loadFallbackExtraConfigs(conf *confmap.Conf) error {
	if err := config.MergeConfFromYAML(conf, s.composeOpAMPExtensionConfig()); err != nil {
		return fmt.Errorf("could not merge opamp extension config: %w", err)
	}

	if ownTelemetryConfig := s.composeOwnTelemetryConfig(); ownTelemetryConfig != nil {
		if err := config.MergeConfFromYAML(conf, ownTelemetryConfig); err != nil {
			return fmt.Errorf("could not merge own telemetry config: %w", err)
		}
	}

	if extraTelemetryConfig := s.composeExtraTelemetryConfig(); extraTelemetryConfig != nil {
		if err := config.MergeConfFromYAML(conf, extraTelemetryConfig); err != nil {
			return fmt.Errorf("could not merge extra telemetry config: %w", err)
		}
	}

	return nil
}

// loadAndWriteFallbackConfig switches the agent to use the fallback configuration.
func (s *Supervisor) loadAndWriteFallbackConfig() error {
	if s.initialOpampConnSuccess.Load() {
		s.telemetrySettings.Logger.Debug("Already connected to OpAMP backend, skipping switch to fallback config")
		return nil
	}

	s.telemetrySettings.Logger.Info("Switching to fallback configuration")

	// Compose the fallback config
	_, err := s.composeFallbackConfig()
	if err != nil {
		s.telemetrySettings.Logger.Error("Failed to compose fallback config", zap.Error(err))
		return err
	}
	if err := s.writeAgentConfig(); err != nil {
		s.telemetrySettings.Logger.Error("Failed to write fallback config", zap.Error(err))
		return err
	}

	return nil
}

// handleRestartCommand takes the server's Restart command to the agent loop. Changed here: it
// restarted the collector on the client's goroutine, outside the agent loop: before Start had
// made the commander (a Restart in the server's first reply) it panicked, the client's
// heartbeats and the server's messages waited for the collector's stop, and it started the
// collector even when its config is deliberately empty.
func (s *Supervisor) handleRestartCommand() error {
	s.telemetrySettings.Logger.Debug("Received restart command")
	select {
	case s.restartRequests <- struct{}{}:
	default:
		// One is pending already: it restarts the collector after this command too.
	}
	return nil
}

// restartAgent restarts the collector, on the agent loop. With no config to run (the no-op
// config) the collector stays stopped.
func (s *Supervisor) restartAgent() error {
	s.telemetrySettings.Logger.Debug("Restarting the agent process")
	defer s.resetAgentReady()

	if err := s.commander.Stop(s.runCtx); err != nil {
		s.telemetrySettings.Logger.Error("Could not restart agent process", zap.Error(err))
		return err
	}
	s.resetPassthroughLogBuffer()
	if s.cfgState.Load().(*configState).configMapIsEmpty {
		s.telemetrySettings.Logger.Info("No config present, not starting agent.")
		// The stop is not an exit the loop restarts the collector after.
		select {
		case <-s.commander.Exited():
		default:
		}
		return nil
	}
	if _, err := s.startAgent(); err != nil {
		s.telemetrySettings.Logger.Error("Could not restart agent process", zap.Error(err))
		return err
	}
	return nil
}

func (s *Supervisor) startAgent() (agentStartStatus, error) {
	if s.cfgState.Load().(*configState).configMapIsEmpty {
		// Don't start the agent if there is no config to run
		s.telemetrySettings.Logger.Info("No config present, not starting agent.")
		// need to manually trigger updating effective config
		err := s.opampClient.UpdateEffectiveConfig(s.runCtx)
		if err != nil {
			s.telemetrySettings.Logger.Error("The OpAMP client failed to update the effective config", zap.Error(err))
		}
		return agentNotStarting, nil
	}

	s.resetPassthroughLogBuffer()
	err := s.commander.Start(s.runCtx)
	if err != nil {
		s.telemetrySettings.Logger.Error("Cannot start the agent", zap.Error(err))
		startErr := fmt.Errorf("cannot start the agent: %w", err)
		if err := s.SetHealth(&protobufs.ComponentHealth{Healthy: false, LastError: startErr.Error()}); err != nil {
			s.telemetrySettings.Logger.Error("Could not report health to OpAMP server", zap.Error(err))
		}
		return "", startErr
	}

	return agentStarting, nil
}

// configApply is a remote config given to the collector, and how that went. Changed here:
// every outcome the agent loop reports is about the config the collector was given, never one
// that came after it.
type configApply struct {
	config *protobufs.AgentRemoteConfig
	// rollback: it is the last working config, restored after a newer one failed. The server
	// was told how that one failed, and hears nothing about this one.
	rollback bool
	// since is the count of collector connections before the collector was given it: only
	// health from a later connection is about it.
	since uint64
	// status is how the apply went: APPLYING while the apply timer runs; UNSET if no outcome
	// is known (the config the Supervisor started with).
	status  protobufs.RemoteConfigStatuses
	message string
	// retry: the apply window a restart after an exit reopened. It rolls nothing back.
	retry bool
	// connection is the hash of the connection settings the apply's outcome answers, if any.
	connection *[]byte
}

// newConfigApply is the apply of the config in s.cfgState, which the collector is about to be
// given.
func (s *Supervisor) newConfigApply() *configApply {
	remote := s.cfgState.Load().(*configState).remote
	lastWorking := s.lastWorkingRemoteConfig.Load()
	return &configApply{
		config: remote,
		rollback: s.usingLastWorkingRemoteConfig.Load() && remote != nil && lastWorking != nil &&
			bytes.Equal(remote.GetConfigHash(), lastWorking.GetConfigHash()),
		since: s.agentConn.count(),
	}
}

// reportApply records the apply's outcome and reports it about the apply's config. A FAILED
// one restores the last working config if restore is set, and says so; it returns whether it
// did.
func (s *Supervisor) reportApply(a *configApply, status protobufs.RemoteConfigStatuses, message string, restore bool) bool {
	a.status, a.message = status, message
	if a.connection != nil {
		switch status {
		case protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED:
			s.reportConnectionSettings(*a.connection, nil)
			a.connection = nil
		case protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED:
			s.reportConnectionSettings(*a.connection, errSettingsNotRunning)
			a.connection = nil
		}
	}
	if !s.config.Capabilities.AcceptsRemoteConfig || a.config == nil {
		return false
	}
	if a.rollback {
		s.telemetrySettings.Logger.Info("The restored last working remote config: "+status.String(), zap.String("error", message))
		return false
	}
	restored := false
	switch status {
	case protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED:
		if restore && s.restoreLastWorkingRemoteConfig() {
			restored = true
			message += " The last working remote config runs in its place."
		}
	case protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED:
		if s.config.Agent.AutomaticConfigRollback {
			if err := s.saveLastWorkingRemoteConfig(a.config); err != nil {
				s.telemetrySettings.Logger.Error("Could not save last working remote config", zap.Error(err))
			}
		}
	}
	s.reportConfigStatus(a.config.GetConfigHash(), status, message)
	return restored
}

// healthySince says whether the collector reported healthy on a connection made after the
// apply began.
func (s *Supervisor) healthySince(a *configApply) bool {
	h := s.lastHealthFromClient.Load()
	return h != nil && h.Healthy && h.connection > a.since
}

func (s *Supervisor) runAgentProcess() {
	apply := s.newConfigApply()
	apply.since = 0
	if st := s.persistentState.GetLastRemoteConfigStatus(); st != nil && apply.config != nil &&
		bytes.Equal(st.GetLastRemoteConfigHash(), apply.config.GetConfigHash()) {
		apply.status, apply.message = st.GetStatus(), st.GetErrorMessage()
	}
	if _, err := os.Stat(s.agentConfigFilePath()); err == nil {
		// We have an effective config file saved previously. Use it to start the agent.
		s.telemetrySettings.Logger.Debug("Effective config found, starting agent initial time")
		_, err := s.startAgent()
		if err != nil {
			s.telemetrySettings.Logger.Error("starting agent failed", zap.Error(err))
			s.reportApply(apply, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, err.Error(), false)
		}
	}

	restartTimer := time.NewTimer(0)
	restartTimer.Stop()

	configApplyTimeoutTimer := time.NewTimer(0)
	configApplyTimeoutTimer.Stop()
	// stopApplyTimer stops the apply timer, and says whether it was running.
	stopApplyTimer := func() bool {
		if configApplyTimeoutTimer.Stop() {
			return true
		}
		select {
		case <-configApplyTimeoutTimer.C:
			return true
		default:
			return false
		}
	}
	// openApplyWindow (re)starts the wait for the collector's health on the apply.
	openApplyWindow := func() {
		stopApplyTimer()
		apply.status, apply.message = protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLYING, ""
		configApplyTimeoutTimer.Reset(s.config.Agent.ConfigApplyTimeout)
	}
	resetRestartTimer := func() {
		if !restartTimer.Stop() {
			select {
			case <-restartTimer.C:
			default:
			}
		}
		restartTimer.Reset(5 * time.Second)
	}

	// The credentials the running Collector was last restarted with for a connection update.
	var startedEnv map[string]string

	for {
		select {
		case update := <-s.connectionUpdates:
			if startedEnv != nil && maps.Equal(startedEnv, update.env) && s.commander.IsRunning() {
				// An offer that changed only other settings, or repeated these credentials: the
				// running Collector already has them.
				s.reportConnectionSettings(update.hash, nil)
				continue
			}
			startedEnv = nil
			applying := stopApplyTimer()
			s.stopAgentApplyConfig()
			since := s.agentConn.count()
			_, err := s.startAgent()
			if applying {
				// A credential restart gives the current config a full application window,
				// on the new collector's health.
				apply.since = since
				configApplyTimeoutTimer.Reset(s.config.Agent.ConfigApplyTimeout)
			}
			if err != nil {
				restartTimer.Reset(5 * time.Second)
			} else {
				startedEnv = update.env
			}
			s.reportConnectionSettings(update.hash, err)
		case req := <-s.agentInstalls:
			// Changed here: the update stops and starts the collector itself. A restart still
			// pending stays so: after an update it finds the collector running.
			if req.remove {
				req.done(s.removeAgentPackage())
			} else if err := s.installAgentPackage(req); err != nil {
				s.telemetrySettings.Logger.Error("Could not install the collector the OpAMP server offered", zap.Error(err))
				req.done(err)
			} else {
				req.done(nil)
			}
		case <-s.configRetargets:
			// A config with the body the collector runs. A new config still to be given to
			// the collector takes it.
			if len(s.hasNewConfig) > 0 {
				continue
			}
			remote := s.cfgState.Load().(*configState).remote
			if remote == nil || (apply.config != nil && bytes.Equal(remote.GetConfigHash(), apply.config.GetConfigHash())) {
				continue
			}
			apply.config, apply.rollback = remote, false
			switch apply.status {
			case protobufs.RemoteConfigStatuses_RemoteConfigStatuses_UNSET:
				// Nothing is known about the body the collector runs: its health decides.
				openApplyWindow()
				s.reportApply(apply, apply.status, "", false)
			default:
				s.reportApply(apply, apply.status, apply.message, false)
			}
		case <-s.hasNewConfig:
			owed := apply.connection
			apply = s.newConfigApply()
			apply.connection = owed
			if pending := s.pendingConnectionSettings.Swap(nil); pending != nil {
				apply.connection = pending
			}
			s.lastHealthFromClient.Store(nil)
			s.telemetrySettings.Logger.Debug("agent has new config")
			openApplyWindow()
			restartTimer.Stop()

			if s.config.Agent.UseHUPConfigReload {
				if hupErr := s.hupReloadAgent(); hupErr != nil {
					s.telemetrySettings.Logger.Error("Failed to HUP restart agent", zap.Error(hupErr))
					stopApplyTimer()
					s.reportApply(apply, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, hupErr.Error(), true)
					if healthErr := s.SetHealth(&protobufs.ComponentHealth{Healthy: false, LastError: hupErr.Error()}); healthErr != nil {
						s.telemetrySettings.Logger.Error("Could not report health to OpAMP server", zap.Error(healthErr))
					}
					continue
				}
			} else {
				s.stopAgentApplyConfig()
			}

			s.telemetrySettings.Logger.Debug("Agent is not running, starting new instance")
			// This call to [startAgent] is useful for both the normal config reload
			// and the HUP one. It takes care of not starting the agent if the config
			// is empty and also of starting it when the config changes from empty
			// to non-empty.
			status, err := s.startAgent()
			if err != nil {
				s.telemetrySettings.Logger.Error("starting agent with new config failed", zap.Error(err))
				stopApplyTimer()
				if s.reportApply(apply, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, err.Error(), true) {
					continue
				}
				// The agent process never started, so no exit event will trigger a
				// restart; arm the restart backoff explicitly.
				restartTimer.Reset(5 * time.Second)
			}
			if status == agentNotStarting {
				// not starting agent because of nop config: clear timer, report applied status, report healthy status
				s.telemetrySettings.Logger.Debug("No config present, nothing to apply")
				stopApplyTimer()
				s.reportApply(apply, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED, "", false)
				if err := s.SetHealth(&protobufs.ComponentHealth{Healthy: true, LastError: ""}); err != nil {
					s.telemetrySettings.Logger.Error("Could not report healthy status to OpAMP server", zap.Error(err))
				}
				// need to clear exit channel to avoid triggering `s.commander.Exited()` case
				// because we stopped the agent and aren't restarting it this case will trigger
				// and report an unhealthy status (collector would be healthy, just choosing to not run)
				if len(s.commander.Exited()) > 0 {
					select {
					case <-s.commander.Exited():
					default:
					}
				}
			}
		case <-s.restartRequests:
			if err := s.restartAgent(); err != nil {
				// Started again on the restart backoff, as after an exit.
				resetRestartTimer()
			}
		case <-s.commander.Exited():
			// the agent process exit is expected for a HUP reload and will not attempt to restart
			if s.agentRestarting.Load() {
				s.telemetrySettings.Logger.Debug("Agent restarted")
				continue
			}

			errMsg := fmt.Sprintf(
				"Agent process PID=%d exited unexpectedly, exit code=%d. Will restart in a bit...",
				s.commander.Pid(), s.commander.ExitCode(),
			)
			logFields := []zap.Field{
				zap.Int("pid", s.commander.Pid()),
				zap.Int("exit_code", s.commander.ExitCode()),
			}
			if snippet := s.collectorCrashLogSnippet(); snippet != "" {
				errMsg = fmt.Sprintf("%s\nCollector log tail:\n%s", errMsg, snippet)
				logFields = append(logFields, zap.String("collector_log_tail", snippet))
			}
			s.telemetrySettings.Logger.Debug("Agent process exited unexpectedly. Will restart in a bit...", logFields...)
			if err := s.SetHealth(&protobufs.ComponentHealth{Healthy: false, LastError: errMsg}); err != nil {
				s.telemetrySettings.Logger.Error("Could not report health to OpAMP server", zap.Error(err))
			}

			switch {
			case stopApplyTimer():
				// The collector exited while applying the config: FAILED now, rather than at
				// the timeout. When the restored last working remote config crashes as well,
				// skip restoring it again and retry it on the regular restart backoff.
				s.telemetrySettings.Logger.Info("Agent crashed during config application, reporting FAILED status")
				failureMsg := fmt.Sprintf("Agent exited unexpectedly with exit code %d while applying configuration", s.commander.ExitCode())
				if s.reportApply(apply, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, s.appendCollectorCrashDetails(failureMsg), !apply.retry) {
					continue
				}
			case apply.status == protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED:
				// Changed here: an APPLIED config the collector then exits on is FAILED. It
				// stayed APPLIED while the collector crash-looped.
				failureMsg := fmt.Sprintf("Agent exited unexpectedly with exit code %d after applying configuration", s.commander.ExitCode())
				s.reportApply(apply, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, s.appendCollectorCrashDetails(failureMsg), false)
			}

			// Wait 5 seconds before starting again.
			resetRestartTimer()

		case <-restartTimer.C:
			s.telemetrySettings.Logger.Debug("Agent starting after start backoff")
			since := s.agentConn.count()
			status, err := s.startAgent()
			switch {
			case err != nil:
				s.telemetrySettings.Logger.Error("restarting agent failed", zap.Error(err))
				s.reportApply(apply, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, err.Error(), false)
			case status == agentStarting && apply.status == protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED:
				// Changed here: the config the collector failed on gets another apply window
				// with this start, and is APPLIED if it is healthy at its end.
				openApplyWindow()
				apply.since, apply.retry = since, true
			}

		case <-configApplyTimeoutTimer.C:
			// Changed here: only health the collector reported since it was given the config
			// counts. The old collector's, sent before it stopped, made a hung one APPLIED.
			if !s.healthySince(apply) {
				s.reportApply(apply, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, "Config apply timeout exceeded", !apply.retry)
				continue
			}
			s.reportApply(apply, protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLIED, "", false)

		case <-s.doneChan:
			err := s.commander.Stop(s.runCtx)
			if err != nil {
				s.telemetrySettings.Logger.Error("Could not stop agent process", zap.Error(err))
			}
			return
		}
	}
}

// markAgentReady marks the agent as ready and sends a signal to
// [agentReadyChan].
func (s *Supervisor) markAgentReady() {
	s.agentReady.Store(true)
	select {
	case s.agentReadyChan <- struct{}{}:
	default:
	}
}

// resetAgentReady resets the agent as not ready and drains [agentReadyChan].
func (s *Supervisor) resetAgentReady() {
	s.agentReady.Store(false)
	select {
	case <-s.agentReadyChan:
	default:
	}
}

// waitForAgentReady waits for the agent to be ready. The agent is considered to
// be ready when its first health report is received by the Supervisor's opamp
// server.
// WARNING: this is not thread-safe! If there are two goroutines waiting for
// the agent to be ready, only one of them will be able to proceed.
func (s *Supervisor) waitForAgentReady() error {
	if s.agentReady.Load() {
		return nil
	}

	bootstrapTimer := time.NewTimer(s.config.Agent.BootstrapTimeout)
	defer bootstrapTimer.Stop()

	select {
	case <-s.agentReadyChan:
		return nil
	case <-bootstrapTimer.C:
		return fmt.Errorf("agent has not started after %s", s.config.Agent.BootstrapTimeout)
	}
}

// hupReloadAgent sends a HUP signal to the agent process  with the intent of
// triggering a configuration reload. There are 3 possible outcomes of this:
// 1. The agent is the "official" Otel Collector, which properly traps the HUP
// signal and reloads the config.
// 2. The agent is a custom process, which does not trap the HUP signal: in
// this case the default behavior of a process that receives a HUP signal is to
// exit. This ends up being the same behavior as the standard stop -> restart
// configuration reload method.
// 3. The agent traps the HUP signal, but does nothing. On the next health
// report the agent will be consiered healthy and ready, even though it might
// be running on old configuration.
func (s *Supervisor) hupReloadAgent() error {
	s.agentRestarting.Store(true)
	defer s.agentRestarting.Store(false)
	defer s.resetAgentReady()

	// If we have an empty config, the agent should be stopped.
	if s.cfgState.Load().(*configState).configMapIsEmpty {
		s.stopAgentApplyConfig()
		return nil
	}

	if err := s.writeAgentConfig(); err != nil {
		return fmt.Errorf("failed to write agent new config: %w", err)
	}

	// If the agent is not running, we can't send a HUP signal to it, so we
	// return and let it be started by the caller.
	if !s.commander.IsRunning() {
		return nil
	}

	if err := s.waitForAgentReady(); err != nil {
		return err
	}

	s.telemetrySettings.Logger.Debug("agent is running, reloading config")
	if err := s.reloadAgentConfig(); err != nil {
		return err
	}

	return nil
}

func (s *Supervisor) reloadAgentConfig() error {
	s.telemetrySettings.Logger.Debug("Reloading the agent config")
	err := s.commander.ReloadConfigFile()
	if err != nil {
		return err
	}
	return nil
}

func (s *Supervisor) writeAgentConfig() error {
	cfgState := s.cfgState.Load().(*configState)
	// Changed here: written and renamed into place, so a collector starting meanwhile reads the
	// old config or the new one, never a truncated file it exits on.
	path := s.agentConfigFilePath()
	tmp, err := os.CreateTemp(filepath.Dir(path), agentConfigFileName+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(cfgState.mergedConfig); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return renameOver(tmp.Name(), path)
}

// renameOver renames from over to. Windows replaces the file in place (MoveFileEx), but refuses
// while another process has it open, as a collector reading its config briefly does: that is
// tried again for a little while.
func renameOver(from, to string) error {
	err := os.Rename(from, to)
	for i := 0; err != nil && runtime.GOOS == "windows" && i < 50; i++ {
		time.Sleep(20 * time.Millisecond)
		err = os.Rename(from, to)
	}
	return err
}

func (s *Supervisor) hasAgentConfigState() bool {
	cfgState, ok := s.cfgState.Load().(*configState)
	if !ok {
		return false
	}
	return cfgState != nil && cfgState.mergedConfig != ""
}

// validateConfig validates a configuration string without storing it in cfgState.
// This prevents race conditions where other goroutines might read invalid config during validation.
// Returns an error if validation fails.
func (s *Supervisor) validateConfig(configContent string) error {
	if s.commander == nil {
		s.telemetrySettings.Logger.Debug("Skipping config validation: commander not initialized")
		return nil
	}

	// Write config to a temporary file for validation
	tempFile, err := os.CreateTemp(s.config.Storage.Directory, "validate-config-*.yaml")
	if err != nil {
		return fmt.Errorf("failed to create temp config file for validation: %w", err)
	}
	defer os.Remove(tempFile.Name())

	if _, err := tempFile.WriteString(configContent); err != nil {
		tempFile.Close()
		return fmt.Errorf("failed to write temp config file for validation: %w", err)
	}

	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("failed to close temp config file: %w", err)
	}

	parentCtx := s.runCtx
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	ctx, cancel := context.WithTimeout(parentCtx, 10*time.Second)
	defer cancel()

	if err := s.commander.ValidateConfig(ctx, tempFile.Name(), s.getFeatureGateFlag()...); err != nil {
		err = fmt.Errorf("configuration validation failed: %w", err)
		// Changed here: reported to the server, so without the local server's token.
		if msg := s.redactLocalServerToken([]byte(err.Error())); string(msg) != err.Error() {
			return errors.New(string(msg))
		}
		return err
	}

	s.telemetrySettings.Logger.Debug("Configuration validation succeeded")
	return nil
}

func (s *Supervisor) stopAgentApplyConfig() {
	s.telemetrySettings.Logger.Debug("Stopping the agent to apply new config")
	err := s.commander.Stop(s.runCtx)
	if err != nil {
		s.telemetrySettings.Logger.Error("Could not stop agent process", zap.Error(err))
	}

	if err := s.writeAgentConfig(); err != nil {
		s.telemetrySettings.Logger.Error("Failed to write agent config", zap.Error(err))
	}
}

func (s *Supervisor) Shutdown() {
	defer s.runCtxCancel()

	s.telemetrySettings.Logger.Debug("Supervisor shutting down...")

	close(s.doneChan)

	// Shutdown in order from producer to consumer (agent -> customMessageForwarder -> local OpAMP server -> client to remote OpAMP server).
	s.agentWG.Wait()
	s.customMessageWG.Wait()

	if s.opampServer != nil {
		s.telemetrySettings.Logger.Debug("Stopping OpAMP server...")
		ctx, cancel := context.WithTimeout(s.runCtx, 5*time.Second)
		defer cancel()

		err := s.opampServer.Stop(ctx)
		if err != nil {
			s.telemetrySettings.Logger.Error("Could not stop the OpAMP Server")
		} else {
			s.telemetrySettings.Logger.Debug("OpAMP server stopped.")
		}
	}

	if s.healthCheckServer != nil {
		s.telemetrySettings.Logger.Debug("Stopping health check server...")
		ctx, cancel := context.WithTimeout(s.runCtx, 5*time.Second)
		defer cancel()

		err := s.healthCheckServer.Shutdown(ctx)

		if err != nil {
			s.telemetrySettings.Logger.Error("Could not stop the health check server", zap.Error(err))
		} else {
			s.healthCheckServerWG.Wait()
			s.telemetrySettings.Logger.Debug("Health check server stopped.")
		}
	}

	if s.opampClient != nil {
		err := s.SetHealth(
			&protobufs.ComponentHealth{
				Healthy: false, LastError: "Supervisor is shutdown",
			},
		)
		if err != nil {
			s.telemetrySettings.Logger.Error("Could not report health to OpAMP server", zap.Error(err))
		}

		err = s.stopOpAMPClient()
		if err != nil {
			s.telemetrySettings.Logger.Error("Could not stop the OpAMP client", zap.Error(err))
		}
	}

	// Stop extensions after the OpAMP client (which may use their HeaderFunc)
	// and before self-telemetry shuts down (extensions may log on shutdown).
	if s.extensions != nil {
		ctx, cancel := context.WithTimeout(s.runCtx, 5*time.Second)
		if err := s.extensions.Shutdown(ctx); err != nil {
			s.telemetrySettings.Logger.Error("Failed to shutdown extensions", zap.Error(err))
		}
		cancel()
	}

	if err := s.shutdownTelemetry(); err != nil {
		s.telemetrySettings.Logger.Error("Could not shut down self telemetry", zap.Error(err))
	}
}

func (s *Supervisor) shutdownTelemetry() error {
	ctx, cancel := context.WithTimeout(s.runCtx, 5*time.Second)
	defer cancel()
	// The metric.MeterProvider and trace.TracerProvider interfaces do not have a Shutdown method.
	// To shutdown the providers we try to cast to this interface, which matches the type signature used in the SDK.
	type shutdownable interface {
		Shutdown(context.Context) error
	}

	var err error

	if prov, ok := s.telemetrySettings.MeterProvider.(shutdownable); ok {
		if shutdownErr := prov.Shutdown(ctx); shutdownErr != nil {
			err = multierr.Append(err, fmt.Errorf("failed to shutdown meter provider: %w", shutdownErr))
		}
	}

	if prov, ok := s.telemetrySettings.TracerProvider.(shutdownable); ok {
		if shutdownErr := prov.Shutdown(ctx); shutdownErr != nil {
			err = multierr.Append(err, fmt.Errorf("failed to shutdown tracer provider: %w", shutdownErr))
		}
	}

	if prov, ok := s.telemetrySettings.loggerProvider.(shutdownable); ok {
		if shutdownErr := prov.Shutdown(ctx); shutdownErr != nil {
			err = multierr.Append(err, fmt.Errorf("failed to shutdown logger provider: %w", shutdownErr))
		}
	}

	return err
}

func (s *Supervisor) saveLastReceivedConfig(config *protobufs.AgentRemoteConfig) error {
	cfg, err := proto.Marshal(config)
	if err != nil {
		return err
	}

	return s.writeStorageFile(lastRecvRemoteConfigFile, cfg)
}

func (s *Supervisor) saveLastWorkingRemoteConfig(config *protobufs.AgentRemoteConfig) error {
	if config == nil {
		return nil
	}

	clonedConfig := proto.Clone(config).(*protobufs.AgentRemoteConfig)
	cfg, err := proto.Marshal(clonedConfig)
	if err != nil {
		return err
	}

	s.lastWorkingRemoteConfig.Store(clonedConfig)
	return s.writeStorageFile(lastWorkingRemoteConfigFile, cfg)
}

func (s *Supervisor) saveLastReceivedOwnTelemetrySettings(set *protobufs.ConnectionSettingsOffers, filePath string) error {
	cfg, err := proto.Marshal(set)
	if err != nil {
		return err
	}

	return s.writeStorageFile(filePath, cfg)
}

// reportConfigStatus saves and reports the status of the remote config with hash, which need
// not be the current one: a refused config is reported without becoming current.
func (s *Supervisor) reportConfigStatus(hash []byte, status protobufs.RemoteConfigStatuses, errorMessage string) {
	rcs := &protobufs.RemoteConfigStatus{
		LastRemoteConfigHash: hash,
		Status:               status,
		ErrorMessage:         errorMessage,
	}
	if err := s.persistentState.SetLastRemoteConfigStatus(rcs); err != nil {
		s.telemetrySettings.Logger.Error("Could not save last remote config status", zap.Error(err))
	}
	if err := s.opampClient.SetRemoteConfigStatus(rcs); err != nil {
		s.telemetrySettings.Logger.Error("Could not report OpAMP remote config status", zap.Error(err))
	}
}

// SetHealth publishes the agent's health to the OpAMP server and records it as
// the health a replacement client is given. All health publication goes through
// it, whether the health came from the agent or from process supervision, so
// that replacing the client preserves what the server was last told.
func (s *Supervisor) SetHealth(componentHealth *protobufs.ComponentHealth) error {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()

	return s.setHealthLocked(componentHealth)
}

// publishLastReportedHealth publishes the health last reported to the OpAMP
// server, so that a newly created client reports it instead of resetting it. It
// publishes unhealthy when no health has been reported yet.
func (s *Supervisor) publishLastReportedHealth() error {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()

	componentHealth := s.lastReportedHealth
	if componentHealth == nil {
		componentHealth = &protobufs.ComponentHealth{Healthy: false}
	}
	return s.setHealthLocked(componentHealth)
}

// setHealthLocked publishes health to the OpAMP server and records it. s.healthMu
// must be held: the read and the publication in publishLastReportedHealth have to
// exclude a concurrent update, otherwise health reported while the client is being
// replaced would be overwritten by the older value being republished.
func (s *Supervisor) setHealthLocked(componentHealth *protobufs.ComponentHealth) error {
	s.telemetrySettings.Logger.Debug(
		"Setting health",
		zap.Bool("healthy", componentHealth.Healthy),
		zap.String("lastError", componentHealth.LastError),
	)
	s.metrics.SetCollectorHealthStatus(s.runCtx, componentHealth.Healthy)

	err := s.opampClient.SetHealth(componentHealth)
	if err != nil {
		return fmt.Errorf("failed to set health to OpAMP server: %w", err)
	}
	// Cloned because the caller retains ownership of the message it passed.
	s.lastReportedHealth = proto.Clone(componentHealth).(*protobufs.ComponentHealth)
	return nil
}

func (s *Supervisor) onMessage(ctx context.Context, msg *types.MessageData) {
	ctx, span := s.getTracer().Start(ctx, "onMessage")
	defer span.End()
	configChanged := false

	if msg.AgentIdentification != nil {
		configChanged = s.processAgentIdentificationMessage(msg.AgentIdentification) || configChanged
	}

	if msg.RemoteConfig != nil {
		configChanged = s.processRemoteConfigMessage(ctx, msg.RemoteConfig) || configChanged
	}

	ownTelemetry := msg.OwnMetricsConnSettings != nil || msg.OwnTracesConnSettings != nil || msg.OwnLogsConnSettings != nil
	var ownChanged bool
	var ownErr error
	if ownTelemetry {
		ownChanged, ownErr = s.processOwnTelemetryConnSettingsMessage(ctx, &protobufs.ConnectionSettingsOffers{
			OwnMetrics: msg.OwnMetricsConnSettings,
			OwnTraces:  msg.OwnTracesConnSettings,
			OwnLogs:    msg.OwnLogsConnSettings,
		})
		configChanged = ownChanged || configChanged
	}

	// Changed here: the connection settings are answered once the collector runs with them, or
	// when they can't be applied. Own telemetry settings were APPLIED as they arrived. The
	// credentials go to the collector after the own telemetry settings, so its restart for them
	// has both.
	hash := msg.OfferedConnectionsSettingsHash
	switch {
	case msg.OtherConnSettings != nil:
		s.receiveConnectionCredentials(hash, msg.OtherConnSettings, ownErr)
	case ownErr != nil:
		s.reportConnectionSettings(hash, ownErr)
	case ownChanged:
		// Answered with the outcome of the collector's apply of its new config.
		s.pendingConnectionSettings.Store(&hash)
	case ownTelemetry:
		// The collector runs these settings already.
		s.reportConnectionSettings(hash, nil)
	}

	// Update the agent config if any messages have touched the config
	if configChanged {
		span.AddEvent("Config changed")
		err := s.opampClient.UpdateEffectiveConfig(ctx)
		if err != nil {
			s.telemetrySettings.Logger.Error("The OpAMP client failed to update the effective config", zap.Error(err))
		}

		s.telemetrySettings.Logger.Debug("Config is changed. Signal to restart the agent")
		// Signal that there is a new config.
		select {
		case s.hasNewConfig <- struct{}{}:
		default:
		}
	}

	messageToAgent := &protobufs.ServerToAgent{
		InstanceUid: s.persistentState.InstanceID[:],
	}
	haveMessageForAgent := false
	// Proxy server capabilities to opamp extension
	if msg.CustomCapabilities != nil {
		messageToAgent.CustomCapabilities = msg.CustomCapabilities
		haveMessageForAgent = true
	}

	// Proxy server messages to opamp extension
	if msg.CustomMessage != nil {
		messageToAgent.CustomMessage = msg.CustomMessage
		haveMessageForAgent = true
	}

	// Send any messages that need proxying to the agent. Changed here: only over a connection
	// still open; a closed one was used until the next collector's first message.
	if haveMessageForAgent {
		if conn := s.agentConn.current(); conn != nil {
			err := conn.Send(ctx, messageToAgent)
			if err != nil {
				s.telemetrySettings.Logger.Error("Error forwarding message to agent from server", zap.Error(err))
			}
		} else if msg.CustomMessage != nil {
			s.telemetrySettings.Logger.Warn("No collector is connected: the server's custom message is not forwarded to it",
				zap.String("capability", msg.CustomMessage.GetCapability()), zap.String("type", msg.CustomMessage.GetType()))
		}
	}

	span.SetStatus(codes.Ok, "")
}

// processRemoteConfigMessage processes an AgentRemoteConfig message, returning true if the agent config has changed.
func (s *Supervisor) processRemoteConfigMessage(ctx context.Context, msg *protobufs.AgentRemoteConfig) bool {
	_, span := s.getTracer().Start(ctx, "processRemoteConfigMessage")
	defer span.End()
	if !s.config.Capabilities.AcceptsRemoteConfig {
		s.telemetrySettings.Logger.Warn("Got remote config message, but the agent does not accept remote config. Ignoring remote config.")
		return false
	}

	// Changed here: a config the policy refuses is reported and goes no further. Kept as the
	// current or last received config, every later compose (and the next start) failed on it.
	if err := s.checkRemoteConfigPolicy(msg); err != nil {
		span.SetStatus(codes.Error, err.Error())
		s.telemetrySettings.Logger.Error("Refusing the remote config", zap.Error(err))
		s.reportConfigStatus(msg.GetConfigHash(), protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, err.Error())
		return false
	}

	// Clone the message to avoid race conditions when the protobuf is being unmarshaled
	// while another goroutine reads from it
	clonedMsg := proto.Clone(msg).(*protobufs.AgentRemoteConfig)
	s.telemetrySettings.Logger.Debug("Received remote config from server", zap.String("hash", fmt.Sprintf("%x", msg.ConfigHash)))

	// Changed here: it becomes the current (and last received) config only once it composes. A
	// config that fails was kept as both, and the next outcome of the config still running was
	// reported about it: APPLIED, and saved as the last working config.
	configChanged, err := s.composeMergedConfig(clonedMsg)
	if err != nil {
		span.SetStatus(codes.Error, fmt.Sprintf("Error composing merged config. Reporting failed remote config status: %s", err.Error()))
		s.telemetrySettings.Logger.Error("Error composing merged config. Reporting failed remote config status.", zap.Error(err))
		s.reportConfigStatus(msg.GetConfigHash(), protobufs.RemoteConfigStatuses_RemoteConfigStatuses_FAILED, err.Error())
		return false
	}
	s.usingLastWorkingRemoteConfig.Store(false)
	if err := s.saveLastReceivedConfig(msg); err != nil {
		span.SetStatus(codes.Error, fmt.Sprintf("Could not save last received remote config: %s", err.Error()))
		s.telemetrySettings.Logger.Error("Could not save last received remote config", zap.Error(err))
	}
	s.remoteConfig.Store(clonedMsg)

	if configChanged {
		// only report applying if the config has changed and will run agent with new config
		s.reportConfigStatus(msg.GetConfigHash(), protobufs.RemoteConfigStatuses_RemoteConfigStatuses_APPLYING, "")
	} else {
		// Changed here: the collector runs this body already, so it gets that apply's outcome,
		// which the agent loop has (APPLYING while it is still going). It was APPLIED at once.
		select {
		case s.configRetargets <- struct{}{}:
		default:
		}
	}

	span.SetStatus(codes.Ok, "")
	return configChanged
}

// processOwnTelemetryConnSettingsMessage processes a TelemetryConnectionSettings message, returning true if the agent config has changed.
// Changed here: settings it can't apply are refused, and neither saved nor applied.
func (s *Supervisor) processOwnTelemetryConnSettingsMessage(ctx context.Context, msg *protobufs.ConnectionSettingsOffers) (bool, error) {
	if err := unsupportedOwnTelemetry(msg); err != nil {
		s.telemetrySettings.Logger.Error("Refusing the own telemetry settings", zap.Error(err))
		return false, err
	}
	changed, err := s.setupOwnTelemetry(ctx, msg)
	if err != nil {
		return false, err
	}
	if err := s.saveLastReceivedOwnTelemetrySettings(msg, lastRecvOwnTelemetryConfigFile); err != nil {
		s.telemetrySettings.Logger.Error("Could not save last received own telemetry settings", zap.Error(err))
	}
	return changed, nil
}

// processAgentIdentificationMessage processes an AgentIdentification message, returning true if the agent config has changed.
func (s *Supervisor) processAgentIdentificationMessage(msg *protobufs.AgentIdentification) bool {
	newInstanceID, err := uuid.FromBytes(msg.NewInstanceUid)
	if err != nil {
		s.telemetrySettings.Logger.Error("Failed to parse instance UUID", zap.Error(err))
		return false
	}

	s.telemetrySettings.Logger.Debug("Agent identity is changing",
		zap.String("old_id", s.persistentState.InstanceID.String()),
		zap.String("new_id", newInstanceID.String()))

	err = s.persistentState.SetInstanceID(newInstanceID)
	if err != nil {
		s.telemetrySettings.Logger.Error("Failed to persist new instance ID, instance ID will revert on restart.", zap.String("new_id", newInstanceID.String()), zap.Error(err))
	}

	err = s.opampClient.SetAgentDescription(s.agentDescription.Load().(*protobufs.AgentDescription))
	if err != nil {
		s.telemetrySettings.Logger.Error("Failed to send agent description to OpAMP server")
	}

	// Need to recalculate the Agent config so that the new agent identification is included in it.
	configChanged, err := s.composeMergedConfig(s.remoteConfig.Load())
	if err != nil {
		s.telemetrySettings.Logger.Error("Error composing merged config with new instance ID", zap.Error(err))
		return false
	}

	return configChanged
}

func (s *Supervisor) persistentStateFilePath() string {
	return filepath.Join(s.config.Storage.Directory, persistentStateFileName)
}

func (s *Supervisor) agentConfigFilePath() string {
	return filepath.Join(s.config.Storage.Directory, agentConfigFileName)
}

func (s *Supervisor) getSupervisorOpAMPServerPort() (int, error) {
	if s.config.Agent.OpAMPServerPort != 0 {
		return s.config.Agent.OpAMPServerPort, nil
	}
	return s.findRandomPort()
}

func (s *Supervisor) getFeatureGateFlag() []string {
	flags := []string{}
	for k := range s.featureGates {
		flags = append(flags, k)
	}

	if len(flags) == 0 {
		return []string{}
	}

	return []string{"--feature-gates", strings.Join(flags, ",")}
}

func (s *Supervisor) isFeatureGateSupported(gate string) bool {
	_, ok := s.featureGates[gate]
	return ok
}

func (*Supervisor) findRandomPort() (int, error) {
	l, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		return 0, err
	}

	port := l.Addr().(*net.TCPAddr).Port

	err = l.Close()
	if err != nil {
		return 0, err
	}

	return port, nil
}

func (s *Supervisor) getTracer() trace.Tracer {
	tracer := s.telemetrySettings.TracerProvider.Tracer("github.com/pydantic/opamp-clients/go/opampsupervisor")
	return tracer
}
