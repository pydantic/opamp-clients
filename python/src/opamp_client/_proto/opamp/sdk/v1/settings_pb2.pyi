from opamp_client._proto.opamp.v1 import anyvalue_pb2 as _anyvalue_pb2
from opamp_client._proto.opamp.v1 import opamp_pb2 as _opamp_pb2
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
    __slots__ = ("sampling_rate", "send_traces", "send_metrics", "send_logs", "send_logs_min_severity", "sampling_rules", "send_logs_scope_levels", "drop_attributes", "mask_attributes", "sampling_traces_per_second", "send_logs_per_second", "deactivate_instrumentations", "deactivate_all_instrumentations", "opamp_polling_interval_ms", "custom")
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
    OPAMP_POLLING_INTERVAL_MS_FIELD_NUMBER: _ClassVar[int]
    CUSTOM_FIELD_NUMBER: _ClassVar[int]
    sampling_rate: float
    send_traces: bool
    send_metrics: bool
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
    opamp_polling_interval_ms: int
    custom: _containers.ScalarMap[str, str]
    def __init__(self, sampling_rate: _Optional[float] = ..., send_traces: bool = ..., send_metrics: bool = ..., send_logs: bool = ..., send_logs_min_severity: _Optional[int] = ..., sampling_rules: _Optional[_Iterable[_Union[SamplingRule, _Mapping]]] = ..., send_logs_scope_levels: _Optional[_Iterable[_Union[ScopeLevel, _Mapping]]] = ..., drop_attributes: _Optional[_Iterable[str]] = ..., mask_attributes: _Optional[_Iterable[str]] = ..., sampling_traces_per_second: _Optional[float] = ..., send_logs_per_second: _Optional[float] = ..., deactivate_instrumentations: _Optional[_Union[Names, _Mapping]] = ..., deactivate_all_instrumentations: bool = ..., opamp_polling_interval_ms: _Optional[int] = ..., custom: _Optional[_Mapping[str, str]] = ...) -> None: ...

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

class Load(_message.Message):
    __slots__ = ("saved", "host", "now_unix_ms", "core_state")
    SAVED_FIELD_NUMBER: _ClassVar[int]
    HOST_FIELD_NUMBER: _ClassVar[int]
    NOW_UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    CORE_STATE_FIELD_NUMBER: _ClassVar[int]
    saved: bytes
    host: Host
    now_unix_ms: int
    core_state: bytes
    def __init__(self, saved: _Optional[bytes] = ..., host: _Optional[_Union[Host, _Mapping]] = ..., now_unix_ms: _Optional[int] = ..., core_state: _Optional[bytes] = ...) -> None: ...

class Loaded(_message.Message):
    __slots__ = ("settings", "current", "effective", "warning", "refresh_at_unix_ms")
    SETTINGS_FIELD_NUMBER: _ClassVar[int]
    CURRENT_FIELD_NUMBER: _ClassVar[int]
    EFFECTIVE_FIELD_NUMBER: _ClassVar[int]
    WARNING_FIELD_NUMBER: _ClassVar[int]
    REFRESH_AT_UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    settings: Settings
    current: bytes
    effective: _opamp_pb2.EffectiveConfig
    warning: str
    refresh_at_unix_ms: int
    def __init__(self, settings: _Optional[_Union[Settings, _Mapping]] = ..., current: _Optional[bytes] = ..., effective: _Optional[_Union[_opamp_pb2.EffectiveConfig, _Mapping]] = ..., warning: _Optional[str] = ..., refresh_at_unix_ms: _Optional[int] = ...) -> None: ...

class Decide(_message.Message):
    __slots__ = ("config", "current", "at_start", "host", "now_unix_ms")
    CONFIG_FIELD_NUMBER: _ClassVar[int]
    CURRENT_FIELD_NUMBER: _ClassVar[int]
    AT_START_FIELD_NUMBER: _ClassVar[int]
    HOST_FIELD_NUMBER: _ClassVar[int]
    NOW_UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    config: _opamp_pb2.AgentRemoteConfig
    current: bytes
    at_start: bytes
    host: Host
    now_unix_ms: int
    def __init__(self, config: _Optional[_Union[_opamp_pb2.AgentRemoteConfig, _Mapping]] = ..., current: _Optional[bytes] = ..., at_start: _Optional[bytes] = ..., host: _Optional[_Union[Host, _Mapping]] = ..., now_unix_ms: _Optional[int] = ...) -> None: ...

class Plan(_message.Message):
    __slots__ = ("save",)
    SAVE_FIELD_NUMBER: _ClassVar[int]
    save: bytes
    def __init__(self, save: _Optional[bytes] = ...) -> None: ...

class Commit(_message.Message):
    __slots__ = ("decide", "saved", "error")
    class Saved(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
        __slots__ = ()
        SAVED_NOWHERE: _ClassVar[Commit.Saved]
        SAVED_DONE: _ClassVar[Commit.Saved]
        SAVED_FAILED: _ClassVar[Commit.Saved]
    SAVED_NOWHERE: Commit.Saved
    SAVED_DONE: Commit.Saved
    SAVED_FAILED: Commit.Saved
    DECIDE_FIELD_NUMBER: _ClassVar[int]
    SAVED_FIELD_NUMBER: _ClassVar[int]
    ERROR_FIELD_NUMBER: _ClassVar[int]
    decide: Decide
    saved: Commit.Saved
    error: str
    def __init__(self, decide: _Optional[_Union[Decide, _Mapping]] = ..., saved: _Optional[_Union[Commit.Saved, str]] = ..., error: _Optional[str] = ...) -> None: ...

class Decision(_message.Message):
    __slots__ = ("status", "apply", "current", "effective", "pending_restart", "refresh_at_unix_ms")
    STATUS_FIELD_NUMBER: _ClassVar[int]
    APPLY_FIELD_NUMBER: _ClassVar[int]
    CURRENT_FIELD_NUMBER: _ClassVar[int]
    EFFECTIVE_FIELD_NUMBER: _ClassVar[int]
    PENDING_RESTART_FIELD_NUMBER: _ClassVar[int]
    REFRESH_AT_UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    status: _opamp_pb2.RemoteConfigStatus
    apply: Settings
    current: bytes
    effective: _opamp_pb2.EffectiveConfig
    pending_restart: _containers.RepeatedScalarFieldContainer[str]
    refresh_at_unix_ms: int
    def __init__(self, status: _Optional[_Union[_opamp_pb2.RemoteConfigStatus, _Mapping]] = ..., apply: _Optional[_Union[Settings, _Mapping]] = ..., current: _Optional[bytes] = ..., effective: _Optional[_Union[_opamp_pb2.EffectiveConfig, _Mapping]] = ..., pending_restart: _Optional[_Iterable[str]] = ..., refresh_at_unix_ms: _Optional[int] = ...) -> None: ...

class Refresh(_message.Message):
    __slots__ = ("current", "at_start", "host", "now_unix_ms")
    CURRENT_FIELD_NUMBER: _ClassVar[int]
    AT_START_FIELD_NUMBER: _ClassVar[int]
    HOST_FIELD_NUMBER: _ClassVar[int]
    NOW_UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    current: bytes
    at_start: bytes
    host: Host
    now_unix_ms: int
    def __init__(self, current: _Optional[bytes] = ..., at_start: _Optional[bytes] = ..., host: _Optional[_Union[Host, _Mapping]] = ..., now_unix_ms: _Optional[int] = ...) -> None: ...

class Refreshed(_message.Message):
    __slots__ = ("settings", "effective", "refresh_at_unix_ms", "warning")
    SETTINGS_FIELD_NUMBER: _ClassVar[int]
    EFFECTIVE_FIELD_NUMBER: _ClassVar[int]
    REFRESH_AT_UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    WARNING_FIELD_NUMBER: _ClassVar[int]
    settings: Settings
    effective: _opamp_pb2.EffectiveConfig
    refresh_at_unix_ms: int
    warning: str
    def __init__(self, settings: _Optional[_Union[Settings, _Mapping]] = ..., effective: _Optional[_Union[_opamp_pb2.EffectiveConfig, _Mapping]] = ..., refresh_at_unix_ms: _Optional[int] = ..., warning: _Optional[str] = ...) -> None: ...

class Health(_message.Message):
    __slots__ = ("counts", "previous", "now_unix_ms", "settings")
    COUNTS_FIELD_NUMBER: _ClassVar[int]
    PREVIOUS_FIELD_NUMBER: _ClassVar[int]
    NOW_UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    SETTINGS_FIELD_NUMBER: _ClassVar[int]
    counts: ExportCounts
    previous: HealthReport
    now_unix_ms: int
    settings: Settings
    def __init__(self, counts: _Optional[_Union[ExportCounts, _Mapping]] = ..., previous: _Optional[_Union[HealthReport, _Mapping]] = ..., now_unix_ms: _Optional[int] = ..., settings: _Optional[_Union[Settings, _Mapping]] = ...) -> None: ...

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
    __slots__ = ("signal", "name", "gated")
    SIGNAL_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    GATED_FIELD_NUMBER: _ClassVar[int]
    signal: str
    name: str
    gated: bool
    def __init__(self, signal: _Optional[str] = ..., name: _Optional[str] = ..., gated: bool = ...) -> None: ...

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
    __slots__ = ("attributes",)
    ATTRIBUTES_FIELD_NUMBER: _ClassVar[int]
    attributes: _containers.RepeatedCompositeFieldContainer[_anyvalue_pb2.KeyValue]
    def __init__(self, attributes: _Optional[_Iterable[_Union[_anyvalue_pb2.KeyValue, _Mapping]]] = ...) -> None: ...

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
    __slots__ = ("refused", "sampling", "kept", "redacted", "passed")
    REFUSED_FIELD_NUMBER: _ClassVar[int]
    SAMPLING_FIELD_NUMBER: _ClassVar[int]
    KEPT_FIELD_NUMBER: _ClassVar[int]
    REDACTED_FIELD_NUMBER: _ClassVar[int]
    PASSED_FIELD_NUMBER: _ClassVar[int]
    refused: str
    sampling: SpanSampling
    kept: bool
    redacted: _containers.RepeatedCompositeFieldContainer[_anyvalue_pb2.KeyValue]
    passed: _containers.RepeatedScalarFieldContainer[int]
    def __init__(self, refused: _Optional[str] = ..., sampling: _Optional[_Union[SpanSampling, _Mapping]] = ..., kept: bool = ..., redacted: _Optional[_Iterable[_Union[_anyvalue_pb2.KeyValue, _Mapping]]] = ..., passed: _Optional[_Iterable[int]] = ...) -> None: ...

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
    __slots__ = ("load", "decide", "describe", "evaluate", "refresh", "health", "components", "commit")
    LOAD_FIELD_NUMBER: _ClassVar[int]
    DECIDE_FIELD_NUMBER: _ClassVar[int]
    DESCRIBE_FIELD_NUMBER: _ClassVar[int]
    EVALUATE_FIELD_NUMBER: _ClassVar[int]
    REFRESH_FIELD_NUMBER: _ClassVar[int]
    HEALTH_FIELD_NUMBER: _ClassVar[int]
    COMPONENTS_FIELD_NUMBER: _ClassVar[int]
    COMMIT_FIELD_NUMBER: _ClassVar[int]
    load: Load
    decide: Decide
    describe: Describe
    evaluate: Evaluate
    refresh: Refresh
    health: Health
    components: Components
    commit: Commit
    def __init__(self, load: _Optional[_Union[Load, _Mapping]] = ..., decide: _Optional[_Union[Decide, _Mapping]] = ..., describe: _Optional[_Union[Describe, _Mapping]] = ..., evaluate: _Optional[_Union[Evaluate, _Mapping]] = ..., refresh: _Optional[_Union[Refresh, _Mapping]] = ..., health: _Optional[_Union[Health, _Mapping]] = ..., components: _Optional[_Union[Components, _Mapping]] = ..., commit: _Optional[_Union[Commit, _Mapping]] = ...) -> None: ...

class Reply(_message.Message):
    __slots__ = ("loaded", "decision", "description", "error", "evaluated", "refreshed", "health_report", "available_components", "plan")
    LOADED_FIELD_NUMBER: _ClassVar[int]
    DECISION_FIELD_NUMBER: _ClassVar[int]
    DESCRIPTION_FIELD_NUMBER: _ClassVar[int]
    ERROR_FIELD_NUMBER: _ClassVar[int]
    EVALUATED_FIELD_NUMBER: _ClassVar[int]
    REFRESHED_FIELD_NUMBER: _ClassVar[int]
    HEALTH_REPORT_FIELD_NUMBER: _ClassVar[int]
    AVAILABLE_COMPONENTS_FIELD_NUMBER: _ClassVar[int]
    PLAN_FIELD_NUMBER: _ClassVar[int]
    loaded: Loaded
    decision: Decision
    description: _opamp_pb2.AgentDescription
    error: str
    evaluated: Evaluated
    refreshed: Refreshed
    health_report: HealthReport
    available_components: _opamp_pb2.AvailableComponents
    plan: Plan
    def __init__(self, loaded: _Optional[_Union[Loaded, _Mapping]] = ..., decision: _Optional[_Union[Decision, _Mapping]] = ..., description: _Optional[_Union[_opamp_pb2.AgentDescription, _Mapping]] = ..., error: _Optional[str] = ..., evaluated: _Optional[_Union[Evaluated, _Mapping]] = ..., refreshed: _Optional[_Union[Refreshed, _Mapping]] = ..., health_report: _Optional[_Union[HealthReport, _Mapping]] = ..., available_components: _Optional[_Union[_opamp_pb2.AvailableComponents, _Mapping]] = ..., plan: _Optional[_Union[Plan, _Mapping]] = ...) -> None: ...
