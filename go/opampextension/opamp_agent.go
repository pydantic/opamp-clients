// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0
//
// Derived from opentelemetry-collector-contrib's extension/opampextension. Changed here: the
// OpAMP client is go/opamp's (the shared core, in Go mode) instead of opamp-go's. The status the
// collector reports before the client starts (health, custom capabilities) is kept and given to
// it at the start; the effective config is pushed when it changes instead of asked for; the
// available components' hash is the core's; and with a state file the client keeps the
// instance UID and the connection settings the server issued across restarts, sealed with
// state_key if set. A health update that clears the error is reported even when the status stays.

package opampextension

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"runtime"
	"slices"
	"sync"
	"syscall"

	"github.com/google/uuid"
	"github.com/oklog/ulid/v2"
	"github.com/open-telemetry/opamp-go/protobufs"
	"github.com/shirou/gopsutil/v4/host"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/component/componentstatus"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/extension"
	"go.opentelemetry.io/collector/extension/extensioncapabilities"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/service"
	"go.opentelemetry.io/collector/service/hostcapabilities"
	conventions "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.uber.org/zap"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"gopkg.in/yaml.v3"

	"github.com/open-telemetry/opentelemetry-collector-contrib/extension/opampcustommessages"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/status"
	"github.com/pydantic/opamp-clients/go/opamp"
	"github.com/pydantic/opamp-clients/go/statekey"
)

// rawConfigMapKey is the key under which the raw (unexpanded) configuration is
// reported in the OpAMP effective config map when reports_raw_config is enabled.
// The fully expanded effective configuration continues to be reported under the
// "" (empty) key for backward compatibility.
const rawConfigMapKey = "raw"

// errNotStarted: a custom message sent before the client started, or after it stopped.
var errNotStarted = errors.New("the OpAMP client isn't running")

// agentClient is what the extension uses of go/opamp's client once it has started (a test's
// stand-in otherwise). None of its methods blocks.
type agentClient interface {
	SetHealth(*protobufs.ComponentHealth)
	SetEffectiveConfig(*protobufs.EffectiveConfig)
	SetCustomCapabilities(*protobufs.CustomCapabilities)
	SendCustomMessage(*protobufs.CustomMessage)
	Stop(context.Context) error
}

type statusAggregator interface {
	Subscribe(scope status.Scope, verbosity status.Verbosity) (<-chan *status.AggregateStatus, status.UnsubscribeFunc)
	RecordStatus(source *componentstatus.InstanceID, event *componentstatus.Event)
}

type eventSourcePair struct {
	source *componentstatus.InstanceID
	event  *componentstatus.Event
}

type opampAgent struct {
	cfg    *Config
	logger *zap.Logger

	serviceName       string
	serviceVersion    string
	serviceInstanceID string
	resourceAttrs     map[string]string

	// instanceUID is the configured one until the client starts, then the client's.
	instanceUID uuid.UUID

	eclk            sync.RWMutex
	effectiveConfig *confmap.Conf
	// rawConfig holds the unexpanded configuration reported alongside the
	// effective config when reportsRawConfig is enabled. It is nil when raw
	// config reporting is disabled or unavailable.
	rawConfig *confmap.Conf
	// reportsRawConfig mirrors Config.ReportsRawConfig.
	reportsRawConfig bool

	// lifetimeCtx is canceled on Stop of the component
	lifetimeCtx       context.Context
	lifetimeCtxCancel context.CancelFunc

	reportFunc func(*componentstatus.Event)

	capabilities Capabilities

	agentDescription    *protobufs.AgentDescription
	availableComponents *protobufs.AvailableComponents

	// mu guards client and the status kept for its start: health and custom capabilities are
	// reported before the client exists (health from construction, capabilities as components
	// register), so the latest of each goes into the client's first message.
	mu                 sync.Mutex
	client             agentClient
	health             *protobufs.ComponentHealth
	customCapabilities *protobufs.CustomCapabilities

	// startClient starts the client: go/opamp's (startOpAMP), or a test's stand-in.
	startClient func(opamp.Config, opamp.Status, func(*opamp.Client, opamp.Notification)) (agentClient, [16]byte, error)

	customCapabilityRegistry *customCapabilityRegistry

	statusAggregator     statusAggregator
	statusSubscriptionWg *sync.WaitGroup
	componentHealthWg    *sync.WaitGroup
	startTimeUnixNano    uint64
	componentStatusCh    chan *eventSourcePair
	readyCh              chan struct{}
}

var (
	_ opampcustommessages.CustomCapabilityRegistry = (*opampAgent)(nil)
	_ extensioncapabilities.Dependent              = (*opampAgent)(nil)
	_ extensioncapabilities.ConfigSnapshotWatcher  = (*opampAgent)(nil)
	_ extensioncapabilities.PipelineWatcher        = (*opampAgent)(nil)
	_ componentstatus.Watcher                      = (*opampAgent)(nil)
	_ customCapabilityClient                       = (*opampAgent)(nil)

	// identifyingAttributes is the list of semantic convention keys that are used
	// for the agent description's identifying attributes.
	identifyingAttributes = map[string]struct{}{
		string(conventions.ServiceNameKey):       {},
		string(conventions.ServiceVersionKey):    {},
		string(conventions.ServiceInstanceIDKey): {},
	}
)

func (o *opampAgent) Start(_ context.Context, host component.Host) error {
	o.reportFunc = func(event *componentstatus.Event) {
		componentstatus.ReportStatus(host, event)
	}

	header := http.Header{}
	for k, v := range o.cfg.Server.GetHeaders() {
		header.Set(k, string(v))
	}

	tls, err := o.cfg.Server.GetTLSConfig(o.lifetimeCtx)
	if err != nil {
		return err
	}

	httpClient, err := newHTTPClient(o.cfg.Server, tls, host)
	if err != nil {
		return err
	}

	if o.cfg.PPID != 0 {
		go monitorPPID(o.lifetimeCtx, o.cfg.PPIDPollInterval, o.cfg.PPID, o.reportFunc)
	}

	if err := o.createAgentDescription(); err != nil {
		return err
	}

	if mi, ok := host.(hostcapabilities.ModuleInfo); ok {
		o.initAvailableComponents(mi.GetModuleInfos())
	} else if o.capabilities.ReportsAvailableComponents {
		// Report an empty set rather than none, as upstream does.
		o.initAvailableComponents(service.ModuleInfos{})
	}

	var sealer opamp.StateSealer
	if o.cfg.StateKey != "" {
		// Found (or made) before Start, so a missing key stops the extension rather than the
		// client writing the issued credentials in the clear.
		s, err := statekey.Load(o.cfg.StateKey, o.cfg.StateFile)
		if err != nil {
			return err
		}
		sealer = s
	}

	clientCfg := opamp.Config{
		Endpoint:     o.cfg.Server.GetEndpoint(),
		Capabilities: uint64(o.capabilities.toAgentCapabilities()),
		Header:       header,
		// Go mode: net/http carries the requests, so the collector's TLS settings, the
		// environment's proxies and auth extensions apply as they do to its other clients.
		Mode:                     opamp.ModeGo,
		HTTPClient:               httpClient,
		StateFile:                o.cfg.StateFile,
		StateSealer:              sealer,
		AcceptConnectionSettings: o.capabilities.AcceptsOpAMPConnectionSettings,
	}
	if o.cfg.InstanceUID != "" {
		clientCfg.InstanceUID = [16]byte(o.instanceUID)
	}
	if o.cfg.Server.HTTP != nil {
		// Over plain HTTP the heartbeat is the poll.
		clientCfg.HeartbeatInterval = o.cfg.Server.GetPollingInterval()
	}

	o.logger.Debug("Starting OpAMP client...")

	o.mu.Lock()
	defer o.mu.Unlock()
	initial := opamp.Status{
		Description:         o.agentDescription,
		Health:              o.health,
		EffectiveConfig:     o.composeEffectiveConfig(),
		AvailableComponents: o.availableComponents,
		CustomCapabilities:  o.customCapabilities,
	}
	client, uid, err := o.startClient(clientCfg, initial, o.onNotification)
	if err != nil {
		return err
	}
	o.client = client
	o.instanceUID = uid

	o.logger.Debug("OpAMP client started", zap.String("instance_uid", o.instanceUID.String()))

	return nil
}

func (o *opampAgent) Shutdown(ctx context.Context) error {
	if o.lifetimeCtxCancel != nil {
		o.lifetimeCtxCancel()
	}

	o.statusSubscriptionWg.Wait()
	o.componentHealthWg.Wait()
	if o.componentStatusCh != nil {
		close(o.componentStatusCh)
	}

	o.logger.Debug("OpAMP agent shutting down...")
	o.mu.Lock()
	client := o.client
	o.client = nil
	o.mu.Unlock()
	if client == nil {
		return nil
	}

	o.logger.Debug("Stopping OpAMP client...")
	return client.Stop(ctx)
}

// Dependencies implements extensioncapabilities.Dependent
func (o *opampAgent) Dependencies() []component.ID {
	if o.cfg.Server == nil {
		return nil
	}

	var emptyComponentID component.ID
	authID := o.cfg.Server.GetAuthExtensionID()
	if authID == emptyComponentID {
		return nil
	}

	return []component.ID{authID}
}

// NotifyConfigSnapshot implements the extensioncapabilities.ConfigSnapshotWatcher
// interface. It records the Collector's effective configuration and, when
// reports_raw_config is enabled, the raw (unexpanded) configuration, then pushes
// the effective config update to the OpAMP server.
func (o *opampAgent) NotifyConfigSnapshot(_ context.Context, configSnapshot extensioncapabilities.ConfigSnapshot) error {
	if !o.capabilities.ReportsEffectiveConfig {
		return nil
	}

	var rawConfig *confmap.Conf
	if o.reportsRawConfig {
		rawConfig = configSnapshot.Unexpanded()
	}
	o.updateEffectiveConfig(configSnapshot.Effective(), rawConfig)

	effective := o.composeEffectiveConfig()
	if effective == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.client != nil {
		o.client.SetEffectiveConfig(effective)
	}
	return nil
}

func (o *opampAgent) Register(capability string, opts ...opampcustommessages.CustomCapabilityRegisterOption) (opampcustommessages.CustomCapabilityHandler, error) {
	return o.customCapabilityRegistry.Register(capability, opts...)
}

// SetCustomCapabilities is the registry's: the capabilities with a handler registered.
func (o *opampAgent) SetCustomCapabilities(customCapabilities *protobufs.CustomCapabilities) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.customCapabilities = customCapabilities
	if o.client != nil {
		o.client.SetCustomCapabilities(customCapabilities)
	}
	return nil
}

// SendCustomMessage is the registry's. go/opamp's client takes a message at once and queues it,
// so the channel returned is closed already; a message it can't send after all is logged
// (CustomMessageNotSent).
func (o *opampAgent) SendCustomMessage(message *protobufs.CustomMessage) (chan struct{}, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.client == nil {
		return nil, errNotStarted
	}
	o.client.SendCustomMessage(message)
	sent := make(chan struct{})
	close(sent)
	return sent, nil
}

func (o *opampAgent) Ready() error {
	o.setHealth(&protobufs.ComponentHealth{Healthy: true})
	close(o.readyCh)
	return nil
}

func (o *opampAgent) NotReady() error {
	o.setHealth(&protobufs.ComponentHealth{Healthy: false})
	return nil
}

// ComponentStatusChanged implements the componentstatus.Watcher interface.
func (o *opampAgent) ComponentStatusChanged(
	source *componentstatus.InstanceID,
	event *componentstatus.Event,
) {
	// There can be late arriving events after shutdown. We need to close
	// the event channel so that this function doesn't block and we release all
	// goroutines, but attempting to write to a closed channel will panic; log
	// and recover.
	defer func() {
		if r := recover(); r != nil {
			o.logger.Info(
				"discarding event received after shutdown",
				zap.Any("source", source),
				zap.Any("event", event),
			)
		}
	}()
	o.componentStatusCh <- &eventSourcePair{source: source, event: event}
}

func (o *opampAgent) updateEffectiveConfig(effective, raw *confmap.Conf) {
	o.eclk.Lock()
	defer o.eclk.Unlock()

	o.effectiveConfig = effective
	o.rawConfig = raw
}

func newOpampAgent(cfg *Config, set extension.Settings) (*opampAgent, error) {
	serviceName := set.BuildInfo.Command

	sn, ok := set.Resource.Attributes().Get(string(conventions.ServiceNameKey))
	if ok {
		serviceName = sn.AsString()
	}

	serviceVersion := set.BuildInfo.Version

	sv, ok := set.Resource.Attributes().Get(string(conventions.ServiceVersionKey))
	if ok {
		serviceVersion = sv.AsString()
	}

	serviceInstanceID := ""

	if sid, ok := set.Resource.Attributes().Get(string(conventions.ServiceInstanceIDKey)); ok {
		serviceInstanceID = sid.Str()
	}

	// Without a configured UID the client makes one (a UUIDv7) and, with a state file, keeps it.
	var uid uuid.UUID
	if cfg.InstanceUID != "" {
		var err error
		uid, err = parseInstanceIDString(cfg.InstanceUID)
		if err != nil {
			return nil, fmt.Errorf("could not parse configured instance id: %w", err)
		}
	}

	resourceAttrs := make(map[string]string, set.Resource.Attributes().Len())
	set.Resource.Attributes().Range(func(k string, v pcommon.Value) bool {
		resourceAttrs[k] = v.Str()
		return true
	})

	agent := &opampAgent{
		cfg:                  cfg,
		logger:               set.Logger,
		serviceName:          serviceName,
		serviceVersion:       serviceVersion,
		serviceInstanceID:    serviceInstanceID,
		instanceUID:          uid,
		capabilities:         cfg.Capabilities,
		reportsRawConfig:     cfg.ReportsRawConfig,
		resourceAttrs:        resourceAttrs,
		statusSubscriptionWg: &sync.WaitGroup{},
		componentHealthWg:    &sync.WaitGroup{},
		readyCh:              make(chan struct{}),
		startClient:          startOpAMP,
	}
	agent.customCapabilityRegistry = newCustomCapabilityRegistry(set.Logger, agent)

	agent.lifetimeCtx, agent.lifetimeCtxCancel = context.WithCancel(context.Background())

	if agent.capabilities.ReportsHealth {
		agent.initHealthReporting()
	}

	return agent, nil
}

// startOpAMP starts go/opamp's client, and says the instance UID it uses.
func startOpAMP(cfg opamp.Config, initial opamp.Status, notify func(*opamp.Client, opamp.Notification)) (agentClient, [16]byte, error) {
	client, err := opamp.Start(context.Background(), cfg, initial, notify)
	if err != nil {
		return nil, [16]byte{}, err
	}
	return client, client.InstanceUID(), nil
}

func parseInstanceIDString(instanceUID string) (uuid.UUID, error) {
	parsedUUID, uuidParseErr := uuid.Parse(instanceUID)
	if uuidParseErr == nil {
		return parsedUUID, nil
	}

	parsedULID, ulidParseErr := ulid.Parse(instanceUID)
	if ulidParseErr == nil {
		return uuid.UUID(parsedULID), nil
	}

	return uuid.Nil, errors.Join(uuidParseErr, ulidParseErr)
}

func stringKeyValue(key, value string) *protobufs.KeyValue {
	return &protobufs.KeyValue{
		Key: key,
		Value: &protobufs.AnyValue{
			Value: &protobufs.AnyValue_StringValue{StringValue: value},
		},
	}
}

func (o *opampAgent) createAgentDescription() error {
	hostname, err := os.Hostname()
	if err != nil {
		return err
	}
	description := getOSDescription(o.logger)

	ident := []*protobufs.KeyValue{
		stringKeyValue(string(conventions.ServiceInstanceIDKey), o.serviceInstanceID),
		stringKeyValue(string(conventions.ServiceNameKey), o.serviceName),
		stringKeyValue(string(conventions.ServiceVersionKey), o.serviceVersion),
	}

	// Initially construct using a map to properly deduplicate any keys that
	// are both automatically determined and defined in the config
	nonIdentifyingAttributeMap := map[string]string{}
	nonIdentifyingAttributeMap[string(conventions.OSTypeKey)] = runtime.GOOS
	nonIdentifyingAttributeMap[string(conventions.HostArchKey)] = runtime.GOARCH
	nonIdentifyingAttributeMap[string(conventions.HostNameKey)] = hostname
	nonIdentifyingAttributeMap[string(conventions.OSDescriptionKey)] = description

	maps.Copy(nonIdentifyingAttributeMap, o.cfg.AgentDescription.NonIdentifyingAttributes)
	if o.cfg.AgentDescription.IncludeResourceAttributes {
		for k, v := range o.resourceAttrs {
			// skip the attributes that are being used in the identifying attributes.
			if _, ok := identifyingAttributes[k]; ok {
				continue
			}
			nonIdentifyingAttributeMap[k] = v
		}
	}

	// Sort the non identifying attributes to give them a stable order for tests
	keys := slices.Sorted(maps.Keys(nonIdentifyingAttributeMap))

	nonIdent := make([]*protobufs.KeyValue, 0, len(nonIdentifyingAttributeMap))
	for _, k := range keys {
		v := nonIdentifyingAttributeMap[k]
		nonIdent = append(nonIdent, stringKeyValue(k, v))
	}

	o.agentDescription = &protobufs.AgentDescription{
		IdentifyingAttributes:    ident,
		NonIdentifyingAttributes: nonIdent,
	}

	return nil
}

func (o *opampAgent) composeEffectiveConfig() *protobufs.EffectiveConfig {
	o.eclk.RLock()
	defer o.eclk.RUnlock()

	if !o.capabilities.ReportsEffectiveConfig || o.effectiveConfig == nil {
		return nil
	}

	conf, err := yaml.Marshal(o.effectiveConfig.ToStringMap())
	if err != nil {
		o.logger.Error("cannot marshal effective config", zap.Any("conf", o.effectiveConfig), zap.Error(err))
		return nil
	}

	configMap := map[string]*protobufs.AgentConfigObject{
		"": {
			Body:        conf,
			ContentType: "text/yaml",
		},
	}

	// When enabled, additionally report the raw (unexpanded) config under a
	// distinct key. A failure to marshal the raw config must not drop the
	// effective config reported under the "" key.
	if o.reportsRawConfig && o.rawConfig != nil {
		rawConf, err := yaml.Marshal(o.rawConfig.ToStringMap())
		if err != nil {
			o.logger.Error("cannot marshal raw config", zap.Any("conf", o.rawConfig), zap.Error(err))
		} else {
			configMap[rawConfigMapKey] = &protobufs.AgentConfigObject{
				Body:        rawConf,
				ContentType: "text/yaml",
			}
		}
	}

	return &protobufs.EffectiveConfig{
		ConfigMap: &protobufs.AgentConfigMap{
			ConfigMap: configMap,
		},
	}
}

// onNotification is the client's callback, run on its goroutine one at a time: it must not wait
// for anything the client does, and calls nothing that takes o.mu (Start holds it while the
// client starts).
func (o *opampAgent) onNotification(_ *opamp.Client, n opamp.Notification) {
	switch n.Kind {
	case opamp.Connected:
		o.logger.Debug("Connected to the OpAMP server")
	case opamp.ConnectFailed:
		o.logger.Error("Failed to connect to the OpAMP server", zap.Int("status", n.Status), zap.String("detail", n.Detail))
	case opamp.ErrorResponse:
		o.logger.Error("OpAMP server returned an error response", zap.String("message", n.Message))
	case opamp.Command:
		if err := o.onCommand(n.Command); err != nil {
			o.logger.Error("Failed to carry out the OpAMP server's command", zap.Error(err))
		}
	case opamp.InstanceUIDChanged:
		o.logger.Debug("OpAMP agent identity is being changed",
			zap.String("new_id", uuid.UUID(n.InstanceUID).String()))
	case opamp.CustomMessage:
		o.customCapabilityRegistry.ProcessMessage(n.CustomMessage)
	case opamp.CustomMessageNotSent:
		o.logger.Warn("A custom message to the OpAMP server was not sent",
			zap.String("capability", n.CustomMessage.GetCapability()), zap.String("reason", n.Detail))
	case opamp.StatusTooLarge:
		o.logger.Warn("A status field was too large to send to the OpAMP server",
			zap.String("field", n.FieldName), zap.Int("bytes", n.Bytes))
	case opamp.BadMessage:
		o.logger.Warn("The OpAMP server sent a message that could not be read", zap.String("detail", n.Detail))
	case opamp.SaveStalled:
		if n.Stalled {
			o.logger.Warn("Saving the OpAMP state has stalled: nothing else is saved until it ends",
				zap.Duration("waited", n.Waited))
		} else {
			o.logger.Info("The stalled save of the OpAMP state has ended", zap.Duration("waited", n.Waited))
		}
	case opamp.Stopped:
		if n.Err != nil {
			o.logger.Error("The OpAMP client stopped", zap.Error(n.Err))
		}
	default:
		// Remote config, own-telemetry settings and packages: capabilities this extension
		// doesn't declare, so the server has none to send.
	}
}

func (o *opampAgent) onCommand(command *protobufs.ServerToAgentCommand) error {
	if command.GetType() != protobufs.CommandType_CommandType_Restart {
		o.logger.Debug("ignoring non-restart command")
		return nil
	}

	if o.capabilities.AcceptsRestartCommand {
		o.logger.Info("received restart command, sending SIGHUP to reload")
		collectorProcess, err := os.FindProcess(os.Getpid())
		if err != nil {
			return fmt.Errorf("finding current process from pid: %w", err)
		}
		return collectorProcess.Signal(syscall.SIGHUP)
	}
	return nil
}

func (o *opampAgent) setHealth(ch *protobufs.ComponentHealth) {
	if !o.capabilities.ReportsHealth {
		return
	}
	if ch.Healthy && o.startTimeUnixNano == 0 {
		ch.StartTimeUnixNano = ch.StatusTimeUnixNano
	} else {
		ch.StartTimeUnixNano = o.startTimeUnixNano
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.health = ch
	if o.client != nil {
		o.client.SetHealth(ch)
	}
}

func getOSDescription(logger *zap.Logger) string {
	// Use PlatformInformation instead of host.Info so Windows containers without
	// MachineGuid can still report os.description.
	platform, _, version, err := host.PlatformInformation()
	if err != nil {
		logger.Error("failed getting platform information", zap.Error(err))
		return runtime.GOOS
	}
	switch runtime.GOOS {
	case "darwin":
		return "macOS " + version
	case "linux":
		return cases.Title(language.English).String(platform) + " " + version
	case "windows":
		return platform + " " + version
	default:
		return runtime.GOOS
	}
}

func (o *opampAgent) initHealthReporting() {
	if !o.capabilities.ReportsHealth {
		return
	}
	o.setHealth(&protobufs.ComponentHealth{Healthy: false})

	if o.statusAggregator == nil {
		o.statusAggregator = status.NewAggregator(status.PriorityPermanent)
	}
	statusChan, unsubscribeFunc := o.statusAggregator.Subscribe(status.ScopeAll, status.Verbose)
	o.statusSubscriptionWg.Add(1)
	go o.statusAggregatorEventLoop(unsubscribeFunc, statusChan)

	// Start processing events in the background so that our status watcher doesn't
	// block others before the extension starts.
	o.componentStatusCh = make(chan *eventSourcePair)
	o.componentHealthWg.Add(1)
	go o.componentHealthEventLoop()
}

// initAvailableComponents leaves the hash out: the core computes it from the components.
func (o *opampAgent) initAvailableComponents(moduleInfos service.ModuleInfos) {
	if !o.capabilities.ReportsAvailableComponents {
		return
	}

	o.availableComponents = &protobufs.AvailableComponents{
		Components: map[string]*protobufs.ComponentDetails{
			"receivers": {
				SubComponentMap: createComponentTypeAvailableComponentDetails(moduleInfos.Receiver),
			},
			"processors": {
				SubComponentMap: createComponentTypeAvailableComponentDetails(moduleInfos.Processor),
			},
			"exporters": {
				SubComponentMap: createComponentTypeAvailableComponentDetails(moduleInfos.Exporter),
			},
			"extensions": {
				SubComponentMap: createComponentTypeAvailableComponentDetails(moduleInfos.Extension),
			},
			"connectors": {
				SubComponentMap: createComponentTypeAvailableComponentDetails(moduleInfos.Connector),
			},
		},
	}
}

func createComponentTypeAvailableComponentDetails(componentTypeComponents map[component.Type]service.ModuleInfo) map[string]*protobufs.ComponentDetails {
	availableComponentDetails := map[string]*protobufs.ComponentDetails{}
	for componentType, r := range componentTypeComponents {
		availableComponentDetails[componentType.String()] = &protobufs.ComponentDetails{
			Metadata: []*protobufs.KeyValue{
				{
					Key: "code.namespace",
					Value: &protobufs.AnyValue{
						Value: &protobufs.AnyValue_StringValue{
							StringValue: r.BuilderRef,
						},
					},
				},
			},
		}
	}
	return availableComponentDetails
}

func (o *opampAgent) componentHealthEventLoop() {
	// Record events with component.StatusStarting, but queue other events until
	// PipelineWatcher.Ready is called. This prevents aggregate statuses from
	// flapping between StatusStarting and StatusOK as components are started
	// individually by the service.
	var eventQueue []*eventSourcePair

	defer o.componentHealthWg.Done()
	for loop := true; loop; {
		select {
		case esp, ok := <-o.componentStatusCh:
			if !ok {
				return
			}
			if esp.event.Status() != componentstatus.StatusStarting {
				eventQueue = append(eventQueue, esp)
				continue
			}
			o.statusAggregator.RecordStatus(esp.source, esp.event)
		case <-o.readyCh:
			for _, esp := range eventQueue {
				o.statusAggregator.RecordStatus(esp.source, esp.event)
			}
			eventQueue = nil
			loop = false
		case <-o.lifetimeCtx.Done():
			return
		}
	}

	// After PipelineWatcher.Ready, record statuses as they are received.
	for {
		select {
		case esp, ok := <-o.componentStatusCh:
			if !ok {
				return
			}
			o.statusAggregator.RecordStatus(esp.source, esp.event)
		case <-o.lifetimeCtx.Done():
			return
		}
	}
}

func (o *opampAgent) statusAggregatorEventLoop(unsubscribeFunc status.UnsubscribeFunc, statusChan <-chan *status.AggregateStatus) {
	defer func() {
		unsubscribeFunc()
		o.statusSubscriptionWg.Done()
	}()

	var (
		lastStatus componentstatus.Status
		lastErr    string
		currentErr string
	)

	for {
		select {
		case <-o.lifetimeCtx.Done():
			return
		case statusUpdate, ok := <-statusChan:
			if !ok {
				return
			}

			if statusUpdate == nil || statusUpdate.Status() == componentstatus.StatusNone {
				continue
			}

			currentStatus := statusUpdate.Status()
			currentErr = ""
			if statusUpdate.Err() != nil {
				currentErr = statusUpdate.Err().Error()
			}

			if currentStatus == lastStatus && currentErr == lastErr {
				continue
			}

			lastStatus = currentStatus
			lastErr = currentErr

			componentHealth := convertComponentHealth(statusUpdate)

			o.setHealth(componentHealth)
		}
	}
}

func convertComponentHealth(statusUpdate *status.AggregateStatus) *protobufs.ComponentHealth {
	var isHealthy bool
	if statusUpdate.Status() == componentstatus.StatusOK {
		isHealthy = true
	} else {
		isHealthy = false
	}

	componentHealth := &protobufs.ComponentHealth{
		Healthy:            isHealthy,
		Status:             statusUpdate.Status().String(),
		StatusTimeUnixNano: uint64(statusUpdate.Timestamp().UnixNano()),
	}

	if statusUpdate.Err() != nil {
		componentHealth.LastError = statusUpdate.Err().Error()
	}

	if len(statusUpdate.ComponentStatusMap) > 0 {
		componentHealth.ComponentHealthMap = map[string]*protobufs.ComponentHealth{}
		for comp, compState := range statusUpdate.ComponentStatusMap {
			componentHealth.ComponentHealthMap[comp] = convertComponentHealth(compState)
		}
	}

	return componentHealth
}
