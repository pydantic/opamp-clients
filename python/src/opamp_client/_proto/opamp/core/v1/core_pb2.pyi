from opamp_client._proto.opamp.v1 import opamp_pb2 as _opamp_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class StatusField(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    STATUS_FIELD_DESCRIPTION: _ClassVar[StatusField]
    STATUS_FIELD_HEALTH: _ClassVar[StatusField]
    STATUS_FIELD_EFFECTIVE_CONFIG: _ClassVar[StatusField]
    STATUS_FIELD_REMOTE_CONFIG_STATUS: _ClassVar[StatusField]
    STATUS_FIELD_CONNECTION_SETTINGS_STATUS: _ClassVar[StatusField]
    STATUS_FIELD_AVAILABLE_COMPONENTS: _ClassVar[StatusField]
    STATUS_FIELD_CUSTOM_CAPABILITIES: _ClassVar[StatusField]
    STATUS_FIELD_PACKAGE_STATUSES: _ClassVar[StatusField]

class SetupOs(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    SETUP_OS_UNIX: _ClassVar[SetupOs]
    SETUP_OS_MACOS: _ClassVar[SetupOs]
    SETUP_OS_WINDOWS: _ClassVar[SetupOs]

class SetupUidFrom(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    SETUP_UID_FROM_MADE: _ClassVar[SetupUidFrom]
    SETUP_UID_FROM_SAVED: _ClassVar[SetupUidFrom]
    SETUP_UID_FROM_GIVEN: _ClassVar[SetupUidFrom]
STATUS_FIELD_DESCRIPTION: StatusField
STATUS_FIELD_HEALTH: StatusField
STATUS_FIELD_EFFECTIVE_CONFIG: StatusField
STATUS_FIELD_REMOTE_CONFIG_STATUS: StatusField
STATUS_FIELD_CONNECTION_SETTINGS_STATUS: StatusField
STATUS_FIELD_AVAILABLE_COMPONENTS: StatusField
STATUS_FIELD_CUSTOM_CAPABILITIES: StatusField
STATUS_FIELD_PACKAGE_STATUSES: StatusField
SETUP_OS_UNIX: SetupOs
SETUP_OS_MACOS: SetupOs
SETUP_OS_WINDOWS: SetupOs
SETUP_UID_FROM_MADE: SetupUidFrom
SETUP_UID_FROM_SAVED: SetupUidFrom
SETUP_UID_FROM_GIVEN: SetupUidFrom

class Config(_message.Message):
    __slots__ = ("instance_uid", "capabilities", "heartbeat_interval_ms", "backoff_initial_ms", "backoff_max_ms", "max_send_bytes", "seed", "endpoint", "headers", "request_timeout_ms", "max_receive_bytes", "persistence", "saved_state", "accept_connection_settings", "verify_timeout_ms", "user_agent", "accept_packages", "max_package_bytes", "download_timeout_ms", "request_instance_uid", "streams", "entropy", "splice_downloads", "unix_ms", "stop_timeout_ms", "connect_timeout_ms", "proxy")
    INSTANCE_UID_FIELD_NUMBER: _ClassVar[int]
    CAPABILITIES_FIELD_NUMBER: _ClassVar[int]
    HEARTBEAT_INTERVAL_MS_FIELD_NUMBER: _ClassVar[int]
    BACKOFF_INITIAL_MS_FIELD_NUMBER: _ClassVar[int]
    BACKOFF_MAX_MS_FIELD_NUMBER: _ClassVar[int]
    MAX_SEND_BYTES_FIELD_NUMBER: _ClassVar[int]
    SEED_FIELD_NUMBER: _ClassVar[int]
    ENDPOINT_FIELD_NUMBER: _ClassVar[int]
    HEADERS_FIELD_NUMBER: _ClassVar[int]
    REQUEST_TIMEOUT_MS_FIELD_NUMBER: _ClassVar[int]
    MAX_RECEIVE_BYTES_FIELD_NUMBER: _ClassVar[int]
    PERSISTENCE_FIELD_NUMBER: _ClassVar[int]
    SAVED_STATE_FIELD_NUMBER: _ClassVar[int]
    ACCEPT_CONNECTION_SETTINGS_FIELD_NUMBER: _ClassVar[int]
    VERIFY_TIMEOUT_MS_FIELD_NUMBER: _ClassVar[int]
    USER_AGENT_FIELD_NUMBER: _ClassVar[int]
    ACCEPT_PACKAGES_FIELD_NUMBER: _ClassVar[int]
    MAX_PACKAGE_BYTES_FIELD_NUMBER: _ClassVar[int]
    DOWNLOAD_TIMEOUT_MS_FIELD_NUMBER: _ClassVar[int]
    REQUEST_INSTANCE_UID_FIELD_NUMBER: _ClassVar[int]
    STREAMS_FIELD_NUMBER: _ClassVar[int]
    ENTROPY_FIELD_NUMBER: _ClassVar[int]
    SPLICE_DOWNLOADS_FIELD_NUMBER: _ClassVar[int]
    UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    STOP_TIMEOUT_MS_FIELD_NUMBER: _ClassVar[int]
    CONNECT_TIMEOUT_MS_FIELD_NUMBER: _ClassVar[int]
    PROXY_FIELD_NUMBER: _ClassVar[int]
    instance_uid: bytes
    capabilities: int
    heartbeat_interval_ms: int
    backoff_initial_ms: int
    backoff_max_ms: int
    max_send_bytes: int
    seed: int
    endpoint: str
    headers: _containers.RepeatedCompositeFieldContainer[Header]
    request_timeout_ms: int
    max_receive_bytes: int
    persistence: bool
    saved_state: bytes
    accept_connection_settings: bool
    verify_timeout_ms: int
    user_agent: str
    accept_packages: bool
    max_package_bytes: int
    download_timeout_ms: int
    request_instance_uid: bool
    streams: bool
    entropy: bytes
    splice_downloads: bool
    unix_ms: int
    stop_timeout_ms: int
    connect_timeout_ms: int
    proxy: ProxyChoice
    def __init__(self, instance_uid: _Optional[bytes] = ..., capabilities: _Optional[int] = ..., heartbeat_interval_ms: _Optional[int] = ..., backoff_initial_ms: _Optional[int] = ..., backoff_max_ms: _Optional[int] = ..., max_send_bytes: _Optional[int] = ..., seed: _Optional[int] = ..., endpoint: _Optional[str] = ..., headers: _Optional[_Iterable[_Union[Header, _Mapping]]] = ..., request_timeout_ms: _Optional[int] = ..., max_receive_bytes: _Optional[int] = ..., persistence: bool = ..., saved_state: _Optional[bytes] = ..., accept_connection_settings: bool = ..., verify_timeout_ms: _Optional[int] = ..., user_agent: _Optional[str] = ..., accept_packages: bool = ..., max_package_bytes: _Optional[int] = ..., download_timeout_ms: _Optional[int] = ..., request_instance_uid: bool = ..., streams: bool = ..., entropy: _Optional[bytes] = ..., splice_downloads: bool = ..., unix_ms: _Optional[int] = ..., stop_timeout_ms: _Optional[int] = ..., connect_timeout_ms: _Optional[int] = ..., proxy: _Optional[_Union[ProxyChoice, _Mapping]] = ...) -> None: ...

class ProxyChoice(_message.Message):
    __slots__ = ("direct", "url", "headers", "resolve_locally", "environment")
    DIRECT_FIELD_NUMBER: _ClassVar[int]
    URL_FIELD_NUMBER: _ClassVar[int]
    HEADERS_FIELD_NUMBER: _ClassVar[int]
    RESOLVE_LOCALLY_FIELD_NUMBER: _ClassVar[int]
    ENVIRONMENT_FIELD_NUMBER: _ClassVar[int]
    direct: bool
    url: str
    headers: _containers.RepeatedCompositeFieldContainer[Header]
    resolve_locally: bool
    environment: _containers.RepeatedCompositeFieldContainer[Header]
    def __init__(self, direct: bool = ..., url: _Optional[str] = ..., headers: _Optional[_Iterable[_Union[Header, _Mapping]]] = ..., resolve_locally: bool = ..., environment: _Optional[_Iterable[_Union[Header, _Mapping]]] = ...) -> None: ...

class Origin(_message.Message):
    __slots__ = ("scheme", "host", "port")
    SCHEME_FIELD_NUMBER: _ClassVar[int]
    HOST_FIELD_NUMBER: _ClassVar[int]
    PORT_FIELD_NUMBER: _ClassVar[int]
    scheme: str
    host: str
    port: int
    def __init__(self, scheme: _Optional[str] = ..., host: _Optional[str] = ..., port: _Optional[int] = ...) -> None: ...

class Header(_message.Message):
    __slots__ = ("name", "value")
    NAME_FIELD_NUMBER: _ClassVar[int]
    VALUE_FIELD_NUMBER: _ClassVar[int]
    name: str
    value: str
    def __init__(self, name: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...

class Headers(_message.Message):
    __slots__ = ("headers",)
    HEADERS_FIELD_NUMBER: _ClassVar[int]
    headers: _containers.RepeatedCompositeFieldContainer[Header]
    def __init__(self, headers: _Optional[_Iterable[_Union[Header, _Mapping]]] = ...) -> None: ...

class Empty(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class Detail(_message.Message):
    __slots__ = ("detail",)
    DETAIL_FIELD_NUMBER: _ClassVar[int]
    detail: str
    def __init__(self, detail: _Optional[str] = ...) -> None: ...

class Input(_message.Message):
    __slots__ = ("start", "set_description", "set_health", "set_effective_config", "set_remote_config_status", "stop", "timer", "http_response", "http_failed", "ws_opened", "ws_failed", "ws_message", "ws_closed", "ws_lost", "stored", "store_failed", "set_connection_settings_status", "set_available_components", "set_custom_capabilities", "send_custom_message", "package_installed", "package_removed", "downloaded", "download_failed", "proxy_answer", "stream_resolved", "stream_opened", "stream_tls_ready", "stream_data", "stream_eof", "stream_failed", "stream_spliced", "download_response", "set_auth_headers", "abort")
    START_FIELD_NUMBER: _ClassVar[int]
    SET_DESCRIPTION_FIELD_NUMBER: _ClassVar[int]
    SET_HEALTH_FIELD_NUMBER: _ClassVar[int]
    SET_EFFECTIVE_CONFIG_FIELD_NUMBER: _ClassVar[int]
    SET_REMOTE_CONFIG_STATUS_FIELD_NUMBER: _ClassVar[int]
    STOP_FIELD_NUMBER: _ClassVar[int]
    TIMER_FIELD_NUMBER: _ClassVar[int]
    HTTP_RESPONSE_FIELD_NUMBER: _ClassVar[int]
    HTTP_FAILED_FIELD_NUMBER: _ClassVar[int]
    WS_OPENED_FIELD_NUMBER: _ClassVar[int]
    WS_FAILED_FIELD_NUMBER: _ClassVar[int]
    WS_MESSAGE_FIELD_NUMBER: _ClassVar[int]
    WS_CLOSED_FIELD_NUMBER: _ClassVar[int]
    WS_LOST_FIELD_NUMBER: _ClassVar[int]
    STORED_FIELD_NUMBER: _ClassVar[int]
    STORE_FAILED_FIELD_NUMBER: _ClassVar[int]
    SET_CONNECTION_SETTINGS_STATUS_FIELD_NUMBER: _ClassVar[int]
    SET_AVAILABLE_COMPONENTS_FIELD_NUMBER: _ClassVar[int]
    SET_CUSTOM_CAPABILITIES_FIELD_NUMBER: _ClassVar[int]
    SEND_CUSTOM_MESSAGE_FIELD_NUMBER: _ClassVar[int]
    PACKAGE_INSTALLED_FIELD_NUMBER: _ClassVar[int]
    PACKAGE_REMOVED_FIELD_NUMBER: _ClassVar[int]
    DOWNLOADED_FIELD_NUMBER: _ClassVar[int]
    DOWNLOAD_FAILED_FIELD_NUMBER: _ClassVar[int]
    PROXY_ANSWER_FIELD_NUMBER: _ClassVar[int]
    STREAM_RESOLVED_FIELD_NUMBER: _ClassVar[int]
    STREAM_OPENED_FIELD_NUMBER: _ClassVar[int]
    STREAM_TLS_READY_FIELD_NUMBER: _ClassVar[int]
    STREAM_DATA_FIELD_NUMBER: _ClassVar[int]
    STREAM_EOF_FIELD_NUMBER: _ClassVar[int]
    STREAM_FAILED_FIELD_NUMBER: _ClassVar[int]
    STREAM_SPLICED_FIELD_NUMBER: _ClassVar[int]
    DOWNLOAD_RESPONSE_FIELD_NUMBER: _ClassVar[int]
    SET_AUTH_HEADERS_FIELD_NUMBER: _ClassVar[int]
    ABORT_FIELD_NUMBER: _ClassVar[int]
    start: Empty
    set_description: _opamp_pb2.AgentDescription
    set_health: _opamp_pb2.ComponentHealth
    set_effective_config: _opamp_pb2.EffectiveConfig
    set_remote_config_status: _opamp_pb2.RemoteConfigStatus
    stop: Empty
    timer: Empty
    http_response: HttpResponse
    http_failed: Failed
    ws_opened: Id
    ws_failed: WsFailed
    ws_message: WsMessage
    ws_closed: WsClosed
    ws_lost: Failed
    stored: Id
    store_failed: Failed
    set_connection_settings_status: _opamp_pb2.ConnectionSettingsStatus
    set_available_components: _opamp_pb2.AvailableComponents
    set_custom_capabilities: _opamp_pb2.CustomCapabilities
    send_custom_message: _opamp_pb2.CustomMessage
    package_installed: PackageInstalled
    package_removed: Name
    downloaded: Downloaded
    download_failed: Failed
    proxy_answer: ProxyAnswer
    stream_resolved: StreamResolved
    stream_opened: Id
    stream_tls_ready: Id
    stream_data: WsMessage
    stream_eof: Id
    stream_failed: Failed
    stream_spliced: Id
    download_response: DownloadResponse
    set_auth_headers: Headers
    abort: Empty
    def __init__(self, start: _Optional[_Union[Empty, _Mapping]] = ..., set_description: _Optional[_Union[_opamp_pb2.AgentDescription, _Mapping]] = ..., set_health: _Optional[_Union[_opamp_pb2.ComponentHealth, _Mapping]] = ..., set_effective_config: _Optional[_Union[_opamp_pb2.EffectiveConfig, _Mapping]] = ..., set_remote_config_status: _Optional[_Union[_opamp_pb2.RemoteConfigStatus, _Mapping]] = ..., stop: _Optional[_Union[Empty, _Mapping]] = ..., timer: _Optional[_Union[Empty, _Mapping]] = ..., http_response: _Optional[_Union[HttpResponse, _Mapping]] = ..., http_failed: _Optional[_Union[Failed, _Mapping]] = ..., ws_opened: _Optional[_Union[Id, _Mapping]] = ..., ws_failed: _Optional[_Union[WsFailed, _Mapping]] = ..., ws_message: _Optional[_Union[WsMessage, _Mapping]] = ..., ws_closed: _Optional[_Union[WsClosed, _Mapping]] = ..., ws_lost: _Optional[_Union[Failed, _Mapping]] = ..., stored: _Optional[_Union[Id, _Mapping]] = ..., store_failed: _Optional[_Union[Failed, _Mapping]] = ..., set_connection_settings_status: _Optional[_Union[_opamp_pb2.ConnectionSettingsStatus, _Mapping]] = ..., set_available_components: _Optional[_Union[_opamp_pb2.AvailableComponents, _Mapping]] = ..., set_custom_capabilities: _Optional[_Union[_opamp_pb2.CustomCapabilities, _Mapping]] = ..., send_custom_message: _Optional[_Union[_opamp_pb2.CustomMessage, _Mapping]] = ..., package_installed: _Optional[_Union[PackageInstalled, _Mapping]] = ..., package_removed: _Optional[_Union[Name, _Mapping]] = ..., downloaded: _Optional[_Union[Downloaded, _Mapping]] = ..., download_failed: _Optional[_Union[Failed, _Mapping]] = ..., proxy_answer: _Optional[_Union[ProxyAnswer, _Mapping]] = ..., stream_resolved: _Optional[_Union[StreamResolved, _Mapping]] = ..., stream_opened: _Optional[_Union[Id, _Mapping]] = ..., stream_tls_ready: _Optional[_Union[Id, _Mapping]] = ..., stream_data: _Optional[_Union[WsMessage, _Mapping]] = ..., stream_eof: _Optional[_Union[Id, _Mapping]] = ..., stream_failed: _Optional[_Union[Failed, _Mapping]] = ..., stream_spliced: _Optional[_Union[Id, _Mapping]] = ..., download_response: _Optional[_Union[DownloadResponse, _Mapping]] = ..., set_auth_headers: _Optional[_Union[Headers, _Mapping]] = ..., abort: _Optional[_Union[Empty, _Mapping]] = ...) -> None: ...

class ProxyAnswer(_message.Message):
    __slots__ = ("id", "proxy", "headers", "resolve_locally")
    ID_FIELD_NUMBER: _ClassVar[int]
    PROXY_FIELD_NUMBER: _ClassVar[int]
    HEADERS_FIELD_NUMBER: _ClassVar[int]
    RESOLVE_LOCALLY_FIELD_NUMBER: _ClassVar[int]
    id: int
    proxy: str
    headers: _containers.RepeatedCompositeFieldContainer[Header]
    resolve_locally: bool
    def __init__(self, id: _Optional[int] = ..., proxy: _Optional[str] = ..., headers: _Optional[_Iterable[_Union[Header, _Mapping]]] = ..., resolve_locally: bool = ...) -> None: ...

class StreamResolved(_message.Message):
    __slots__ = ("id", "addresses")
    ID_FIELD_NUMBER: _ClassVar[int]
    ADDRESSES_FIELD_NUMBER: _ClassVar[int]
    id: int
    addresses: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, id: _Optional[int] = ..., addresses: _Optional[_Iterable[str]] = ...) -> None: ...

class Name(_message.Message):
    __slots__ = ("name",)
    NAME_FIELD_NUMBER: _ClassVar[int]
    name: str
    def __init__(self, name: _Optional[str] = ...) -> None: ...

class PackageInstalled(_message.Message):
    __slots__ = ("name", "hash", "ok", "detail")
    NAME_FIELD_NUMBER: _ClassVar[int]
    HASH_FIELD_NUMBER: _ClassVar[int]
    OK_FIELD_NUMBER: _ClassVar[int]
    DETAIL_FIELD_NUMBER: _ClassVar[int]
    name: str
    hash: bytes
    ok: bool
    detail: str
    def __init__(self, name: _Optional[str] = ..., hash: _Optional[bytes] = ..., ok: bool = ..., detail: _Optional[str] = ...) -> None: ...

class Downloaded(_message.Message):
    __slots__ = ("id", "sha256")
    ID_FIELD_NUMBER: _ClassVar[int]
    SHA256_FIELD_NUMBER: _ClassVar[int]
    id: int
    sha256: bytes
    def __init__(self, id: _Optional[int] = ..., sha256: _Optional[bytes] = ...) -> None: ...

class DownloadResponse(_message.Message):
    __slots__ = ("id", "status", "sha256", "location", "location_origin")
    ID_FIELD_NUMBER: _ClassVar[int]
    STATUS_FIELD_NUMBER: _ClassVar[int]
    SHA256_FIELD_NUMBER: _ClassVar[int]
    LOCATION_FIELD_NUMBER: _ClassVar[int]
    LOCATION_ORIGIN_FIELD_NUMBER: _ClassVar[int]
    id: int
    status: int
    sha256: bytes
    location: str
    location_origin: Origin
    def __init__(self, id: _Optional[int] = ..., status: _Optional[int] = ..., sha256: _Optional[bytes] = ..., location: _Optional[str] = ..., location_origin: _Optional[_Union[Origin, _Mapping]] = ...) -> None: ...

class Id(_message.Message):
    __slots__ = ("id",)
    ID_FIELD_NUMBER: _ClassVar[int]
    id: int
    def __init__(self, id: _Optional[int] = ...) -> None: ...

class Failed(_message.Message):
    __slots__ = ("id", "detail")
    ID_FIELD_NUMBER: _ClassVar[int]
    DETAIL_FIELD_NUMBER: _ClassVar[int]
    id: int
    detail: str
    def __init__(self, id: _Optional[int] = ..., detail: _Optional[str] = ...) -> None: ...

class HttpResponse(_message.Message):
    __slots__ = ("id", "status", "headers", "body", "unix_ms")
    ID_FIELD_NUMBER: _ClassVar[int]
    STATUS_FIELD_NUMBER: _ClassVar[int]
    HEADERS_FIELD_NUMBER: _ClassVar[int]
    BODY_FIELD_NUMBER: _ClassVar[int]
    UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    id: int
    status: int
    headers: _containers.RepeatedCompositeFieldContainer[Header]
    body: bytes
    unix_ms: int
    def __init__(self, id: _Optional[int] = ..., status: _Optional[int] = ..., headers: _Optional[_Iterable[_Union[Header, _Mapping]]] = ..., body: _Optional[bytes] = ..., unix_ms: _Optional[int] = ...) -> None: ...

class WsFailed(_message.Message):
    __slots__ = ("id", "status", "headers", "detail", "unix_ms")
    ID_FIELD_NUMBER: _ClassVar[int]
    STATUS_FIELD_NUMBER: _ClassVar[int]
    HEADERS_FIELD_NUMBER: _ClassVar[int]
    DETAIL_FIELD_NUMBER: _ClassVar[int]
    UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    id: int
    status: int
    headers: _containers.RepeatedCompositeFieldContainer[Header]
    detail: str
    unix_ms: int
    def __init__(self, id: _Optional[int] = ..., status: _Optional[int] = ..., headers: _Optional[_Iterable[_Union[Header, _Mapping]]] = ..., detail: _Optional[str] = ..., unix_ms: _Optional[int] = ...) -> None: ...

class WsMessage(_message.Message):
    __slots__ = ("id", "data")
    ID_FIELD_NUMBER: _ClassVar[int]
    DATA_FIELD_NUMBER: _ClassVar[int]
    id: int
    data: bytes
    def __init__(self, id: _Optional[int] = ..., data: _Optional[bytes] = ...) -> None: ...

class WsClosed(_message.Message):
    __slots__ = ("id", "code")
    ID_FIELD_NUMBER: _ClassVar[int]
    CODE_FIELD_NUMBER: _ClassVar[int]
    id: int
    code: int
    def __init__(self, id: _Optional[int] = ..., code: _Optional[int] = ...) -> None: ...

class Step(_message.Message):
    __slots__ = ("now_ms", "input", "unix_ms")
    NOW_MS_FIELD_NUMBER: _ClassVar[int]
    INPUT_FIELD_NUMBER: _ClassVar[int]
    UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    now_ms: int
    input: Input
    unix_ms: int
    def __init__(self, now_ms: _Optional[int] = ..., input: _Optional[_Union[Input, _Mapping]] = ..., unix_ms: _Optional[int] = ...) -> None: ...

class Reply(_message.Message):
    __slots__ = ("output", "error")
    OUTPUT_FIELD_NUMBER: _ClassVar[int]
    ERROR_FIELD_NUMBER: _ClassVar[int]
    output: Output
    error: str
    def __init__(self, output: _Optional[_Union[Output, _Mapping]] = ..., error: _Optional[str] = ...) -> None: ...

class Output(_message.Message):
    __slots__ = ("effects", "wake_at_ms", "stopped", "instance_uid", "limits")
    EFFECTS_FIELD_NUMBER: _ClassVar[int]
    WAKE_AT_MS_FIELD_NUMBER: _ClassVar[int]
    STOPPED_FIELD_NUMBER: _ClassVar[int]
    INSTANCE_UID_FIELD_NUMBER: _ClassVar[int]
    LIMITS_FIELD_NUMBER: _ClassVar[int]
    effects: _containers.RepeatedCompositeFieldContainer[Effect]
    wake_at_ms: int
    stopped: bool
    instance_uid: bytes
    limits: Limits
    def __init__(self, effects: _Optional[_Iterable[_Union[Effect, _Mapping]]] = ..., wake_at_ms: _Optional[int] = ..., stopped: bool = ..., instance_uid: _Optional[bytes] = ..., limits: _Optional[_Union[Limits, _Mapping]] = ...) -> None: ...

class Limits(_message.Message):
    __slots__ = ("heartbeat_interval_ms", "backoff_initial_ms", "backoff_max_ms", "max_send_bytes", "request_timeout_ms", "max_receive_bytes", "verify_timeout_ms", "max_package_bytes", "download_timeout_ms", "stop_timeout_ms", "connect_timeout_ms")
    HEARTBEAT_INTERVAL_MS_FIELD_NUMBER: _ClassVar[int]
    BACKOFF_INITIAL_MS_FIELD_NUMBER: _ClassVar[int]
    BACKOFF_MAX_MS_FIELD_NUMBER: _ClassVar[int]
    MAX_SEND_BYTES_FIELD_NUMBER: _ClassVar[int]
    REQUEST_TIMEOUT_MS_FIELD_NUMBER: _ClassVar[int]
    MAX_RECEIVE_BYTES_FIELD_NUMBER: _ClassVar[int]
    VERIFY_TIMEOUT_MS_FIELD_NUMBER: _ClassVar[int]
    MAX_PACKAGE_BYTES_FIELD_NUMBER: _ClassVar[int]
    DOWNLOAD_TIMEOUT_MS_FIELD_NUMBER: _ClassVar[int]
    STOP_TIMEOUT_MS_FIELD_NUMBER: _ClassVar[int]
    CONNECT_TIMEOUT_MS_FIELD_NUMBER: _ClassVar[int]
    heartbeat_interval_ms: int
    backoff_initial_ms: int
    backoff_max_ms: int
    max_send_bytes: int
    request_timeout_ms: int
    max_receive_bytes: int
    verify_timeout_ms: int
    max_package_bytes: int
    download_timeout_ms: int
    stop_timeout_ms: int
    connect_timeout_ms: int
    def __init__(self, heartbeat_interval_ms: _Optional[int] = ..., backoff_initial_ms: _Optional[int] = ..., backoff_max_ms: _Optional[int] = ..., max_send_bytes: _Optional[int] = ..., request_timeout_ms: _Optional[int] = ..., max_receive_bytes: _Optional[int] = ..., verify_timeout_ms: _Optional[int] = ..., max_package_bytes: _Optional[int] = ..., download_timeout_ms: _Optional[int] = ..., stop_timeout_ms: _Optional[int] = ..., connect_timeout_ms: _Optional[int] = ...) -> None: ...

class Effect(_message.Message):
    __slots__ = ("http_request", "ws_open", "ws_send", "ws_close", "notify", "store", "download", "discard", "cancel", "proxy_query", "stream_resolve", "stream_open", "stream_start_tls", "stream_write", "stream_close", "download_data", "download_done", "stream_splice")
    HTTP_REQUEST_FIELD_NUMBER: _ClassVar[int]
    WS_OPEN_FIELD_NUMBER: _ClassVar[int]
    WS_SEND_FIELD_NUMBER: _ClassVar[int]
    WS_CLOSE_FIELD_NUMBER: _ClassVar[int]
    NOTIFY_FIELD_NUMBER: _ClassVar[int]
    STORE_FIELD_NUMBER: _ClassVar[int]
    DOWNLOAD_FIELD_NUMBER: _ClassVar[int]
    DISCARD_FIELD_NUMBER: _ClassVar[int]
    CANCEL_FIELD_NUMBER: _ClassVar[int]
    PROXY_QUERY_FIELD_NUMBER: _ClassVar[int]
    STREAM_RESOLVE_FIELD_NUMBER: _ClassVar[int]
    STREAM_OPEN_FIELD_NUMBER: _ClassVar[int]
    STREAM_START_TLS_FIELD_NUMBER: _ClassVar[int]
    STREAM_WRITE_FIELD_NUMBER: _ClassVar[int]
    STREAM_CLOSE_FIELD_NUMBER: _ClassVar[int]
    DOWNLOAD_DATA_FIELD_NUMBER: _ClassVar[int]
    DOWNLOAD_DONE_FIELD_NUMBER: _ClassVar[int]
    STREAM_SPLICE_FIELD_NUMBER: _ClassVar[int]
    http_request: HttpRequest
    ws_open: WsOpen
    ws_send: WsMessage
    ws_close: Id
    notify: Notification
    store: Store
    download: Download
    discard: Id
    cancel: Id
    proxy_query: Url
    stream_resolve: StreamName
    stream_open: StreamOpen
    stream_start_tls: StreamName
    stream_write: WsMessage
    stream_close: StreamClose
    download_data: WsMessage
    download_done: Id
    stream_splice: StreamSplice
    def __init__(self, http_request: _Optional[_Union[HttpRequest, _Mapping]] = ..., ws_open: _Optional[_Union[WsOpen, _Mapping]] = ..., ws_send: _Optional[_Union[WsMessage, _Mapping]] = ..., ws_close: _Optional[_Union[Id, _Mapping]] = ..., notify: _Optional[_Union[Notification, _Mapping]] = ..., store: _Optional[_Union[Store, _Mapping]] = ..., download: _Optional[_Union[Download, _Mapping]] = ..., discard: _Optional[_Union[Id, _Mapping]] = ..., cancel: _Optional[_Union[Id, _Mapping]] = ..., proxy_query: _Optional[_Union[Url, _Mapping]] = ..., stream_resolve: _Optional[_Union[StreamName, _Mapping]] = ..., stream_open: _Optional[_Union[StreamOpen, _Mapping]] = ..., stream_start_tls: _Optional[_Union[StreamName, _Mapping]] = ..., stream_write: _Optional[_Union[WsMessage, _Mapping]] = ..., stream_close: _Optional[_Union[StreamClose, _Mapping]] = ..., download_data: _Optional[_Union[WsMessage, _Mapping]] = ..., download_done: _Optional[_Union[Id, _Mapping]] = ..., stream_splice: _Optional[_Union[StreamSplice, _Mapping]] = ...) -> None: ...

class StreamClose(_message.Message):
    __slots__ = ("id", "abort")
    ID_FIELD_NUMBER: _ClassVar[int]
    ABORT_FIELD_NUMBER: _ClassVar[int]
    id: int
    abort: bool
    def __init__(self, id: _Optional[int] = ..., abort: bool = ...) -> None: ...

class Url(_message.Message):
    __slots__ = ("id", "url")
    ID_FIELD_NUMBER: _ClassVar[int]
    URL_FIELD_NUMBER: _ClassVar[int]
    id: int
    url: str
    def __init__(self, id: _Optional[int] = ..., url: _Optional[str] = ...) -> None: ...

class StreamName(_message.Message):
    __slots__ = ("id", "host")
    ID_FIELD_NUMBER: _ClassVar[int]
    HOST_FIELD_NUMBER: _ClassVar[int]
    id: int
    host: str
    def __init__(self, id: _Optional[int] = ..., host: _Optional[str] = ...) -> None: ...

class StreamSplice(_message.Message):
    __slots__ = ("id", "download", "bytes")
    ID_FIELD_NUMBER: _ClassVar[int]
    DOWNLOAD_FIELD_NUMBER: _ClassVar[int]
    BYTES_FIELD_NUMBER: _ClassVar[int]
    id: int
    download: int
    bytes: int
    def __init__(self, id: _Optional[int] = ..., download: _Optional[int] = ..., bytes: _Optional[int] = ...) -> None: ...

class StreamOpen(_message.Message):
    __slots__ = ("id", "host", "port")
    ID_FIELD_NUMBER: _ClassVar[int]
    HOST_FIELD_NUMBER: _ClassVar[int]
    PORT_FIELD_NUMBER: _ClassVar[int]
    id: int
    host: str
    port: int
    def __init__(self, id: _Optional[int] = ..., host: _Optional[str] = ..., port: _Optional[int] = ...) -> None: ...

class Download(_message.Message):
    __slots__ = ("id", "url", "headers", "max_bytes", "timeout_ms")
    ID_FIELD_NUMBER: _ClassVar[int]
    URL_FIELD_NUMBER: _ClassVar[int]
    HEADERS_FIELD_NUMBER: _ClassVar[int]
    MAX_BYTES_FIELD_NUMBER: _ClassVar[int]
    TIMEOUT_MS_FIELD_NUMBER: _ClassVar[int]
    id: int
    url: str
    headers: _containers.RepeatedCompositeFieldContainer[Header]
    max_bytes: int
    timeout_ms: int
    def __init__(self, id: _Optional[int] = ..., url: _Optional[str] = ..., headers: _Optional[_Iterable[_Union[Header, _Mapping]]] = ..., max_bytes: _Optional[int] = ..., timeout_ms: _Optional[int] = ...) -> None: ...

class Store(_message.Message):
    __slots__ = ("id", "data")
    ID_FIELD_NUMBER: _ClassVar[int]
    DATA_FIELD_NUMBER: _ClassVar[int]
    id: int
    data: bytes
    def __init__(self, id: _Optional[int] = ..., data: _Optional[bytes] = ...) -> None: ...

class HttpRequest(_message.Message):
    __slots__ = ("id", "url", "headers", "body", "timeout_ms", "max_response_bytes")
    ID_FIELD_NUMBER: _ClassVar[int]
    URL_FIELD_NUMBER: _ClassVar[int]
    HEADERS_FIELD_NUMBER: _ClassVar[int]
    BODY_FIELD_NUMBER: _ClassVar[int]
    TIMEOUT_MS_FIELD_NUMBER: _ClassVar[int]
    MAX_RESPONSE_BYTES_FIELD_NUMBER: _ClassVar[int]
    id: int
    url: str
    headers: _containers.RepeatedCompositeFieldContainer[Header]
    body: bytes
    timeout_ms: int
    max_response_bytes: int
    def __init__(self, id: _Optional[int] = ..., url: _Optional[str] = ..., headers: _Optional[_Iterable[_Union[Header, _Mapping]]] = ..., body: _Optional[bytes] = ..., timeout_ms: _Optional[int] = ..., max_response_bytes: _Optional[int] = ...) -> None: ...

class WsOpen(_message.Message):
    __slots__ = ("id", "url", "headers", "max_message_bytes", "timeout_ms")
    ID_FIELD_NUMBER: _ClassVar[int]
    URL_FIELD_NUMBER: _ClassVar[int]
    HEADERS_FIELD_NUMBER: _ClassVar[int]
    MAX_MESSAGE_BYTES_FIELD_NUMBER: _ClassVar[int]
    TIMEOUT_MS_FIELD_NUMBER: _ClassVar[int]
    id: int
    url: str
    headers: _containers.RepeatedCompositeFieldContainer[Header]
    max_message_bytes: int
    timeout_ms: int
    def __init__(self, id: _Optional[int] = ..., url: _Optional[str] = ..., headers: _Optional[_Iterable[_Union[Header, _Mapping]]] = ..., max_message_bytes: _Optional[int] = ..., timeout_ms: _Optional[int] = ...) -> None: ...

class Notification(_message.Message):
    __slots__ = ("connected", "connect_failed", "error_response", "remote_config", "command", "instance_uid_changed", "heartbeat_interval_ms", "status_too_large", "bad_message", "stopped", "connection_settings", "custom_message", "server_custom_capabilities", "custom_message_not_sent", "install_package", "remove_package", "save_stalled")
    CONNECTED_FIELD_NUMBER: _ClassVar[int]
    CONNECT_FAILED_FIELD_NUMBER: _ClassVar[int]
    ERROR_RESPONSE_FIELD_NUMBER: _ClassVar[int]
    REMOTE_CONFIG_FIELD_NUMBER: _ClassVar[int]
    COMMAND_FIELD_NUMBER: _ClassVar[int]
    INSTANCE_UID_CHANGED_FIELD_NUMBER: _ClassVar[int]
    HEARTBEAT_INTERVAL_MS_FIELD_NUMBER: _ClassVar[int]
    STATUS_TOO_LARGE_FIELD_NUMBER: _ClassVar[int]
    BAD_MESSAGE_FIELD_NUMBER: _ClassVar[int]
    STOPPED_FIELD_NUMBER: _ClassVar[int]
    CONNECTION_SETTINGS_FIELD_NUMBER: _ClassVar[int]
    CUSTOM_MESSAGE_FIELD_NUMBER: _ClassVar[int]
    SERVER_CUSTOM_CAPABILITIES_FIELD_NUMBER: _ClassVar[int]
    CUSTOM_MESSAGE_NOT_SENT_FIELD_NUMBER: _ClassVar[int]
    INSTALL_PACKAGE_FIELD_NUMBER: _ClassVar[int]
    REMOVE_PACKAGE_FIELD_NUMBER: _ClassVar[int]
    SAVE_STALLED_FIELD_NUMBER: _ClassVar[int]
    connected: Empty
    connect_failed: NotifyConnectFailed
    error_response: ErrorResponse
    remote_config: _opamp_pb2.AgentRemoteConfig
    command: _opamp_pb2.ServerToAgentCommand
    instance_uid_changed: bytes
    heartbeat_interval_ms: int
    status_too_large: StatusTooLarge
    bad_message: Detail
    stopped: Empty
    connection_settings: _opamp_pb2.ConnectionSettingsOffers
    custom_message: _opamp_pb2.CustomMessage
    server_custom_capabilities: _opamp_pb2.CustomCapabilities
    custom_message_not_sent: CustomMessageNotSent
    install_package: InstallPackage
    remove_package: Name
    save_stalled: SaveStalled
    def __init__(self, connected: _Optional[_Union[Empty, _Mapping]] = ..., connect_failed: _Optional[_Union[NotifyConnectFailed, _Mapping]] = ..., error_response: _Optional[_Union[ErrorResponse, _Mapping]] = ..., remote_config: _Optional[_Union[_opamp_pb2.AgentRemoteConfig, _Mapping]] = ..., command: _Optional[_Union[_opamp_pb2.ServerToAgentCommand, _Mapping]] = ..., instance_uid_changed: _Optional[bytes] = ..., heartbeat_interval_ms: _Optional[int] = ..., status_too_large: _Optional[_Union[StatusTooLarge, _Mapping]] = ..., bad_message: _Optional[_Union[Detail, _Mapping]] = ..., stopped: _Optional[_Union[Empty, _Mapping]] = ..., connection_settings: _Optional[_Union[_opamp_pb2.ConnectionSettingsOffers, _Mapping]] = ..., custom_message: _Optional[_Union[_opamp_pb2.CustomMessage, _Mapping]] = ..., server_custom_capabilities: _Optional[_Union[_opamp_pb2.CustomCapabilities, _Mapping]] = ..., custom_message_not_sent: _Optional[_Union[CustomMessageNotSent, _Mapping]] = ..., install_package: _Optional[_Union[InstallPackage, _Mapping]] = ..., remove_package: _Optional[_Union[Name, _Mapping]] = ..., save_stalled: _Optional[_Union[SaveStalled, _Mapping]] = ...) -> None: ...

class SaveStalled(_message.Message):
    __slots__ = ("stalled", "waited_ms")
    STALLED_FIELD_NUMBER: _ClassVar[int]
    WAITED_MS_FIELD_NUMBER: _ClassVar[int]
    stalled: bool
    waited_ms: int
    def __init__(self, stalled: bool = ..., waited_ms: _Optional[int] = ...) -> None: ...

class InstallPackage(_message.Message):
    __slots__ = ("name", "package", "download")
    NAME_FIELD_NUMBER: _ClassVar[int]
    PACKAGE_FIELD_NUMBER: _ClassVar[int]
    DOWNLOAD_FIELD_NUMBER: _ClassVar[int]
    name: str
    package: _opamp_pb2.PackageAvailable
    download: int
    def __init__(self, name: _Optional[str] = ..., package: _Optional[_Union[_opamp_pb2.PackageAvailable, _Mapping]] = ..., download: _Optional[int] = ...) -> None: ...

class CustomMessageNotSent(_message.Message):
    __slots__ = ("message", "reason")
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    message: _opamp_pb2.CustomMessage
    reason: str
    def __init__(self, message: _Optional[_Union[_opamp_pb2.CustomMessage, _Mapping]] = ..., reason: _Optional[str] = ...) -> None: ...

class NotifyConnectFailed(_message.Message):
    __slots__ = ("status", "detail")
    STATUS_FIELD_NUMBER: _ClassVar[int]
    DETAIL_FIELD_NUMBER: _ClassVar[int]
    status: int
    detail: str
    def __init__(self, status: _Optional[int] = ..., detail: _Optional[str] = ...) -> None: ...

class ErrorResponse(_message.Message):
    __slots__ = ("kind", "message")
    KIND_FIELD_NUMBER: _ClassVar[int]
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    kind: int
    message: str
    def __init__(self, kind: _Optional[int] = ..., message: _Optional[str] = ...) -> None: ...

class StatusTooLarge(_message.Message):
    __slots__ = ("field", "bytes", "name")
    FIELD_FIELD_NUMBER: _ClassVar[int]
    BYTES_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    field: StatusField
    bytes: int
    name: str
    def __init__(self, field: _Optional[_Union[StatusField, str]] = ..., bytes: _Optional[int] = ..., name: _Optional[str] = ...) -> None: ...

class SetupCall(_message.Message):
    __slots__ = ("start", "identity")
    START_FIELD_NUMBER: _ClassVar[int]
    IDENTITY_FIELD_NUMBER: _ClassVar[int]
    start: SetupStart
    identity: SetupIdentity
    def __init__(self, start: _Optional[_Union[SetupStart, _Mapping]] = ..., identity: _Optional[_Union[SetupIdentity, _Mapping]] = ...) -> None: ...

class SetupReply(_message.Message):
    __slots__ = ("started", "identified", "error")
    STARTED_FIELD_NUMBER: _ClassVar[int]
    IDENTIFIED_FIELD_NUMBER: _ClassVar[int]
    ERROR_FIELD_NUMBER: _ClassVar[int]
    started: SetupStarted
    identified: SetupIdentified
    error: str
    def __init__(self, started: _Optional[_Union[SetupStarted, _Mapping]] = ..., identified: _Optional[_Union[SetupIdentified, _Mapping]] = ..., error: _Optional[str] = ...) -> None: ...

class Variable(_message.Message):
    __slots__ = ("name", "value")
    NAME_FIELD_NUMBER: _ClassVar[int]
    VALUE_FIELD_NUMBER: _ClassVar[int]
    name: str
    value: str
    def __init__(self, name: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...

class SetupStart(_message.Message):
    __slots__ = ("env", "os", "program", "cwd", "code")
    ENV_FIELD_NUMBER: _ClassVar[int]
    OS_FIELD_NUMBER: _ClassVar[int]
    PROGRAM_FIELD_NUMBER: _ClassVar[int]
    CWD_FIELD_NUMBER: _ClassVar[int]
    CODE_FIELD_NUMBER: _ClassVar[int]
    env: _containers.RepeatedCompositeFieldContainer[Variable]
    os: SetupOs
    program: str
    cwd: str
    code: SetupInCode
    def __init__(self, env: _Optional[_Iterable[_Union[Variable, _Mapping]]] = ..., os: _Optional[_Union[SetupOs, str]] = ..., program: _Optional[str] = ..., cwd: _Optional[str] = ..., code: _Optional[_Union[SetupInCode, _Mapping]] = ...) -> None: ...

class SetupInCode(_message.Message):
    __slots__ = ("endpoint", "headers_given", "headers", "state_dir", "heartbeat_ms", "endpoint_name", "headers_name", "heartbeat_name")
    ENDPOINT_FIELD_NUMBER: _ClassVar[int]
    HEADERS_GIVEN_FIELD_NUMBER: _ClassVar[int]
    HEADERS_FIELD_NUMBER: _ClassVar[int]
    STATE_DIR_FIELD_NUMBER: _ClassVar[int]
    HEARTBEAT_MS_FIELD_NUMBER: _ClassVar[int]
    ENDPOINT_NAME_FIELD_NUMBER: _ClassVar[int]
    HEADERS_NAME_FIELD_NUMBER: _ClassVar[int]
    HEARTBEAT_NAME_FIELD_NUMBER: _ClassVar[int]
    endpoint: str
    headers_given: bool
    headers: _containers.RepeatedCompositeFieldContainer[Header]
    state_dir: str
    heartbeat_ms: int
    endpoint_name: str
    headers_name: str
    heartbeat_name: str
    def __init__(self, endpoint: _Optional[str] = ..., headers_given: bool = ..., headers: _Optional[_Iterable[_Union[Header, _Mapping]]] = ..., state_dir: _Optional[str] = ..., heartbeat_ms: _Optional[int] = ..., endpoint_name: _Optional[str] = ..., headers_name: _Optional[str] = ..., heartbeat_name: _Optional[str] = ...) -> None: ...

class SetupStarted(_message.Message):
    __slots__ = ("off", "endpoint", "headers", "heartbeat_interval_ms", "state_dir", "default_slots", "all_slots_taken", "service", "shown_endpoint", "hints", "warnings")
    OFF_FIELD_NUMBER: _ClassVar[int]
    ENDPOINT_FIELD_NUMBER: _ClassVar[int]
    HEADERS_FIELD_NUMBER: _ClassVar[int]
    HEARTBEAT_INTERVAL_MS_FIELD_NUMBER: _ClassVar[int]
    STATE_DIR_FIELD_NUMBER: _ClassVar[int]
    DEFAULT_SLOTS_FIELD_NUMBER: _ClassVar[int]
    ALL_SLOTS_TAKEN_FIELD_NUMBER: _ClassVar[int]
    SERVICE_FIELD_NUMBER: _ClassVar[int]
    SHOWN_ENDPOINT_FIELD_NUMBER: _ClassVar[int]
    HINTS_FIELD_NUMBER: _ClassVar[int]
    WARNINGS_FIELD_NUMBER: _ClassVar[int]
    off: bool
    endpoint: str
    headers: _containers.RepeatedCompositeFieldContainer[Header]
    heartbeat_interval_ms: int
    state_dir: str
    default_slots: _containers.RepeatedScalarFieldContainer[str]
    all_slots_taken: str
    service: str
    shown_endpoint: str
    hints: SetupHints
    warnings: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, off: bool = ..., endpoint: _Optional[str] = ..., headers: _Optional[_Iterable[_Union[Header, _Mapping]]] = ..., heartbeat_interval_ms: _Optional[int] = ..., state_dir: _Optional[str] = ..., default_slots: _Optional[_Iterable[str]] = ..., all_slots_taken: _Optional[str] = ..., service: _Optional[str] = ..., shown_endpoint: _Optional[str] = ..., hints: _Optional[_Union[SetupHints, _Mapping]] = ..., warnings: _Optional[_Iterable[str]] = ...) -> None: ...

class SetupHints(_message.Message):
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

class SetupIdentity(_message.Message):
    __slots__ = ("state_dir", "saved", "resource_attributes", "entropy", "unix_ms")
    STATE_DIR_FIELD_NUMBER: _ClassVar[int]
    SAVED_FIELD_NUMBER: _ClassVar[int]
    RESOURCE_ATTRIBUTES_FIELD_NUMBER: _ClassVar[int]
    ENTROPY_FIELD_NUMBER: _ClassVar[int]
    UNIX_MS_FIELD_NUMBER: _ClassVar[int]
    state_dir: bool
    saved: str
    resource_attributes: str
    entropy: bytes
    unix_ms: int
    def __init__(self, state_dir: bool = ..., saved: _Optional[str] = ..., resource_attributes: _Optional[str] = ..., entropy: _Optional[bytes] = ..., unix_ms: _Optional[int] = ...) -> None: ...

class SetupIdentified(_message.Message):
    __slots__ = ("instance_uid", "save", "text", "unsaved_uid", "unsaved_from")
    INSTANCE_UID_FIELD_NUMBER: _ClassVar[int]
    FROM_FIELD_NUMBER: _ClassVar[int]
    SAVE_FIELD_NUMBER: _ClassVar[int]
    TEXT_FIELD_NUMBER: _ClassVar[int]
    UNSAVED_UID_FIELD_NUMBER: _ClassVar[int]
    UNSAVED_FROM_FIELD_NUMBER: _ClassVar[int]
    instance_uid: bytes
    save: bool
    text: str
    unsaved_uid: bytes
    unsaved_from: SetupUidFrom
    def __init__(self, instance_uid: _Optional[bytes] = ..., save: bool = ..., text: _Optional[str] = ..., unsaved_uid: _Optional[bytes] = ..., unsaved_from: _Optional[_Union[SetupUidFrom, str]] = ..., **kwargs) -> None: ...
