from logfire_fleet_client._proto.opamp.v1 import anyvalue_pb2 as _anyvalue_pb2
from logfire_fleet_client._proto.opamp.v1 import opamp_pb2 as _opamp_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class SpanKind(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    SPAN_KIND_UNSPECIFIED: _ClassVar[SpanKind]
    SPAN_KIND_INTERNAL: _ClassVar[SpanKind]
    SPAN_KIND_SERVER: _ClassVar[SpanKind]
    SPAN_KIND_CLIENT: _ClassVar[SpanKind]
    SPAN_KIND_PRODUCER: _ClassVar[SpanKind]
    SPAN_KIND_CONSUMER: _ClassVar[SpanKind]

class Parent(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    PARENT_NONE: _ClassVar[Parent]
    PARENT_LOCAL_SAMPLED: _ClassVar[Parent]
    PARENT_LOCAL_UNSAMPLED: _ClassVar[Parent]
    PARENT_REMOTE_SAMPLED: _ClassVar[Parent]
    PARENT_REMOTE_UNSAMPLED: _ClassVar[Parent]
SPAN_KIND_UNSPECIFIED: SpanKind
SPAN_KIND_INTERNAL: SpanKind
SPAN_KIND_SERVER: SpanKind
SPAN_KIND_CLIENT: SpanKind
SPAN_KIND_PRODUCER: SpanKind
SPAN_KIND_CONSUMER: SpanKind
PARENT_NONE: Parent
PARENT_LOCAL_SAMPLED: Parent
PARENT_LOCAL_UNSAMPLED: Parent
PARENT_REMOTE_SAMPLED: Parent
PARENT_REMOTE_UNSAMPLED: Parent

class Settings(_message.Message):
    __slots__ = ("sampling_rate", "send_traces", "send_metrics", "send_self_metrics", "send_logs", "send_logs_min_severity", "sampling_rules", "send_logs_scope_levels", "drop_attributes", "mask_attributes", "sampling_traces_per_second", "send_logs_per_second", "deactivate_instrumentations", "deactivate_all_instrumentations", "custom", "scrub_defaults", "scrub_patterns")
    class CustomEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    SAMPLING_RATE_FIELD_NUMBER: _ClassVar[int]
    SEND_TRACES_FIELD_NUMBER: _ClassVar[int]
    SEND_METRICS_FIELD_NUMBER: _ClassVar[int]
    SEND_SELF_METRICS_FIELD_NUMBER: _ClassVar[int]
    SEND_LOGS_FIELD_NUMBER: _ClassVar[int]
    SEND_LOGS_MIN_SEVERITY_FIELD_NUMBER: _ClassVar[int]
    SAMPLING_RULES_FIELD_NUMBER: _ClassVar[int]
    SEND_LOGS_SCOPE_LEVELS_FIELD_NUMBER: _ClassVar[int]
    DROP_ATTRIBUTES_FIELD_NUMBER: _ClassVar[int]
    MASK_ATTRIBUTES_FIELD_NUMBER: _ClassVar[int]
    SAMPLING_TRACES_PER_SECOND_FIELD_NUMBER: _ClassVar[int]
    SEND_LOGS_PER_SECOND_FIELD_NUMBER: _ClassVar[int]
    DEACTIVATE_INSTRUMENTATIONS_FIELD_NUMBER: _ClassVar[int]
    DEACTIVATE_ALL_INSTRUMENTATIONS_FIELD_NUMBER: _ClassVar[int]
    CUSTOM_FIELD_NUMBER: _ClassVar[int]
    SCRUB_DEFAULTS_FIELD_NUMBER: _ClassVar[int]
    SCRUB_PATTERNS_FIELD_NUMBER: _ClassVar[int]
    sampling_rate: float
    send_traces: bool
    send_metrics: bool
    send_self_metrics: bool
    send_logs: bool
    send_logs_min_severity: int
    sampling_rules: _containers.RepeatedCompositeFieldContainer[SamplingRule]
    send_logs_scope_levels: _containers.RepeatedCompositeFieldContainer[ScopeLevel]
    drop_attributes: _containers.RepeatedScalarFieldContainer[str]
    mask_attributes: _containers.RepeatedScalarFieldContainer[str]
    sampling_traces_per_second: float
    send_logs_per_second: float
    deactivate_instrumentations: Names
    deactivate_all_instrumentations: bool
    custom: _containers.ScalarMap[str, str]
    scrub_defaults: bool
    scrub_patterns: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, sampling_rate: _Optional[float] = ..., send_traces: bool = ..., send_metrics: bool = ..., send_self_metrics: bool = ..., send_logs: bool = ..., send_logs_min_severity: _Optional[int] = ..., sampling_rules: _Optional[_Iterable[_Union[SamplingRule, _Mapping]]] = ..., send_logs_scope_levels: _Optional[_Iterable[_Union[ScopeLevel, _Mapping]]] = ..., drop_attributes: _Optional[_Iterable[str]] = ..., mask_attributes: _Optional[_Iterable[str]] = ..., sampling_traces_per_second: _Optional[float] = ..., send_logs_per_second: _Optional[float] = ..., deactivate_instrumentations: _Optional[_Union[Names, _Mapping]] = ..., deactivate_all_instrumentations: bool = ..., custom: _Optional[_Mapping[str, str]] = ..., scrub_defaults: bool = ..., scrub_patterns: _Optional[_Iterable[str]] = ...) -> None: ...

class SamplingRule(_message.Message):
    __slots__ = ("rate", "name", "kind", "path", "attributes")
    class AttributesEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    RATE_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    KIND_FIELD_NUMBER: _ClassVar[int]
    PATH_FIELD_NUMBER: _ClassVar[int]
    ATTRIBUTES_FIELD_NUMBER: _ClassVar[int]
    rate: float
    name: str
    kind: SpanKind
    path: str
    attributes: _containers.ScalarMap[str, str]
    def __init__(self, rate: _Optional[float] = ..., name: _Optional[str] = ..., kind: _Optional[_Union[SpanKind, str]] = ..., path: _Optional[str] = ..., attributes: _Optional[_Mapping[str, str]] = ...) -> None: ...

class ScopeLevel(_message.Message):
    __slots__ = ("pattern", "min_severity")
    PATTERN_FIELD_NUMBER: _ClassVar[int]
    MIN_SEVERITY_FIELD_NUMBER: _ClassVar[int]
    pattern: str
    min_severity: int
    def __init__(self, pattern: _Optional[str] = ..., min_severity: _Optional[int] = ...) -> None: ...

class Names(_message.Message):
    __slots__ = ("names",)
    NAMES_FIELD_NUMBER: _ClassVar[int]
    names: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, names: _Optional[_Iterable[str]] = ...) -> None: ...

class Host(_message.Message):
    __slots__ = ("live", "next_start")
    LIVE_FIELD_NUMBER: _ClassVar[int]
    NEXT_START_FIELD_NUMBER: _ClassVar[int]
    live: _containers.RepeatedScalarFieldContainer[str]
    next_start: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, live: _Optional[_Iterable[str]] = ..., next_start: _Optional[_Iterable[str]] = ...) -> None: ...

class AgentStart(_message.Message):
    __slots__ = ("host", "state_dir", "core_state", "endpoint", "instance_uid", "hints", "now_unix_ms", "now_mono_ms")
    HOST_FIELD_NUMBER: _ClassVar[int]
    STATE_DIR_FIELD_NUMBER: _ClassVar[int]
    CORE_STATE_FIELD_NUMBER: _ClassVar[int]
    ENDPOINT_FIELD_NUMBER: _ClassVar[int]
    INSTANCE_UID_FIELD_NUMBER: _ClassVar[int]
    HINTS_FIELD_NUMBER: _ClassVar[int]
    NOW_UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    NOW_MONO_MS_FIELD_NUMBER: _ClassVar[int]
    host: Host
    state_dir: bool
    core_state: bytes
    endpoint: str
    instance_uid: bytes
    hints: AgentHints
    now_unix_ms: int
    now_mono_ms: int
    def __init__(self, host: _Optional[_Union[Host, _Mapping]] = ..., state_dir: bool = ..., core_state: _Optional[bytes] = ..., endpoint: _Optional[str] = ..., instance_uid: _Optional[bytes] = ..., hints: _Optional[_Union[AgentHints, _Mapping]] = ..., now_unix_ms: _Optional[int] = ..., now_mono_ms: _Optional[int] = ...) -> None: ...

class AgentHints(_message.Message):
    __slots__ = ("refused", "not_found", "unreachable", "error")
    REFUSED_FIELD_NUMBER: _ClassVar[int]
    NOT_FOUND_FIELD_NUMBER: _ClassVar[int]
    UNREACHABLE_FIELD_NUMBER: _ClassVar[int]
    ERROR_FIELD_NUMBER: _ClassVar[int]
    refused: str
    not_found: str
    unreachable: str
    error: str
    def __init__(self, refused: _Optional[str] = ..., not_found: _Optional[str] = ..., unreachable: _Optional[str] = ..., error: _Optional[str] = ...) -> None: ...

class AgentStep(_message.Message):
    __slots__ = ("agent", "now_unix_ms", "now_mono_ms", "remote_config", "connected", "connect_failed", "instance_uid_changed", "uid_not_saved", "state_from_newer_version", "stopped", "timer", "exporters", "host", "built")
    AGENT_FIELD_NUMBER: _ClassVar[int]
    NOW_UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    NOW_MONO_MS_FIELD_NUMBER: _ClassVar[int]
    REMOTE_CONFIG_FIELD_NUMBER: _ClassVar[int]
    CONNECTED_FIELD_NUMBER: _ClassVar[int]
    CONNECT_FAILED_FIELD_NUMBER: _ClassVar[int]
    INSTANCE_UID_CHANGED_FIELD_NUMBER: _ClassVar[int]
    UID_NOT_SAVED_FIELD_NUMBER: _ClassVar[int]
    STATE_FROM_NEWER_VERSION_FIELD_NUMBER: _ClassVar[int]
    STOPPED_FIELD_NUMBER: _ClassVar[int]
    TIMER_FIELD_NUMBER: _ClassVar[int]
    EXPORTERS_FIELD_NUMBER: _ClassVar[int]
    HOST_FIELD_NUMBER: _ClassVar[int]
    BUILT_FIELD_NUMBER: _ClassVar[int]
    agent: bytes
    now_unix_ms: int
    now_mono_ms: int
    remote_config: _opamp_pb2.AgentRemoteConfig
    connected: AgentConnected
    connect_failed: AgentConnectFailed
    instance_uid_changed: bytes
    uid_not_saved: str
    state_from_newer_version: int
    stopped: AgentStopped
    timer: ExportCounts
    exporters: AgentExporters
    host: Host
    built: AgentBuilt
    def __init__(self, agent: _Optional[bytes] = ..., now_unix_ms: _Optional[int] = ..., now_mono_ms: _Optional[int] = ..., remote_config: _Optional[_Union[_opamp_pb2.AgentRemoteConfig, _Mapping]] = ..., connected: _Optional[_Union[AgentConnected, _Mapping]] = ..., connect_failed: _Optional[_Union[AgentConnectFailed, _Mapping]] = ..., instance_uid_changed: _Optional[bytes] = ..., uid_not_saved: _Optional[str] = ..., state_from_newer_version: _Optional[int] = ..., stopped: _Optional[_Union[AgentStopped, _Mapping]] = ..., timer: _Optional[_Union[ExportCounts, _Mapping]] = ..., exporters: _Optional[_Union[AgentExporters, _Mapping]] = ..., host: _Optional[_Union[Host, _Mapping]] = ..., built: _Optional[_Union[AgentBuilt, _Mapping]] = ...) -> None: ...

class AgentBuilt(_message.Message):
    __slots__ = ("service", "exporters")
    SERVICE_FIELD_NUMBER: _ClassVar[int]
    EXPORTERS_FIELD_NUMBER: _ClassVar[int]
    service: str
    exporters: _containers.RepeatedCompositeFieldContainer[Exporter]
    def __init__(self, service: _Optional[str] = ..., exporters: _Optional[_Iterable[_Union[Exporter, _Mapping]]] = ...) -> None: ...

class AgentConnected(_message.Message):
    __slots__ = ("endpoint",)
    ENDPOINT_FIELD_NUMBER: _ClassVar[int]
    endpoint: str
    def __init__(self, endpoint: _Optional[str] = ...) -> None: ...

class AgentConnectFailed(_message.Message):
    __slots__ = ("endpoint", "status", "detail")
    ENDPOINT_FIELD_NUMBER: _ClassVar[int]
    STATUS_FIELD_NUMBER: _ClassVar[int]
    DETAIL_FIELD_NUMBER: _ClassVar[int]
    endpoint: str
    status: int
    detail: str
    def __init__(self, endpoint: _Optional[str] = ..., status: _Optional[int] = ..., detail: _Optional[str] = ...) -> None: ...

class AgentStopped(_message.Message):
    __slots__ = ("error",)
    ERROR_FIELD_NUMBER: _ClassVar[int]
    error: str
    def __init__(self, error: _Optional[str] = ...) -> None: ...

class AgentExporters(_message.Message):
    __slots__ = ("exporters", "counts")
    EXPORTERS_FIELD_NUMBER: _ClassVar[int]
    COUNTS_FIELD_NUMBER: _ClassVar[int]
    exporters: _containers.RepeatedCompositeFieldContainer[Exporter]
    counts: ExportCounts
    def __init__(self, exporters: _Optional[_Iterable[_Union[Exporter, _Mapping]]] = ..., counts: _Optional[_Union[ExportCounts, _Mapping]] = ...) -> None: ...

class AgentOutput(_message.Message):
    __slots__ = ("agent", "effects", "wake_at_mono_ms", "effective")
    AGENT_FIELD_NUMBER: _ClassVar[int]
    EFFECTS_FIELD_NUMBER: _ClassVar[int]
    WAKE_AT_MONO_MS_FIELD_NUMBER: _ClassVar[int]
    EFFECTIVE_FIELD_NUMBER: _ClassVar[int]
    agent: bytes
    effects: _containers.RepeatedCompositeFieldContainer[AgentEffect]
    wake_at_mono_ms: int
    effective: _opamp_pb2.EffectiveConfig
    def __init__(self, agent: _Optional[bytes] = ..., effects: _Optional[_Iterable[_Union[AgentEffect, _Mapping]]] = ..., wake_at_mono_ms: _Optional[int] = ..., effective: _Optional[_Union[_opamp_pb2.EffectiveConfig, _Mapping]] = ...) -> None: ...

class AgentEffect(_message.Message):
    __slots__ = ("apply", "effective", "status", "health", "save_uid", "log")
    APPLY_FIELD_NUMBER: _ClassVar[int]
    EFFECTIVE_FIELD_NUMBER: _ClassVar[int]
    STATUS_FIELD_NUMBER: _ClassVar[int]
    HEALTH_FIELD_NUMBER: _ClassVar[int]
    SAVE_UID_FIELD_NUMBER: _ClassVar[int]
    LOG_FIELD_NUMBER: _ClassVar[int]
    apply: Settings
    effective: _opamp_pb2.EffectiveConfig
    status: _opamp_pb2.RemoteConfigStatus
    health: _opamp_pb2.ComponentHealth
    save_uid: str
    log: AgentLog
    def __init__(self, apply: _Optional[_Union[Settings, _Mapping]] = ..., effective: _Optional[_Union[_opamp_pb2.EffectiveConfig, _Mapping]] = ..., status: _Optional[_Union[_opamp_pb2.RemoteConfigStatus, _Mapping]] = ..., health: _Optional[_Union[_opamp_pb2.ComponentHealth, _Mapping]] = ..., save_uid: _Optional[str] = ..., log: _Optional[_Union[AgentLog, _Mapping]] = ...) -> None: ...

class AgentLog(_message.Message):
    __slots__ = ("level", "text")
    class Level(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
        __slots__ = ()
        DEBUG: _ClassVar[AgentLog.Level]
        INFO: _ClassVar[AgentLog.Level]
        NOTICE: _ClassVar[AgentLog.Level]
        WARN: _ClassVar[AgentLog.Level]
        ERROR: _ClassVar[AgentLog.Level]
    DEBUG: AgentLog.Level
    INFO: AgentLog.Level
    NOTICE: AgentLog.Level
    WARN: AgentLog.Level
    ERROR: AgentLog.Level
    LEVEL_FIELD_NUMBER: _ClassVar[int]
    TEXT_FIELD_NUMBER: _ClassVar[int]
    level: AgentLog.Level
    text: str
    def __init__(self, level: _Optional[_Union[AgentLog.Level, str]] = ..., text: _Optional[str] = ...) -> None: ...

class AgentState(_message.Message):
    __slots__ = ("host", "state_dir", "current", "at_start", "effective", "exporters", "health", "refresh_at_unix_ms", "refresh_due_mono_ms", "health_due_mono_ms", "started_unix_ms", "endpoint", "service", "uid", "hints", "ever", "failing", "asked_unix_ms", "asked_mono_ms")
    HOST_FIELD_NUMBER: _ClassVar[int]
    STATE_DIR_FIELD_NUMBER: _ClassVar[int]
    CURRENT_FIELD_NUMBER: _ClassVar[int]
    AT_START_FIELD_NUMBER: _ClassVar[int]
    EFFECTIVE_FIELD_NUMBER: _ClassVar[int]
    EXPORTERS_FIELD_NUMBER: _ClassVar[int]
    HEALTH_FIELD_NUMBER: _ClassVar[int]
    REFRESH_AT_UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    REFRESH_DUE_MONO_MS_FIELD_NUMBER: _ClassVar[int]
    HEALTH_DUE_MONO_MS_FIELD_NUMBER: _ClassVar[int]
    STARTED_UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    ENDPOINT_FIELD_NUMBER: _ClassVar[int]
    SERVICE_FIELD_NUMBER: _ClassVar[int]
    UID_FIELD_NUMBER: _ClassVar[int]
    HINTS_FIELD_NUMBER: _ClassVar[int]
    EVER_FIELD_NUMBER: _ClassVar[int]
    FAILING_FIELD_NUMBER: _ClassVar[int]
    ASKED_UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    ASKED_MONO_MS_FIELD_NUMBER: _ClassVar[int]
    host: Host
    state_dir: bool
    current: bytes
    at_start: bytes
    effective: str
    exporters: _containers.RepeatedCompositeFieldContainer[Exporter]
    health: HealthReport
    refresh_at_unix_ms: int
    refresh_due_mono_ms: int
    health_due_mono_ms: int
    started_unix_ms: int
    endpoint: str
    service: str
    uid: str
    hints: AgentHints
    ever: bool
    failing: bool
    asked_unix_ms: int
    asked_mono_ms: int
    def __init__(self, host: _Optional[_Union[Host, _Mapping]] = ..., state_dir: bool = ..., current: _Optional[bytes] = ..., at_start: _Optional[bytes] = ..., effective: _Optional[str] = ..., exporters: _Optional[_Iterable[_Union[Exporter, _Mapping]]] = ..., health: _Optional[_Union[HealthReport, _Mapping]] = ..., refresh_at_unix_ms: _Optional[int] = ..., refresh_due_mono_ms: _Optional[int] = ..., health_due_mono_ms: _Optional[int] = ..., started_unix_ms: _Optional[int] = ..., endpoint: _Optional[str] = ..., service: _Optional[str] = ..., uid: _Optional[str] = ..., hints: _Optional[_Union[AgentHints, _Mapping]] = ..., ever: bool = ..., failing: bool = ..., asked_unix_ms: _Optional[int] = ..., asked_mono_ms: _Optional[int] = ...) -> None: ...

class ExportCounts(_message.Message):
    __slots__ = ("traces", "metrics", "logs")
    TRACES_FIELD_NUMBER: _ClassVar[int]
    METRICS_FIELD_NUMBER: _ClassVar[int]
    LOGS_FIELD_NUMBER: _ClassVar[int]
    traces: SignalCounts
    metrics: SignalCounts
    logs: SignalCounts
    def __init__(self, traces: _Optional[_Union[SignalCounts, _Mapping]] = ..., metrics: _Optional[_Union[SignalCounts, _Mapping]] = ..., logs: _Optional[_Union[SignalCounts, _Mapping]] = ...) -> None: ...

class SignalCounts(_message.Message):
    __slots__ = ("exports_ok", "exports_failed", "items_failed", "items_filtered", "last_failed", "last_error")
    EXPORTS_OK_FIELD_NUMBER: _ClassVar[int]
    EXPORTS_FAILED_FIELD_NUMBER: _ClassVar[int]
    ITEMS_FAILED_FIELD_NUMBER: _ClassVar[int]
    ITEMS_FILTERED_FIELD_NUMBER: _ClassVar[int]
    LAST_FAILED_FIELD_NUMBER: _ClassVar[int]
    LAST_ERROR_FIELD_NUMBER: _ClassVar[int]
    exports_ok: int
    exports_failed: int
    items_failed: int
    items_filtered: int
    last_failed: bool
    last_error: str
    def __init__(self, exports_ok: _Optional[int] = ..., exports_failed: _Optional[int] = ..., items_failed: _Optional[int] = ..., items_filtered: _Optional[int] = ..., last_failed: bool = ..., last_error: _Optional[str] = ...) -> None: ...

class HealthReport(_message.Message):
    __slots__ = ("send", "exporters", "sent_unix_ms")
    SEND_FIELD_NUMBER: _ClassVar[int]
    EXPORTERS_FIELD_NUMBER: _ClassVar[int]
    SENT_UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    send: bool
    exporters: _opamp_pb2.ComponentHealth
    sent_unix_ms: int
    def __init__(self, send: bool = ..., exporters: _Optional[_Union[_opamp_pb2.ComponentHealth, _Mapping]] = ..., sent_unix_ms: _Optional[int] = ...) -> None: ...

class Describe(_message.Message):
    __slots__ = ("resource", "host", "now_unix_ms")
    RESOURCE_FIELD_NUMBER: _ClassVar[int]
    HOST_FIELD_NUMBER: _ClassVar[int]
    NOW_UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    resource: _containers.RepeatedCompositeFieldContainer[_anyvalue_pb2.KeyValue]
    host: Host
    now_unix_ms: int
    def __init__(self, resource: _Optional[_Iterable[_Union[_anyvalue_pb2.KeyValue, _Mapping]]] = ..., host: _Optional[_Union[Host, _Mapping]] = ..., now_unix_ms: _Optional[int] = ...) -> None: ...

class Components(_message.Message):
    __slots__ = ("inventory", "host", "now_unix_ms")
    INVENTORY_FIELD_NUMBER: _ClassVar[int]
    HOST_FIELD_NUMBER: _ClassVar[int]
    NOW_UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    inventory: Inventory
    host: Host
    now_unix_ms: int
    def __init__(self, inventory: _Optional[_Union[Inventory, _Mapping]] = ..., host: _Optional[_Union[Host, _Mapping]] = ..., now_unix_ms: _Optional[int] = ...) -> None: ...

class Inventory(_message.Message):
    __slots__ = ("integration", "integration_version", "sdk_language", "sdk_version", "instrumentations", "exporters")
    INTEGRATION_FIELD_NUMBER: _ClassVar[int]
    INTEGRATION_VERSION_FIELD_NUMBER: _ClassVar[int]
    SDK_LANGUAGE_FIELD_NUMBER: _ClassVar[int]
    SDK_VERSION_FIELD_NUMBER: _ClassVar[int]
    INSTRUMENTATIONS_FIELD_NUMBER: _ClassVar[int]
    EXPORTERS_FIELD_NUMBER: _ClassVar[int]
    integration: str
    integration_version: str
    sdk_language: str
    sdk_version: str
    instrumentations: _containers.RepeatedCompositeFieldContainer[Instrumentation]
    exporters: _containers.RepeatedCompositeFieldContainer[Exporter]
    def __init__(self, integration: _Optional[str] = ..., integration_version: _Optional[str] = ..., sdk_language: _Optional[str] = ..., sdk_version: _Optional[str] = ..., instrumentations: _Optional[_Iterable[_Union[Instrumentation, _Mapping]]] = ..., exporters: _Optional[_Iterable[_Union[Exporter, _Mapping]]] = ...) -> None: ...

class Instrumentation(_message.Message):
    __slots__ = ("name", "version")
    NAME_FIELD_NUMBER: _ClassVar[int]
    VERSION_FIELD_NUMBER: _ClassVar[int]
    name: str
    version: str
    def __init__(self, name: _Optional[str] = ..., version: _Optional[str] = ...) -> None: ...

class Exporter(_message.Message):
    __slots__ = ("signal", "name", "gated", "destination")
    SIGNAL_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    GATED_FIELD_NUMBER: _ClassVar[int]
    DESTINATION_FIELD_NUMBER: _ClassVar[int]
    signal: str
    name: str
    gated: bool
    destination: str
    def __init__(self, signal: _Optional[str] = ..., name: _Optional[str] = ..., gated: bool = ..., destination: _Optional[str] = ...) -> None: ...

class Exporters(_message.Message):
    __slots__ = ("found", "env", "default_protocol")
    FOUND_FIELD_NUMBER: _ClassVar[int]
    ENV_FIELD_NUMBER: _ClassVar[int]
    DEFAULT_PROTOCOL_FIELD_NUMBER: _ClassVar[int]
    found: _containers.RepeatedCompositeFieldContainer[FoundExporter]
    env: _containers.RepeatedCompositeFieldContainer[EnvVar]
    default_protocol: str
    def __init__(self, found: _Optional[_Iterable[_Union[FoundExporter, _Mapping]]] = ..., env: _Optional[_Iterable[_Union[EnvVar, _Mapping]]] = ..., default_protocol: _Optional[str] = ...) -> None: ...

class FoundExporter(_message.Message):
    __slots__ = ("signal", "kind", "gated", "endpoint", "from_env")
    SIGNAL_FIELD_NUMBER: _ClassVar[int]
    KIND_FIELD_NUMBER: _ClassVar[int]
    GATED_FIELD_NUMBER: _ClassVar[int]
    ENDPOINT_FIELD_NUMBER: _ClassVar[int]
    FROM_ENV_FIELD_NUMBER: _ClassVar[int]
    signal: str
    kind: str
    gated: bool
    endpoint: str
    from_env: bool
    def __init__(self, signal: _Optional[str] = ..., kind: _Optional[str] = ..., gated: bool = ..., endpoint: _Optional[str] = ..., from_env: bool = ...) -> None: ...

class EnvVar(_message.Message):
    __slots__ = ("name", "value")
    NAME_FIELD_NUMBER: _ClassVar[int]
    VALUE_FIELD_NUMBER: _ClassVar[int]
    name: str
    value: str
    def __init__(self, name: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...

class ExportersDecided(_message.Message):
    __slots__ = ("exporters", "governed", "ungoverned", "warnings")
    EXPORTERS_FIELD_NUMBER: _ClassVar[int]
    GOVERNED_FIELD_NUMBER: _ClassVar[int]
    UNGOVERNED_FIELD_NUMBER: _ClassVar[int]
    WARNINGS_FIELD_NUMBER: _ClassVar[int]
    exporters: _containers.RepeatedCompositeFieldContainer[Exporter]
    governed: _containers.RepeatedScalarFieldContainer[str]
    ungoverned: _containers.RepeatedScalarFieldContainer[str]
    warnings: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, exporters: _Optional[_Iterable[_Union[Exporter, _Mapping]]] = ..., governed: _Optional[_Iterable[str]] = ..., ungoverned: _Optional[_Iterable[str]] = ..., warnings: _Optional[_Iterable[str]] = ...) -> None: ...

class Evaluate(_message.Message):
    __slots__ = ("settings", "host", "now_unix_ms", "span", "log", "attributes", "cap")
    SETTINGS_FIELD_NUMBER: _ClassVar[int]
    HOST_FIELD_NUMBER: _ClassVar[int]
    NOW_UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    SPAN_FIELD_NUMBER: _ClassVar[int]
    LOG_FIELD_NUMBER: _ClassVar[int]
    ATTRIBUTES_FIELD_NUMBER: _ClassVar[int]
    CAP_FIELD_NUMBER: _ClassVar[int]
    settings: bytes
    host: Host
    now_unix_ms: int
    span: SpanStart
    log: LogRecord
    attributes: Attributes
    cap: CapRun
    def __init__(self, settings: _Optional[bytes] = ..., host: _Optional[_Union[Host, _Mapping]] = ..., now_unix_ms: _Optional[int] = ..., span: _Optional[_Union[SpanStart, _Mapping]] = ..., log: _Optional[_Union[LogRecord, _Mapping]] = ..., attributes: _Optional[_Union[Attributes, _Mapping]] = ..., cap: _Optional[_Union[CapRun, _Mapping]] = ...) -> None: ...

class CapRun(_message.Message):
    __slots__ = ("signal", "events")
    class Signal(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
        __slots__ = ()
        TRACES: _ClassVar[CapRun.Signal]
        LOGS: _ClassVar[CapRun.Signal]
    TRACES: CapRun.Signal
    LOGS: CapRun.Signal
    SIGNAL_FIELD_NUMBER: _ClassVar[int]
    EVENTS_FIELD_NUMBER: _ClassVar[int]
    signal: CapRun.Signal
    events: _containers.RepeatedCompositeFieldContainer[CapEvent]
    def __init__(self, signal: _Optional[_Union[CapRun.Signal, str]] = ..., events: _Optional[_Iterable[_Union[CapEvent, _Mapping]]] = ...) -> None: ...

class CapEvent(_message.Message):
    __slots__ = ("at_ns", "parent", "sampled", "records")
    AT_NS_FIELD_NUMBER: _ClassVar[int]
    PARENT_FIELD_NUMBER: _ClassVar[int]
    SAMPLED_FIELD_NUMBER: _ClassVar[int]
    RECORDS_FIELD_NUMBER: _ClassVar[int]
    at_ns: int
    parent: Parent
    sampled: bool
    records: int
    def __init__(self, at_ns: _Optional[int] = ..., parent: _Optional[_Union[Parent, str]] = ..., sampled: bool = ..., records: _Optional[int] = ...) -> None: ...

class Attributes(_message.Message):
    __slots__ = ("attributes", "body")
    ATTRIBUTES_FIELD_NUMBER: _ClassVar[int]
    BODY_FIELD_NUMBER: _ClassVar[int]
    attributes: _containers.RepeatedCompositeFieldContainer[_anyvalue_pb2.KeyValue]
    body: _anyvalue_pb2.AnyValue
    def __init__(self, attributes: _Optional[_Iterable[_Union[_anyvalue_pb2.KeyValue, _Mapping]]] = ..., body: _Optional[_Union[_anyvalue_pb2.AnyValue, _Mapping]] = ...) -> None: ...

class LogRecord(_message.Message):
    __slots__ = ("scope", "severity")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    SEVERITY_FIELD_NUMBER: _ClassVar[int]
    scope: str
    severity: int
    def __init__(self, scope: _Optional[str] = ..., severity: _Optional[int] = ...) -> None: ...

class SpanStart(_message.Message):
    __slots__ = ("name", "kind", "attributes", "parent")
    NAME_FIELD_NUMBER: _ClassVar[int]
    KIND_FIELD_NUMBER: _ClassVar[int]
    ATTRIBUTES_FIELD_NUMBER: _ClassVar[int]
    PARENT_FIELD_NUMBER: _ClassVar[int]
    name: str
    kind: SpanKind
    attributes: _containers.RepeatedCompositeFieldContainer[_anyvalue_pb2.KeyValue]
    parent: Parent
    def __init__(self, name: _Optional[str] = ..., kind: _Optional[_Union[SpanKind, str]] = ..., attributes: _Optional[_Iterable[_Union[_anyvalue_pb2.KeyValue, _Mapping]]] = ..., parent: _Optional[_Union[Parent, str]] = ...) -> None: ...

class Evaluated(_message.Message):
    __slots__ = ("refused", "sampling", "kept", "redacted", "body", "passed")
    REFUSED_FIELD_NUMBER: _ClassVar[int]
    SAMPLING_FIELD_NUMBER: _ClassVar[int]
    KEPT_FIELD_NUMBER: _ClassVar[int]
    REDACTED_FIELD_NUMBER: _ClassVar[int]
    BODY_FIELD_NUMBER: _ClassVar[int]
    PASSED_FIELD_NUMBER: _ClassVar[int]
    refused: str
    sampling: SpanSampling
    kept: bool
    redacted: _containers.RepeatedCompositeFieldContainer[_anyvalue_pb2.KeyValue]
    body: _anyvalue_pb2.AnyValue
    passed: _containers.RepeatedScalarFieldContainer[int]
    def __init__(self, refused: _Optional[str] = ..., sampling: _Optional[_Union[SpanSampling, _Mapping]] = ..., kept: bool = ..., redacted: _Optional[_Iterable[_Union[_anyvalue_pb2.KeyValue, _Mapping]]] = ..., body: _Optional[_Union[_anyvalue_pb2.AnyValue, _Mapping]] = ..., passed: _Optional[_Iterable[int]] = ...) -> None: ...

class SpanSampling(_message.Message):
    __slots__ = ("how", "parent_sampled", "rate", "rule")
    class How(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
        __slots__ = ()
        FOLLOW_PARENT: _ClassVar[SpanSampling.How]
        RATE: _ClassVar[SpanSampling.How]
        APP_SAMPLER: _ClassVar[SpanSampling.How]
    FOLLOW_PARENT: SpanSampling.How
    RATE: SpanSampling.How
    APP_SAMPLER: SpanSampling.How
    HOW_FIELD_NUMBER: _ClassVar[int]
    PARENT_SAMPLED_FIELD_NUMBER: _ClassVar[int]
    RATE_FIELD_NUMBER: _ClassVar[int]
    RULE_FIELD_NUMBER: _ClassVar[int]
    how: SpanSampling.How
    parent_sampled: bool
    rate: float
    rule: int
    def __init__(self, how: _Optional[_Union[SpanSampling.How, str]] = ..., parent_sampled: bool = ..., rate: _Optional[float] = ..., rule: _Optional[int] = ...) -> None: ...

class Request(_message.Message):
    __slots__ = ("describe", "evaluate", "components", "exporters", "agent_start", "agent_step")
    DESCRIBE_FIELD_NUMBER: _ClassVar[int]
    EVALUATE_FIELD_NUMBER: _ClassVar[int]
    COMPONENTS_FIELD_NUMBER: _ClassVar[int]
    EXPORTERS_FIELD_NUMBER: _ClassVar[int]
    AGENT_START_FIELD_NUMBER: _ClassVar[int]
    AGENT_STEP_FIELD_NUMBER: _ClassVar[int]
    describe: Describe
    evaluate: Evaluate
    components: Components
    exporters: Exporters
    agent_start: AgentStart
    agent_step: AgentStep
    def __init__(self, describe: _Optional[_Union[Describe, _Mapping]] = ..., evaluate: _Optional[_Union[Evaluate, _Mapping]] = ..., components: _Optional[_Union[Components, _Mapping]] = ..., exporters: _Optional[_Union[Exporters, _Mapping]] = ..., agent_start: _Optional[_Union[AgentStart, _Mapping]] = ..., agent_step: _Optional[_Union[AgentStep, _Mapping]] = ...) -> None: ...

class Reply(_message.Message):
    __slots__ = ("description", "error", "evaluated", "available_components", "exporters_decided", "agent_output")
    DESCRIPTION_FIELD_NUMBER: _ClassVar[int]
    ERROR_FIELD_NUMBER: _ClassVar[int]
    EVALUATED_FIELD_NUMBER: _ClassVar[int]
    AVAILABLE_COMPONENTS_FIELD_NUMBER: _ClassVar[int]
    EXPORTERS_DECIDED_FIELD_NUMBER: _ClassVar[int]
    AGENT_OUTPUT_FIELD_NUMBER: _ClassVar[int]
    description: _opamp_pb2.AgentDescription
    error: str
    evaluated: Evaluated
    available_components: _opamp_pb2.AvailableComponents
    exporters_decided: ExportersDecided
    agent_output: AgentOutput
    def __init__(self, description: _Optional[_Union[_opamp_pb2.AgentDescription, _Mapping]] = ..., error: _Optional[str] = ..., evaluated: _Optional[_Union[Evaluated, _Mapping]] = ..., available_components: _Optional[_Union[_opamp_pb2.AvailableComponents, _Mapping]] = ..., exporters_decided: _Optional[_Union[ExportersDecided, _Mapping]] = ..., agent_output: _Optional[_Union[AgentOutput, _Mapping]] = ...) -> None: ...
